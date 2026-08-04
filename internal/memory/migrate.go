package memory

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/safeio"
)

const legacyMemoryClaimVersion = 1
const maxLegacyMemoryMarkerBytes int64 = 64 << 10

var legacyMemoryMigrationReader = safeio.NewReader()
var legacyMemoryMarkerReader = safeio.NewReader()
var legacyMemoryReadTimeout = 5 * time.Second

// ErrLegacyMemoryClaimedByAnotherWorkspace prevents a completed one-time
// claim from being reused by another workspace. Callers may treat this durable,
// expected ownership receipt as non-actionable while still failing closed.
var ErrLegacyMemoryClaimedByAnotherWorkspace = errors.New("legacy memory is claimed by another workspace")

// ErrLegacyMemoryPreviewStale prevents a confirmation from claiming a source
// whose exact contents changed after the user reviewed it.
var ErrLegacyMemoryPreviewStale = errors.New("legacy memory preview is stale")

// LegacyClaimPreview is a read-only inventory and confirmation token for a
// provenance-free memory file. SourceSHA256 intentionally binds confirmation
// to exact bytes, not merely a count.
type LegacyClaimPreview struct {
	Count          int
	Workspace      string
	LegacyPath     string
	ScopedPath     string
	BackupPath     string
	MarkerPath     string
	SourceSHA256   string
	AlreadyClaimed bool
}

// LegacyClaimResult describes an explicit legacy-memory claim. BackupPath,
// TargetBackupPath, and MarkerPath are deterministic so recovery never depends
// on directory scans. TargetBackupPath only exists when an empty scoped store
// had to be replaced during the claim.
type LegacyClaimResult struct {
	Claimed          bool
	AlreadyClaimed   bool
	BackupPath       string
	TargetBackupPath string
	MarkerPath       string
}

type legacyMemoryClaimMarker struct {
	Version      int    `json:"version"`
	Workspace    string `json:"workspace"`
	TargetPath   string `json:"target_path"`
	SourceSHA256 string `json:"source_sha256"`
	Status       string `json:"status"`
}

// ClaimLegacyFileForWorkspace explicitly assigns the pre-workspace global
// memory file to one canonical workspace. It creates an immutable backup and a
// global claim marker before installing the scoped copy. Repeated calls for the
// same workspace are idempotent; a different workspace is rejected.
//
// Call this before NewStore(scopedPath). A pre-existing scoped target without a
// matching pending marker is adopted only when it is valid and contains no
// memories. Its original bytes are backed up before replacement. Non-empty or
// invalid targets are never merged or overwritten.
func ClaimLegacyFileForWorkspace(legacyPath, scopedPath, workspace string) (LegacyClaimResult, error) {
	return claimLegacyFileForWorkspace(legacyPath, scopedPath, workspace, "")
}

func claimLegacyFileForWorkspace(legacyPath, scopedPath, workspace, expectedSourceSHA256 string) (LegacyClaimResult, error) {
	result := LegacyClaimResult{
		BackupPath:       legacyPath + ".pre-workspace.bak",
		TargetBackupPath: scopedPath + ".pre-legacy-claim.bak",
		MarkerPath:       legacyPath + ".workspace-claim.json",
	}
	canonicalWorkspace, err := canonicalClaimWorkspace(workspace)
	if err != nil {
		return result, err
	}
	if legacyPath == "" || scopedPath == "" {
		return result, fmt.Errorf("legacy and scoped memory paths are required")
	}
	legacyPath, err = filepath.Abs(legacyPath)
	if err != nil {
		return result, fmt.Errorf("resolve legacy memory path: %w", err)
	}
	scopedPath, err = filepath.Abs(scopedPath)
	if err != nil {
		return result, fmt.Errorf("resolve scoped memory path: %w", err)
	}
	result.BackupPath = legacyPath + ".pre-workspace.bak"
	result.TargetBackupPath = scopedPath + ".pre-legacy-claim.bak"
	result.MarkerPath = legacyPath + ".workspace-claim.json"
	if filepath.Clean(legacyPath) == filepath.Clean(scopedPath) {
		return result, fmt.Errorf("legacy and scoped memory paths must differ")
	}
	for _, dir := range []string{filepath.Dir(legacyPath), filepath.Dir(scopedPath)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return result, fmt.Errorf("prepare legacy memory claim directory: %w", err)
		}
	}

	lockedResult := result
	err = withLegacyMemoryClaimLocks([]string{legacyPath + ".lock", scopedPath + ".lock"}, func() error {
		var claimErr error
		lockedResult, claimErr = claimResolvedLegacyFileForWorkspace(result, legacyPath, scopedPath, canonicalWorkspace, expectedSourceSHA256)
		return claimErr
	})
	return lockedResult, err
}

func claimResolvedLegacyFileForWorkspace(result LegacyClaimResult, legacyPath, scopedPath, canonicalWorkspace, expectedSourceSHA256 string) (LegacyClaimResult, error) {

	marker, markerExists, err := readLegacyMemoryMarker(result.MarkerPath)
	if err != nil {
		return result, err
	}
	if markerExists {
		if err := validateLegacyMemoryMarkerIdentity(marker, canonicalWorkspace, scopedPath); err != nil {
			return result, err
		}
		if marker.Status == "complete" {
			if _, err := readLegacyMemoryData(scopedPath); err != nil {
				return result, fmt.Errorf("legacy memory claim is complete but scoped target is unavailable: %w", err)
			}
			if err := removeLegacyMemorySourceIfUnchanged(legacyPath, marker.SourceSHA256, nil); err != nil {
				return result, fmt.Errorf("inspect completed legacy memory source: %w", err)
			}
			result.AlreadyClaimed = true
			return result, nil
		}
	}

	source, err := readLegacyMemoryData(legacyPath)
	sourceFromLegacy := err == nil
	var sourceInfo os.FileInfo
	if sourceFromLegacy {
		sourceInfo, err = os.Lstat(legacyPath)
		if err != nil {
			return result, fmt.Errorf("identify legacy memory source: %w", err)
		}
	}
	if errors.Is(err, os.ErrNotExist) && markerExists {
		source, err = readLegacyMemoryData(result.BackupPath)
		sourceFromLegacy = false
	}
	if errors.Is(err, os.ErrNotExist) {
		if markerExists {
			return result, fmt.Errorf("pending legacy memory claim has no source or backup")
		}
		return result, nil
	}
	if err != nil {
		return result, fmt.Errorf("read legacy memory file: %w", err)
	}
	var memories []Memory
	if err := json.Unmarshal(source, &memories); err != nil {
		return result, fmt.Errorf("validate legacy memory file: %w", err)
	}
	digest := sha256.Sum256(source)
	sourceDigest := hex.EncodeToString(digest[:])
	if expectedSourceSHA256 != "" && sourceDigest != expectedSourceSHA256 {
		return result, fmt.Errorf("%w: source contents changed after preview", ErrLegacyMemoryPreviewStale)
	}
	if markerExists {
		if marker.SourceSHA256 != sourceDigest {
			return result, fmt.Errorf("legacy memory changed after its workspace claim was recorded")
		}
	} else {
		target, targetErr := readLegacyMemoryData(scopedPath)
		if targetErr == nil {
			if !isEmptyMemoryFile(target) {
				return result, fmt.Errorf("scoped memory target %q already exists without a legacy claim marker and is not empty", scopedPath)
			}
			if err := ensurePrivateBackup(result.TargetBackupPath, target); err != nil {
				return result, fmt.Errorf("back up empty scoped memory target: %w", err)
			}
		} else if !errors.Is(targetErr, os.ErrNotExist) {
			return result, fmt.Errorf("inspect scoped memory target: %w", targetErr)
		}
		if err := ensurePrivateBackup(result.BackupPath, source); err != nil {
			return result, err
		}
		marker = legacyMemoryClaimMarker{
			Version:      legacyMemoryClaimVersion,
			Workspace:    canonicalWorkspace,
			TargetPath:   scopedPath,
			SourceSHA256: sourceDigest,
			Status:       "pending",
		}
		markerData, err := json.MarshalIndent(marker, "", "  ")
		if err != nil {
			return result, fmt.Errorf("encode legacy memory claim marker: %w", err)
		}
		if err := installPrivateFile(result.MarkerPath, markerData); err != nil {
			if !errors.Is(err, os.ErrExist) {
				return result, fmt.Errorf("create legacy memory claim marker: %w", err)
			}
			marker, markerExists, err = readLegacyMemoryMarker(result.MarkerPath)
			if err != nil || !markerExists {
				return result, fmt.Errorf("read concurrently created legacy memory claim marker: %w", err)
			}
			if err := validateLegacyMemoryMarkerIdentity(marker, canonicalWorkspace, scopedPath); err != nil {
				return result, err
			}
			if marker.SourceSHA256 != sourceDigest {
				return result, fmt.Errorf("legacy memory changed after its workspace claim was recorded")
			}
			if marker.Status == "complete" {
				result.AlreadyClaimed = true
				return result, nil
			}
		}
	}

	if target, err := readLegacyMemoryData(scopedPath); err == nil {
		if bytes.Equal(target, source) {
			// A previous attempt installed the claimed source before it stopped.
		} else if isEmptyMemoryFile(target) {
			if err := ensurePrivateBackup(result.TargetBackupPath, target); err != nil {
				return result, fmt.Errorf("back up empty scoped memory target: %w", err)
			}
			if err := replacePrivateFile(scopedPath, source); err != nil {
				return result, fmt.Errorf("replace empty scoped memory target: %w", err)
			}
		} else {
			return result, fmt.Errorf("pending legacy claim target %q does not match the claimed source", scopedPath)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := installPrivateFile(scopedPath, source); err != nil {
			if !errors.Is(err, os.ErrExist) {
				return result, fmt.Errorf("install scoped memory file: %w", err)
			}
			installed, readErr := readLegacyMemoryData(scopedPath)
			if readErr != nil {
				return result, fmt.Errorf("read concurrently installed scoped memory file: %w", readErr)
			}
			if !bytes.Equal(installed, source) {
				return result, fmt.Errorf("concurrently installed scoped memory file does not match the claimed source")
			}
		}
	} else {
		return result, fmt.Errorf("read pending scoped memory target: %w", err)
	}

	if sourceFromLegacy {
		if err := verifyLegacyMemorySourceUnchanged(legacyPath, sourceDigest, sourceInfo); err != nil {
			return result, err
		}
	}
	marker.Status = "complete"
	markerData, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return result, fmt.Errorf("encode completed legacy memory claim marker: %w", err)
	}
	if err := replacePrivateFile(result.MarkerPath, markerData); err != nil {
		return result, fmt.Errorf("complete legacy memory claim marker: %w", err)
	}
	if err := removeLegacyMemorySourceIfUnchanged(legacyPath, sourceDigest, sourceInfo); err != nil {
		return result, fmt.Errorf("remove migrated legacy memory source: %w", err)
	}
	syncDir(filepath.Dir(legacyPath))
	result.Claimed = true
	return result, nil
}

func isEmptyMemoryFile(data []byte) bool {
	var memories []Memory
	return json.Unmarshal(data, &memories) == nil && len(memories) == 0
}

// ClaimDefaultLegacyForWorkspace claims the historical default global file
// into the deterministic scoped path for workspace.
func ClaimDefaultLegacyForWorkspace(workspace string) (LegacyClaimResult, error) {
	return ClaimLegacyFileForWorkspace(
		DefaultPathForWorkspace(""),
		DefaultPathForWorkspace(workspace),
		workspace,
	)
}

// PreviewDefaultLegacyForWorkspace inventories the historical global memory
// file without creating a marker, backup, or scoped target.
func PreviewDefaultLegacyForWorkspace(workspace string) (LegacyClaimPreview, error) {
	return previewLegacyFileForWorkspace(
		DefaultPathForWorkspace(""),
		DefaultPathForWorkspace(workspace),
		workspace,
	)
}

// ClaimPreviewedDefaultLegacyForWorkspace performs the explicit claim only if
// the destination identity and source bytes still match the user's preview.
func ClaimPreviewedDefaultLegacyForWorkspace(workspace string, preview LegacyClaimPreview) (LegacyClaimResult, error) {
	canonicalWorkspace, err := canonicalClaimWorkspace(workspace)
	if err != nil {
		return LegacyClaimResult{}, err
	}
	legacyPath, err := filepath.Abs(DefaultPathForWorkspace(""))
	if err != nil {
		return LegacyClaimResult{}, fmt.Errorf("resolve legacy memory path: %w", err)
	}
	scopedPath, err := filepath.Abs(DefaultPathForWorkspace(workspace))
	if err != nil {
		return LegacyClaimResult{}, fmt.Errorf("resolve scoped memory path: %w", err)
	}
	if preview.Workspace != canonicalWorkspace || filepath.Clean(preview.LegacyPath) != filepath.Clean(legacyPath) || filepath.Clean(preview.ScopedPath) != filepath.Clean(scopedPath) || preview.SourceSHA256 == "" {
		return LegacyClaimResult{}, fmt.Errorf("%w: destination identity does not match", ErrLegacyMemoryPreviewStale)
	}
	return claimLegacyFileForWorkspace(legacyPath, scopedPath, canonicalWorkspace, preview.SourceSHA256)
}

func previewLegacyFileForWorkspace(legacyPath, scopedPath, workspace string) (LegacyClaimPreview, error) {
	preview := LegacyClaimPreview{}
	canonicalWorkspace, err := canonicalClaimWorkspace(workspace)
	if err != nil {
		return preview, err
	}
	legacyPath, err = filepath.Abs(legacyPath)
	if err != nil {
		return preview, fmt.Errorf("resolve legacy memory path: %w", err)
	}
	scopedPath, err = filepath.Abs(scopedPath)
	if err != nil {
		return preview, fmt.Errorf("resolve scoped memory path: %w", err)
	}
	preview = LegacyClaimPreview{
		Workspace:  canonicalWorkspace,
		LegacyPath: legacyPath,
		ScopedPath: scopedPath,
		BackupPath: legacyPath + ".pre-workspace.bak",
		MarkerPath: legacyPath + ".workspace-claim.json",
	}

	marker, markerExists, err := readLegacyMemoryMarker(preview.MarkerPath)
	if err != nil {
		return preview, err
	}
	if markerExists {
		if err := validateLegacyMemoryMarkerIdentity(marker, canonicalWorkspace, scopedPath); err != nil {
			return preview, err
		}
		if marker.Status == "complete" {
			if _, readErr := readLegacyMemoryData(scopedPath); readErr != nil {
				return preview, fmt.Errorf("legacy memory claim is complete but scoped target is unavailable: %w", readErr)
			}
			preview.AlreadyClaimed = true
			return preview, nil
		}
	}

	source, err := readLegacyMemoryData(legacyPath)
	if errors.Is(err, os.ErrNotExist) && markerExists {
		source, err = readLegacyMemoryData(preview.BackupPath)
	}
	if errors.Is(err, os.ErrNotExist) {
		if markerExists {
			return preview, fmt.Errorf("pending legacy memory claim has no source or backup")
		}
		return preview, nil
	}
	if err != nil {
		return preview, fmt.Errorf("read legacy memory file: %w", err)
	}
	var memories []Memory
	if err := json.Unmarshal(source, &memories); err != nil {
		return preview, fmt.Errorf("validate legacy memory file: %w", err)
	}
	digest := sha256.Sum256(source)
	preview.SourceSHA256 = hex.EncodeToString(digest[:])
	preview.Count = len(memories)
	if markerExists && marker.SourceSHA256 != preview.SourceSHA256 {
		return preview, fmt.Errorf("legacy memory changed after its workspace claim was recorded")
	}

	target, targetErr := readLegacyMemoryData(scopedPath)
	if targetErr == nil {
		if markerExists && bytes.Equal(target, source) {
			return preview, nil
		}
		if !isEmptyMemoryFile(target) {
			return preview, fmt.Errorf("scoped memory target %q is not empty; explicit manual merge required", scopedPath)
		}
	} else if !errors.Is(targetErr, os.ErrNotExist) {
		return preview, fmt.Errorf("inspect scoped memory target: %w", targetErr)
	}
	return preview, nil
}

func canonicalClaimWorkspace(workspace string) (string, error) {
	if workspace == "" {
		return "", fmt.Errorf("workspace identity is required to claim legacy memory")
	}
	absolute, err := filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("resolve claim workspace: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(absolute); resolveErr == nil {
		absolute = resolved
	}
	return filepath.Clean(absolute), nil
}

func readLegacyMemoryMarker(path string) (legacyMemoryClaimMarker, bool, error) {
	if err := safeio.ValidatePublishPath(path); err != nil {
		return legacyMemoryClaimMarker{}, false, fmt.Errorf("validate legacy memory claim marker path: %w", err)
	}
	data, err := legacyMemoryMarkerReader.ReadRegularFileNoFollow(path, maxLegacyMemoryMarkerBytes, legacyMemoryReadTimeout)
	if errors.Is(err, os.ErrNotExist) {
		return legacyMemoryClaimMarker{}, false, nil
	}
	if err != nil {
		return legacyMemoryClaimMarker{}, false, fmt.Errorf("read legacy memory claim marker: %w", err)
	}
	var marker legacyMemoryClaimMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return marker, false, fmt.Errorf("parse legacy memory claim marker: %w", err)
	}
	return marker, true, nil
}

func validateLegacyMemoryMarkerIdentity(marker legacyMemoryClaimMarker, workspace, targetPath string) error {
	if marker.Version != legacyMemoryClaimVersion || (marker.Status != "pending" && marker.Status != "complete") {
		return fmt.Errorf("unsupported legacy memory claim marker")
	}
	if marker.Workspace != workspace {
		return fmt.Errorf("%w %q", ErrLegacyMemoryClaimedByAnotherWorkspace, marker.Workspace)
	}
	if filepath.Clean(marker.TargetPath) != filepath.Clean(targetPath) {
		return fmt.Errorf("legacy memory claim for workspace %q targets %q, not %q", marker.Workspace, marker.TargetPath, targetPath)
	}
	return nil
}

func ensurePrivateBackup(path string, data []byte) error {
	if err := safeio.ValidatePublishPath(path); err != nil {
		return err
	}
	existing, err := legacyMemoryMigrationReader.ReadPrivateRegularFileNoFollow(path, maxMemoryStoreBytes, legacyMemoryReadTimeout)
	if err == nil {
		if !bytes.Equal(existing, data) {
			return fmt.Errorf("legacy memory backup %q does not match the source", path)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read legacy memory backup: %w", err)
	}
	if err := installPrivateFile(path, data); err != nil {
		return fmt.Errorf("create legacy memory backup: %w", err)
	}
	return nil
}

func installPrivateFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := safeio.ValidatePublishPath(path); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".claim-*.tmp")
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
	syncDir(dir)
	return nil
}

func replacePrivateFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := safeio.ValidatePublishPath(path); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".claim-marker-*.tmp")
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
	syncDir(dir)
	return nil
}

func readLegacyMemoryData(path string) ([]byte, error) {
	if err := safeio.ValidatePublishPath(path); err != nil {
		return nil, err
	}
	return legacyMemoryMigrationReader.ReadRegularFileNoFollow(path, maxMemoryStoreBytes, legacyMemoryReadTimeout)
}

func withLegacyMemoryClaimLocks(lockPaths []string, fn func() error) error {
	paths := append([]string(nil), lockPaths...)
	sort.Strings(paths)
	unique := paths[:0]
	for _, path := range paths {
		if len(unique) == 0 || unique[len(unique)-1] != path {
			unique = append(unique, path)
		}
	}
	var acquire func(int) error
	acquire = func(index int) error {
		if index == len(unique) {
			return fn()
		}
		return safeio.WithExclusiveFileLock(unique[index], memoryStoreLockTimeout, func() error {
			return acquire(index + 1)
		})
	}
	return acquire(0)
}

func verifyLegacyMemorySourceUnchanged(path, expectedDigest string, expectedInfo os.FileInfo) error {
	data, err := readLegacyMemoryData(path)
	if err != nil {
		return fmt.Errorf("re-read legacy memory source before removal: %w", err)
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != expectedDigest {
		return fmt.Errorf("legacy memory source changed before removal")
	}
	currentInfo, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("identify legacy memory source before removal: %w", err)
	}
	if expectedInfo != nil && !os.SameFile(expectedInfo, currentInfo) {
		return fmt.Errorf("legacy memory source was replaced before removal")
	}
	return nil
}

func removeLegacyMemorySourceIfUnchanged(path, expectedDigest string, expectedInfo os.FileInfo) error {
	quarantinePath := path + ".workspace-claim.consumed"
	if err := finalizeLegacyMemoryQuarantine(quarantinePath, expectedDigest, expectedInfo); err != nil {
		return err
	}

	// Move first, verify second. A cooperating writer is excluded by the store
	// lock. A legacy/non-cooperating writer that publishes before this rename is
	// preserved at quarantinePath if it differs; one publishing after the rename
	// remains at path and can never be deleted by this claim.
	if err := safeio.ValidatePublishPath(path); err != nil {
		return fmt.Errorf("validate legacy memory source removal path: %w", err)
	}
	if err := os.Rename(path, quarantinePath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("quarantine legacy memory source: %w", err)
	}
	syncDir(filepath.Dir(path))
	return finalizeLegacyMemoryQuarantine(quarantinePath, expectedDigest, expectedInfo)
}

func finalizeLegacyMemoryQuarantine(quarantinePath, expectedDigest string, expectedInfo os.FileInfo) error {
	if err := safeio.ValidatePublishPath(quarantinePath); err != nil {
		return fmt.Errorf("validate legacy memory quarantine path: %w", err)
	}
	data, err := legacyMemoryMigrationReader.ReadPrivateRegularFileNoFollow(quarantinePath, maxMemoryStoreBytes, legacyMemoryReadTimeout)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect legacy memory quarantine %q: %w", quarantinePath, err)
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != expectedDigest {
		return fmt.Errorf("legacy memory source changed; preserved at %q", quarantinePath)
	}
	currentInfo, err := os.Lstat(quarantinePath)
	if err != nil {
		return fmt.Errorf("identify legacy memory quarantine %q: %w", quarantinePath, err)
	}
	if expectedInfo != nil && !os.SameFile(expectedInfo, currentInfo) {
		return fmt.Errorf("legacy memory source was replaced; preserved at %q", quarantinePath)
	}
	if err := os.Remove(quarantinePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove verified legacy memory quarantine: %w", err)
	}
	syncDir(filepath.Dir(quarantinePath))
	return nil
}
