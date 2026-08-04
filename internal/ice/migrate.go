package ice

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/safeio"
)

const legacyICEClaimVersion = 1
const maxLegacyICEClaimMarkerBytes int64 = 64 << 10

var legacyICEClaimMarkerReader = safeio.NewReader()
var legacyICEClaimReadTimeout = 5 * time.Second

var ErrLegacyICEClaimedByAnotherProject = errors.New("legacy ICE entries are claimed by another project")

var ErrLegacyICEPreviewStale = errors.New("legacy ICE preview is stale")

type LegacyEntryClaimPreview struct {
	Count          int
	ProjectID      string
	StorePath      string
	MarkerPath     string
	AlreadyClaimed bool
}

type LegacyEntryClaimResult struct {
	Claimed        int
	AlreadyClaimed bool
	MarkerPath     string
}

type legacyICEClaimMarker struct {
	Version   int    `json:"version"`
	ProjectID string `json:"project_id"`
	Status    string `json:"status"`
}

// ClaimLegacyEntries claims this engine's legacy entries for its canonical
// project identity. It is intentionally explicit so startup can surface the
// migration receipt or error before enabling retrieval.
func (e *Engine) ClaimLegacyEntries() (LegacyEntryClaimResult, error) {
	if e == nil || e.store == nil {
		return LegacyEntryClaimResult{}, fmt.Errorf("ICE engine is unavailable")
	}
	return e.store.ClaimLegacyEntries(e.projectID)
}

// PreviewLegacyEntries inventories provenance-free entries without creating a
// marker or mutating the conversation store.
func (e *Engine) PreviewLegacyEntries() (LegacyEntryClaimPreview, error) {
	if e == nil || e.store == nil {
		return LegacyEntryClaimPreview{}, fmt.Errorf("ICE engine is unavailable")
	}
	return e.store.PreviewLegacyEntries(e.projectID)
}

// ClaimPreviewedLegacyEntries claims only the exact count and destination the
// user reviewed. A changed set must be previewed again.
func (e *Engine) ClaimPreviewedLegacyEntries(preview LegacyEntryClaimPreview) (LegacyEntryClaimResult, error) {
	if e == nil || e.store == nil {
		return LegacyEntryClaimResult{}, fmt.Errorf("ICE engine is unavailable")
	}
	if preview.ProjectID != e.projectID || filepath.Clean(preview.StorePath) != filepath.Clean(e.store.path) || filepath.Clean(preview.MarkerPath) != filepath.Clean(e.store.path+".workspace-claim.json") {
		return LegacyEntryClaimResult{}, fmt.Errorf("%w: destination identity does not match", ErrLegacyICEPreviewStale)
	}
	return e.store.claimLegacyEntries(e.projectID, preview.Count)
}

// ClaimLegacyEntries explicitly assigns entries written before ProjectID was
// introduced to one project. The sidecar marker is installed before mutation,
// making a competing claim by another project fail closed. Repeating the claim
// for the same project is idempotent.
func (s *Store) ClaimLegacyEntries(projectID string) (LegacyEntryClaimResult, error) {
	return s.claimLegacyEntries(projectID, -1)
}

func (s *Store) claimLegacyEntries(projectID string, expectedCount int) (LegacyEntryClaimResult, error) {
	result := LegacyEntryClaimResult{MarkerPath: s.path + ".workspace-claim.json"}
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return result, fmt.Errorf("project identity is required to claim legacy ICE entries")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return result, fmt.Errorf("cannot claim unreadable ICE store: %w", s.loadErr)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return result, fmt.Errorf("prepare ICE claim directory: %w", err)
	}

	err := safeio.WithExclusiveFileLock(s.path+".lock", iceStoreLockTimeout, func() error {
		if err := s.reloadFromDisk(); err != nil {
			return fmt.Errorf("reload ICE store before legacy claim: %w", err)
		}
		marker, markerExists, err := readICEClaimMarker(result.MarkerPath)
		if err != nil {
			return err
		}
		if markerExists {
			if err := validateICEClaimMarker(marker, projectID); err != nil {
				return err
			}
		}

		legacyIndexes := make([]int, 0)
		for i := range s.entries {
			if s.entries[i].ProjectID == "" {
				legacyIndexes = append(legacyIndexes, i)
			}
		}
		if expectedCount >= 0 && len(legacyIndexes) != expectedCount {
			return fmt.Errorf("%w: previewed %d entries, now %d", ErrLegacyICEPreviewStale, expectedCount, len(legacyIndexes))
		}
		if markerExists && marker.Status == "complete" {
			if len(legacyIndexes) != 0 {
				return fmt.Errorf("completed ICE claim contains %d new unscoped entries; refusing implicit adoption", len(legacyIndexes))
			}
			result.AlreadyClaimed = true
			return nil
		}
		if !markerExists && len(legacyIndexes) == 0 {
			return nil
		}
		if !markerExists {
			marker = legacyICEClaimMarker{Version: legacyICEClaimVersion, ProjectID: projectID, Status: "pending"}
			markerData, err := json.MarshalIndent(marker, "", "  ")
			if err != nil {
				return fmt.Errorf("encode ICE claim marker: %w", err)
			}
			if err := installICEClaimMarker(result.MarkerPath, markerData); err != nil {
				if !errors.Is(err, os.ErrExist) {
					return fmt.Errorf("create ICE claim marker: %w", err)
				}
				marker, markerExists, err = readICEClaimMarker(result.MarkerPath)
				if err != nil {
					return err
				}
				if !markerExists {
					return fmt.Errorf("concurrently created ICE claim marker is unavailable")
				}
				if err := validateICEClaimMarker(marker, projectID); err != nil {
					return err
				}
			}
		}

		if len(legacyIndexes) > 0 {
			before, beforeNextID, beforeDirty := s.snapshot()
			for _, index := range legacyIndexes {
				s.entries[index].ProjectID = projectID
			}
			s.dirty = true
			if err := s.persist(); err != nil {
				s.restore(before, beforeNextID, beforeDirty)
				return fmt.Errorf("persist claimed ICE entries: %w", err)
			}
			result.Claimed = len(legacyIndexes)
		}

		marker.Status = "complete"
		markerData, err := json.MarshalIndent(marker, "", "  ")
		if err != nil {
			return fmt.Errorf("encode completed ICE claim marker: %w", err)
		}
		if err := replaceICEClaimMarker(result.MarkerPath, markerData); err != nil {
			return fmt.Errorf("complete ICE claim marker: %w", err)
		}
		return nil
	})
	return result, err
}

func (s *Store) PreviewLegacyEntries(projectID string) (LegacyEntryClaimPreview, error) {
	preview := LegacyEntryClaimPreview{
		ProjectID:  strings.TrimSpace(projectID),
		StorePath:  s.path,
		MarkerPath: s.path + ".workspace-claim.json",
	}
	if preview.ProjectID == "" {
		return preview, fmt.Errorf("project identity is required to preview legacy ICE entries")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return preview, fmt.Errorf("cannot preview unreadable ICE store: %w", s.loadErr)
	}
	if err := s.reloadFromDisk(); err != nil {
		return preview, fmt.Errorf("reload ICE store before legacy preview: %w", err)
	}
	marker, markerExists, err := readICEClaimMarker(preview.MarkerPath)
	if err != nil {
		return preview, err
	}
	if markerExists {
		if err := validateICEClaimMarker(marker, preview.ProjectID); err != nil {
			return preview, err
		}
	}
	for i := range s.entries {
		if s.entries[i].ProjectID == "" {
			preview.Count++
		}
	}
	if markerExists && marker.Status == "pending" && preview.Count == 0 {
		err := safeio.WithExclusiveFileLock(s.path+".lock", iceStoreLockTimeout, func() error {
			if err := s.reloadFromDisk(); err != nil {
				return fmt.Errorf("reload ICE store before claim recovery: %w", err)
			}
			currentMarker, exists, err := readICEClaimMarker(preview.MarkerPath)
			if err != nil {
				return err
			}
			if !exists {
				return fmt.Errorf("pending ICE claim marker disappeared during recovery")
			}
			if err := validateICEClaimMarker(currentMarker, preview.ProjectID); err != nil {
				return err
			}
			preview.Count = 0
			for i := range s.entries {
				if s.entries[i].ProjectID == "" {
					preview.Count++
				}
			}
			if currentMarker.Status == "complete" {
				preview.AlreadyClaimed = preview.Count == 0
				return nil
			}
			if preview.Count != 0 {
				return nil
			}
			currentMarker.Status = "complete"
			markerData, err := json.MarshalIndent(currentMarker, "", "  ")
			if err != nil {
				return fmt.Errorf("encode recovered ICE claim marker: %w", err)
			}
			if err := replaceICEClaimMarker(preview.MarkerPath, markerData); err != nil {
				return fmt.Errorf("complete recovered ICE claim marker: %w", err)
			}
			preview.AlreadyClaimed = true
			return nil
		})
		if err != nil {
			return preview, err
		}
		if preview.AlreadyClaimed {
			return preview, nil
		}
	}
	if markerExists && marker.Status == "complete" {
		if preview.Count != 0 {
			return preview, fmt.Errorf("completed ICE claim contains %d new unscoped entries; refusing implicit adoption", preview.Count)
		}
		preview.AlreadyClaimed = true
	}
	return preview, nil
}

func readICEClaimMarker(path string) (legacyICEClaimMarker, bool, error) {
	if err := safeio.ValidatePublishPath(path); err != nil {
		return legacyICEClaimMarker{}, false, fmt.Errorf("validate ICE claim marker path: %w", err)
	}
	data, err := legacyICEClaimMarkerReader.ReadRegularFileNoFollow(path, maxLegacyICEClaimMarkerBytes, legacyICEClaimReadTimeout)
	if errors.Is(err, os.ErrNotExist) {
		return legacyICEClaimMarker{}, false, nil
	}
	if err != nil {
		return legacyICEClaimMarker{}, false, fmt.Errorf("read ICE claim marker: %w", err)
	}
	var marker legacyICEClaimMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return marker, false, fmt.Errorf("parse ICE claim marker: %w", err)
	}
	return marker, true, nil
}

func validateICEClaimMarker(marker legacyICEClaimMarker, projectID string) error {
	if marker.Version != legacyICEClaimVersion || (marker.Status != "pending" && marker.Status != "complete") {
		return fmt.Errorf("unsupported ICE claim marker")
	}
	if marker.ProjectID != projectID {
		return fmt.Errorf("%w %q", ErrLegacyICEClaimedByAnotherProject, marker.ProjectID)
	}
	return nil
}

func installICEClaimMarker(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := safeio.ValidatePublishPath(path); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".ice-claim-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := safeio.ValidatePublishPath(path); err != nil {
		return err
	}
	if err := os.Link(tmpPath, path); err != nil {
		return err
	}
	syncICEClaimDir(dir)
	return nil
}

func replaceICEClaimMarker(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := safeio.ValidatePublishPath(path); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".ice-claim-marker-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := safeio.ValidatePublishPath(path); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	syncICEClaimDir(dir)
	return nil
}

func syncICEClaimDir(dir string) {
	if directory, err := os.Open(dir); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
}
