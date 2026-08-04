package agent

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/abdul-hamid-achik/sonar/internal/config"
	"golang.org/x/text/unicode/norm"
)

const (
	maxAdditionalReadAuthorities = 16
	readDirectoryBatchSize       = 64
)

// ReadGrantKind distinguishes broad directory authority from one exact file.
// Both are temporary and read-only; neither participates in mutation policy.
type ReadGrantKind string

const (
	ReadGrantDirectory ReadGrantKind = "directory"
	ReadGrantExactFile ReadGrantKind = "exact_file"
)

// ReadGrant is the bounded host projection used by system and UI surfaces.
type ReadGrant struct {
	Path string
	Kind ReadGrantKind
	// identity is an opaque preview-time filesystem identity. It is carried by
	// the TUI from InspectReadPath back to AddInspectedReadGrant and is never
	// rendered, serialized, or accepted from the model/user as data.
	identity *readPathIdentity
}

// ReadPathInspection is a non-mutating, canonical host preflight result.
type ReadPathInspection struct {
	Path            string
	Kind            ReadGrantKind
	External        bool
	AlreadyReadable bool
	identity        *readPathIdentity
	physicalInfo    os.FileInfo
}

type readPathIdentity struct {
	path string
	info os.FileInfo
	pin  *os.File
	once sync.Once
}

// Grant returns the opaque commit value for this exact inspection. Callers may
// display Path and Kind, but only AddInspectedReadGrant can consume identity.
func (inspection *ReadPathInspection) Grant() ReadGrant {
	if inspection == nil {
		return ReadGrant{}
	}
	identity := inspection.identity
	inspection.identity = nil
	return ReadGrant{Path: inspection.Path, Kind: inspection.Kind, identity: identity}
}

// Release closes an uncommitted preview identity. Grant transfers ownership to
// the returned ReadGrant, so releasing the inspection afterwards is harmless.
func (inspection *ReadPathInspection) Release() {
	if inspection == nil || inspection.identity == nil {
		return
	}
	inspection.identity.release()
	inspection.identity = nil
}

// SamePhysicalIdentity reports whether two successful preflight results name
// the same existing filesystem object. It lets bounded host policy merge
// aliases (including hard links and case-insensitive spellings) without
// exposing inode data or performing a second path-based lookup.
func (inspection ReadPathInspection) SamePhysicalIdentity(other ReadPathInspection) bool {
	return inspection.physicalInfo != nil && other.physicalInfo != nil &&
		os.SameFile(inspection.physicalInfo, other.physicalInfo)
}

// Release closes the opaque preview/snapshot identity carried by a grant. It
// is idempotent across value copies of the grant.
func (grant ReadGrant) Release() {
	if grant.identity != nil {
		grant.identity.release()
	}
}

func (identity *readPathIdentity) release() {
	if identity == nil {
		return
	}
	identity.once.Do(func() {
		if identity.pin != nil {
			_ = identity.pin.Close()
		}
	})
}

// additionalReadRoot is an explicit, process-local read grant. os.Root pins the
// selected directory and prevents relative operations (including symlinks) from
// escaping it. Additional roots never participate in mutation authorization.
type additionalReadRoot struct {
	path          string
	root          *os.Root
	info          os.FileInfo
	ignoreContent string
}

// additionalReadFile pins the target's parent directory and original file
// identity. Every open revalidates os.SameFile before returning bytes, so a
// replacement cannot inherit authority granted to the original file.
type additionalReadFile struct {
	path   string
	parent string
	name   string
	root   *os.Root
	pin    *os.File
}

// readablePath carries the authority needed to open one path. Workspace reads
// own a per-operation pinned root; additional grants borrow their process-local
// roots and exact-file pins from Agent state.
type readablePath struct {
	absolute  string
	relative  string
	workspace *workspaceRoot
	root      *additionalReadRoot
	file      *additionalReadFile
}

// pruneStaleReadGrantsLocked removes authorities whose displayed path no
// longer names the pinned filesystem object. Callers hold a.mu for writing and
// the turn boundary, so no provider operation can still be using the handles.
func (a *Agent) pruneStaleReadGrantsLocked() {
	for path, root := range a.readRoots {
		if root.isCurrent() {
			continue
		}
		delete(a.readRoots, path)
		_ = root.root.Close()
	}
	for path, file := range a.readFiles {
		if file.isCurrent() {
			continue
		}
		delete(a.readFiles, path)
		_ = file.pin.Close()
		_ = file.root.Close()
	}
}

func (root *additionalReadRoot) currentInfo() (os.FileInfo, error) {
	if root == nil || root.root == nil || root.info == nil {
		return nil, errors.New("read-only root is unavailable")
	}
	pinned, err := root.root.Stat(".")
	if err != nil {
		return nil, fmt.Errorf("read-only root identity is unavailable: %w", err)
	}
	current, err := os.Stat(root.path)
	if err != nil {
		return nil, err
	}
	if !pinned.IsDir() || !os.SameFile(root.info, pinned) || !os.SameFile(pinned, current) {
		return nil, fmt.Errorf("read-only root changed after authorization: %s", root.path)
	}
	return pinned, nil
}

func (root *additionalReadRoot) isCurrent() bool {
	_, err := root.currentInfo()
	return err == nil
}

func (file *additionalReadFile) isCurrent() bool {
	_, err := file.currentInfo()
	return err == nil
}

// AddReadRoot grants read-only access to one external directory for this Agent
// process. It deliberately rejects overlap with the writable workspace and with
// another grant so ignore and authority boundaries remain unambiguous.
func (a *Agent) AddReadRoot(path string) (string, error) {
	return a.addReadRoot(path, nil)
}

func (a *Agent) addReadRoot(path string, expected *readPathIdentity) (string, error) {
	canonical, err := canonicalReadRootPath(path)
	if err != nil {
		return "", err
	}
	if isFilesystemRoot(canonical) {
		return "", errors.New("refusing to grant the filesystem root as read-only scope")
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("inspect read-only root: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("read-only root is not a directory: %s", canonical)
	}
	if err := validateInspectedReadIdentity(expected, canonical, info); err != nil {
		return "", err
	}

	workspace, err := a.canonicalWorkDir()
	if err != nil {
		return "", err
	}
	overlapsWorkspace, err := physicalPathsOverlap(workspace, canonical)
	if err != nil {
		return "", fmt.Errorf("compare read-only root with writable workspace: %w", err)
	}
	if overlapsWorkspace {
		return "", fmt.Errorf("read-only root %q overlaps writable workspace %q", canonical, workspace)
	}

	ignore, err := config.LoadIgnoreFileWithError(canonical)
	if err != nil {
		return "", fmt.Errorf("load read-only root ignore policy: %w", err)
	}
	root, err := os.OpenRoot(canonical)
	if err != nil {
		return "", fmt.Errorf("open read-only root: %w", err)
	}
	pinned, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return "", fmt.Errorf("inspect opened read-only root: %w", err)
	}
	if !pinned.IsDir() || !os.SameFile(info, pinned) {
		_ = root.Close()
		return "", fmt.Errorf("read-only root changed during authorization: %s", canonical)
	}
	if err := validateInspectedReadIdentity(expected, canonical, pinned); err != nil {
		_ = root.Close()
		return "", err
	}
	grant := &additionalReadRoot{
		path:          canonical,
		root:          root,
		info:          pinned,
		ignoreContent: ignore.EffectiveRaw(),
	}

	// Authority is immutable while a provider turn is active. Check only at the
	// commit boundary so a turn that started during filesystem validation wins.
	a.turnMu.Lock()
	defer a.turnMu.Unlock()
	if a.closed {
		_ = root.Close()
		return "", errors.New("agent is closed")
	}
	if a.turnRunning.Load() {
		_ = root.Close()
		return "", errors.New("read-only scope cannot change during an active agent turn")
	}

	a.mu.Lock()
	a.pruneStaleReadGrantsLocked()
	if existing := a.readRoots[canonical]; existing != nil {
		a.mu.Unlock()
		_ = root.Close()
		return existing.path, nil
	}
	for _, existing := range a.readRoots {
		existingInfo, compareErr := existing.currentInfo()
		if compareErr != nil {
			a.mu.Unlock()
			_ = root.Close()
			return "", fmt.Errorf("compare read-only roots: %w", compareErr)
		}
		if os.SameFile(existingInfo, pinned) {
			a.mu.Unlock()
			_ = root.Close()
			return existing.path, nil
		}
		overlaps, overlapErr := physicalPathsOverlap(existing.path, canonical)
		if overlapErr != nil {
			a.mu.Unlock()
			_ = root.Close()
			return "", fmt.Errorf("compare read-only roots: %w", overlapErr)
		}
		if overlaps {
			a.mu.Unlock()
			_ = root.Close()
			return "", fmt.Errorf("read-only root %q overlaps existing root %q", canonical, existing.path)
		}
	}
	// A newly authorized directory supersedes exact-file grants it contains.
	// Removing those narrower grants keeps the total authority count honest and
	// avoids presenting duplicate scopes for the same path.
	var superseded []*additionalReadFile
	for path, file := range a.readFiles {
		if _, currentErr := file.currentInfo(); currentErr != nil {
			delete(a.readFiles, path)
			_ = file.pin.Close()
			_ = file.root.Close()
			continue
		}
		if _, inside, overlapErr := physicalPathRelative(canonical, path); overlapErr == nil && inside {
			superseded = append(superseded, file)
			delete(a.readFiles, path)
		} else if overlapErr != nil {
			for _, supersededFile := range superseded {
				a.readFiles[supersededFile.path] = supersededFile
			}
			a.mu.Unlock()
			_ = root.Close()
			return "", fmt.Errorf("compare exact read file with read-only root: %w", overlapErr)
		}
	}
	if len(a.readRoots)+len(a.readFiles) >= maxAdditionalReadAuthorities {
		for _, file := range superseded {
			a.readFiles[file.path] = file
		}
		a.mu.Unlock()
		_ = root.Close()
		return "", fmt.Errorf("read-only authority limit reached (%d)", maxAdditionalReadAuthorities)
	}
	if a.readRoots == nil {
		a.readRoots = make(map[string]*additionalReadRoot)
	}
	a.readRoots[canonical] = grant
	a.mu.Unlock()
	for _, file := range superseded {
		_ = file.pin.Close()
		_ = file.root.Close()
	}
	return canonical, nil
}

// AddReadFile grants read-only authority to one exact existing regular file.
// The parent os.Root is an implementation boundary, not granted authority:
// sibling files remain unavailable.
func (a *Agent) AddReadFile(path string) (string, error) {
	return a.addReadFile(path, nil)
}

func (a *Agent) addReadFile(path string, expected *readPathIdentity) (string, error) {
	canonical, info, err := canonicalExistingReadPath(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("exact read grant requires a regular file: %s", canonical)
	}
	if err := validateInspectedReadIdentity(expected, canonical, info); err != nil {
		return "", err
	}
	workspace, err := a.canonicalWorkDir()
	if err != nil {
		return "", err
	}
	if _, inside, relErr := physicalPathRelative(workspace, canonical); relErr != nil {
		return "", fmt.Errorf("compare exact read file with writable workspace: %w", relErr)
	} else if inside {
		return "", fmt.Errorf("exact read file %q is already inside writable workspace %q", canonical, workspace)
	}

	a.mu.RLock()
	if existing := a.readFiles[canonical]; existing != nil && existing.isCurrent() {
		a.mu.RUnlock()
		return existing.path, nil
	}
	for _, existing := range a.readFiles {
		if !existing.isCurrent() {
			continue
		}
		same, compareErr := sameExistingFileEntry(existing.path, canonical)
		if compareErr != nil {
			a.mu.RUnlock()
			return "", fmt.Errorf("compare exact read files: %w", compareErr)
		}
		if same {
			a.mu.RUnlock()
			return existing.path, nil
		}
	}
	for _, root := range a.readRoots {
		if !root.isCurrent() {
			continue
		}
		if _, inside, relErr := physicalPathRelative(root.path, canonical); relErr == nil && inside {
			a.mu.RUnlock()
			return canonical, nil
		} else if relErr != nil {
			a.mu.RUnlock()
			return "", fmt.Errorf("compare exact read file with read-only root: %w", relErr)
		}
	}
	a.mu.RUnlock()

	parent := filepath.Dir(canonical)
	name := filepath.Base(canonical)
	root, err := os.OpenRoot(parent)
	if err != nil {
		return "", fmt.Errorf("open exact read-file parent: %w", err)
	}
	pin, err := root.Open(filepath.ToSlash(name))
	if err != nil {
		_ = root.Close()
		return "", fmt.Errorf("pin exact read file: %w", err)
	}
	pinned, err := pin.Stat()
	if err != nil {
		_ = pin.Close()
		_ = root.Close()
		return "", fmt.Errorf("inspect exact read file: %w", err)
	}
	if !pinned.Mode().IsRegular() || !os.SameFile(info, pinned) {
		_ = pin.Close()
		_ = root.Close()
		return "", fmt.Errorf("exact read file changed during authorization: %s", canonical)
	}
	if err := validateInspectedReadIdentity(expected, canonical, pinned); err != nil {
		_ = pin.Close()
		_ = root.Close()
		return "", err
	}
	grant := &additionalReadFile{path: canonical, parent: parent, name: name, root: root, pin: pin}

	a.turnMu.Lock()
	defer a.turnMu.Unlock()
	if a.closed {
		_ = pin.Close()
		_ = root.Close()
		return "", errors.New("agent is closed")
	}
	if a.turnRunning.Load() {
		_ = pin.Close()
		_ = root.Close()
		return "", errors.New("read-only scope cannot change during an active agent turn")
	}

	a.mu.Lock()
	a.pruneStaleReadGrantsLocked()
	if existing := a.readFiles[canonical]; existing != nil {
		a.mu.Unlock()
		_ = pin.Close()
		_ = root.Close()
		return existing.path, nil
	}
	for _, existing := range a.readFiles {
		same, compareErr := sameExistingFileEntry(existing.path, canonical)
		if compareErr != nil {
			a.mu.Unlock()
			_ = pin.Close()
			_ = root.Close()
			return "", fmt.Errorf("compare exact read files: %w", compareErr)
		}
		if same {
			a.mu.Unlock()
			_ = pin.Close()
			_ = root.Close()
			return existing.path, nil
		}
	}
	for _, directory := range a.readRoots {
		if _, inside, relErr := physicalPathRelative(directory.path, canonical); relErr == nil && inside {
			a.mu.Unlock()
			_ = pin.Close()
			_ = root.Close()
			return canonical, nil
		} else if relErr != nil {
			a.mu.Unlock()
			_ = pin.Close()
			_ = root.Close()
			return "", fmt.Errorf("compare exact read file with read-only root: %w", relErr)
		}
	}
	if len(a.readRoots)+len(a.readFiles) >= maxAdditionalReadAuthorities {
		a.mu.Unlock()
		_ = pin.Close()
		_ = root.Close()
		return "", fmt.Errorf("read-only authority limit reached (%d)", maxAdditionalReadAuthorities)
	}
	if a.readFiles == nil {
		a.readFiles = make(map[string]*additionalReadFile)
	}
	a.readFiles[canonical] = grant
	a.mu.Unlock()
	return canonical, nil
}

// AddInspectedReadGrant commits only the filesystem object observed by the
// corresponding InspectReadPath call. A path that was replaced or retargeted
// while the approval UI was open fails closed instead of inheriting consent.
func (a *Agent) AddInspectedReadGrant(grant ReadGrant) (string, error) {
	defer grant.Release()
	if grant.identity == nil || grant.identity.info == nil {
		return "", errors.New("read grant preview identity is unavailable; inspect the path again")
	}
	switch grant.Kind {
	case ReadGrantDirectory:
		return a.addReadRoot(grant.Path, grant.identity)
	case ReadGrantExactFile:
		return a.addReadFile(grant.Path, grant.identity)
	default:
		return "", fmt.Errorf("unsupported read grant kind %q", grant.Kind)
	}
}

func validateInspectedReadIdentity(expected *readPathIdentity, canonical string, current os.FileInfo) error {
	if expected == nil {
		return nil
	}
	expectedInfo := expected.info
	if expected.pin != nil {
		var err error
		expectedInfo, err = expected.pin.Stat()
		if err != nil {
			return fmt.Errorf("read path changed after approval preview; inspect it again: %s", canonical)
		}
	}
	if expectedInfo == nil || filepath.Clean(expected.path) != filepath.Clean(canonical) ||
		current == nil || !os.SameFile(expectedInfo, current) {
		return fmt.Errorf("read path changed after approval preview; inspect it again: %s", canonical)
	}
	return nil
}

func inspectReadPathIdentity(canonical string, kind ReadGrantKind, expected os.FileInfo) (*readPathIdentity, error) {
	pin, err := os.Open(canonical)
	if err != nil {
		return nil, fmt.Errorf("pin read path identity: %w", err)
	}
	info, err := pin.Stat()
	if err != nil {
		_ = pin.Close()
		return nil, fmt.Errorf("inspect pinned read path identity: %w", err)
	}
	validKind := (kind == ReadGrantExactFile && info.Mode().IsRegular()) ||
		(kind == ReadGrantDirectory && info.IsDir())
	if !validKind || (expected != nil && !os.SameFile(expected, info)) {
		_ = pin.Close()
		return nil, fmt.Errorf("read path changed during inspection: %s", canonical)
	}
	return &readPathIdentity{path: canonical, info: info, pin: pin}, nil
}

// InspectReadPath canonicalizes one existing host path without changing
// authority. It is used by local UI preflight and never enters model or MCP
// contextual-routing input.
func (a *Agent) InspectReadPath(path string) (ReadPathInspection, error) {
	canonical, info, err := canonicalExistingReadPath(path)
	if err != nil {
		return ReadPathInspection{}, err
	}
	kind := ReadGrantDirectory
	if info.Mode().IsRegular() {
		kind = ReadGrantExactFile
	} else if !info.IsDir() {
		return ReadPathInspection{}, fmt.Errorf("read path is neither a regular file nor directory: %s", canonical)
	}
	if kind == ReadGrantDirectory && isFilesystemRoot(canonical) {
		return ReadPathInspection{}, errors.New("refusing to grant the filesystem root as read-only scope")
	}
	workspace, err := a.canonicalWorkDir()
	if err != nil {
		return ReadPathInspection{}, err
	}
	identity, err := inspectReadPathIdentity(canonical, kind, info)
	if err != nil {
		return ReadPathInspection{}, err
	}
	if _, inside, relErr := physicalPathRelative(workspace, canonical); relErr != nil {
		identity.release()
		return ReadPathInspection{}, fmt.Errorf("compare read path with writable workspace: %w", relErr)
	} else if inside {
		identity.release()
		return ReadPathInspection{Path: canonical, Kind: kind, AlreadyReadable: true, physicalInfo: info}, nil
	}
	if kind == ReadGrantDirectory {
		overlaps, overlapErr := physicalPathsOverlap(workspace, canonical)
		if overlapErr != nil {
			identity.release()
			return ReadPathInspection{}, fmt.Errorf("compare read-only root with writable workspace: %w", overlapErr)
		}
		if overlaps {
			identity.release()
			return ReadPathInspection{}, fmt.Errorf("read-only root %q overlaps writable workspace %q", canonical, workspace)
		}
	}

	inspection := ReadPathInspection{Path: canonical, Kind: kind, External: true, identity: identity, physicalInfo: info}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if kind == ReadGrantExactFile {
		if file := a.readFiles[canonical]; file != nil && file.isCurrent() {
			inspection.AlreadyReadable = true
		}
	}
	if kind == ReadGrantExactFile && !inspection.AlreadyReadable {
		for _, file := range a.readFiles {
			if !file.isCurrent() {
				continue
			}
			same, compareErr := sameExistingFileEntry(file.path, canonical)
			if compareErr != nil {
				identity.release()
				return ReadPathInspection{}, fmt.Errorf("compare exact read files: %w", compareErr)
			}
			if same {
				inspection.AlreadyReadable = true
				break
			}
		}
	}
	if !inspection.AlreadyReadable {
		for _, root := range a.readRoots {
			if !root.isCurrent() {
				continue
			}
			if _, inside, relErr := physicalPathRelative(root.path, canonical); relErr == nil && inside {
				inspection.AlreadyReadable = true
				break
			} else if relErr != nil {
				identity.release()
				return ReadPathInspection{}, fmt.Errorf("compare read path with read-only root: %w", relErr)
			}
			if kind == ReadGrantDirectory {
				overlaps, overlapErr := physicalPathsOverlap(root.path, canonical)
				if overlapErr != nil {
					identity.release()
					return ReadPathInspection{}, fmt.Errorf("compare read-only roots: %w", overlapErr)
				}
				if overlaps {
					identity.release()
					return ReadPathInspection{}, fmt.Errorf("read-only root %q overlaps existing root %q", canonical, root.path)
				}
			}
		}
	}
	if inspection.AlreadyReadable {
		identity.release()
		inspection.identity = nil
	}
	return inspection, nil
}

// RemoveReadPath revokes one exact-file or directory grant. It never changes
// the writable workspace or persisted session state.
func (a *Agent) RemoveReadPath(path string) (ReadGrant, error) {
	requested, err := absoluteCleanReadPath(path)
	if err != nil {
		return ReadGrant{}, err
	}
	canonical := requested
	if resolved, resolveErr := filepath.EvalSymlinks(requested); resolveErr == nil {
		canonical = filepath.Clean(resolved)
	}

	a.turnMu.Lock()
	defer a.turnMu.Unlock()
	if a.closed {
		return ReadGrant{}, errors.New("agent is closed")
	}
	if a.turnRunning.Load() {
		return ReadGrant{}, errors.New("read-only scope cannot change during an active agent turn")
	}

	a.mu.Lock()
	directory := a.readRoots[canonical]
	file := a.readFiles[canonical]
	if directory == nil && file == nil && canonical != requested {
		directory = a.readRoots[requested]
		file = a.readFiles[requested]
		canonical = requested
	}
	if directory == nil && file == nil {
		for path, existing := range a.readRoots {
			if same, compareErr := sameExistingPath(existing.path, canonical); compareErr == nil && same {
				directory = existing
				canonical = path
				break
			}
		}
	}
	if directory == nil && file == nil {
		for path, existing := range a.readFiles {
			if same, compareErr := sameExistingFileEntry(existing.path, canonical); compareErr == nil && same {
				file = existing
				canonical = path
				break
			}
		}
	}
	if directory != nil {
		delete(a.readRoots, canonical)
	} else if file != nil {
		delete(a.readFiles, canonical)
	}
	a.mu.Unlock()
	if directory == nil && file == nil {
		return ReadGrant{}, fmt.Errorf("read-only path is not active: %s", requested)
	}
	if directory != nil {
		result := ReadGrant{Path: directory.path, Kind: ReadGrantDirectory}
		if err := directory.root.Close(); err != nil {
			return result, fmt.Errorf("close read-only root: %w", err)
		}
		return result, nil
	}
	result := ReadGrant{Path: file.path, Kind: ReadGrantExactFile}
	if err := errors.Join(file.pin.Close(), file.root.Close()); err != nil {
		return result, fmt.Errorf("close exact read file: %w", err)
	}
	return result, nil
}

// RemoveReadRoot is the source-compatible string projection of RemoveReadPath.
func (a *Agent) RemoveReadRoot(path string) (string, error) {
	grant, err := a.RemoveReadPath(path)
	return grant.Path, err
}

// ClearReadRoots revokes every temporary directory and exact-file read grant.
func (a *Agent) ClearReadRoots() (int, error) {
	a.turnMu.Lock()
	defer a.turnMu.Unlock()
	if a.closed {
		return 0, errors.New("agent is closed")
	}
	if a.turnRunning.Load() {
		return 0, errors.New("read-only scope cannot change during an active agent turn")
	}

	a.mu.Lock()
	roots := make([]*additionalReadRoot, 0, len(a.readRoots))
	for _, root := range a.readRoots {
		roots = append(roots, root)
	}
	files := make([]*additionalReadFile, 0, len(a.readFiles))
	for _, file := range a.readFiles {
		files = append(files, file)
	}
	a.readRoots = make(map[string]*additionalReadRoot)
	a.readFiles = make(map[string]*additionalReadFile)
	a.mu.Unlock()
	var closeErr error
	for _, root := range roots {
		closeErr = errors.Join(closeErr, root.root.Close())
	}
	for _, file := range files {
		closeErr = errors.Join(closeErr, file.pin.Close(), file.root.Close())
	}
	if closeErr != nil {
		return len(roots) + len(files), fmt.Errorf("close read-only grants: %w", closeErr)
	}
	return len(roots) + len(files), nil
}

// ReadRoots returns a stable, sorted copy suitable for prompt and UI display.
func (a *Agent) ReadRoots() []string {
	a.mu.RLock()
	roots := make([]string, 0, len(a.readRoots))
	for path := range a.readRoots {
		roots = append(roots, path)
	}
	a.mu.RUnlock()
	sort.Strings(roots)
	return roots
}

// ReadGrants returns a stable typed copy for host and system presentation.
func (a *Agent) ReadGrants() []ReadGrant {
	a.mu.RLock()
	grants := make([]ReadGrant, 0, len(a.readRoots)+len(a.readFiles))
	for path := range a.readRoots {
		grants = append(grants, ReadGrant{Path: path, Kind: ReadGrantDirectory})
	}
	for path := range a.readFiles {
		grants = append(grants, ReadGrant{Path: path, Kind: ReadGrantExactFile})
	}
	a.mu.RUnlock()
	sort.Slice(grants, func(i, j int) bool {
		if grants[i].Path != grants[j].Path {
			return grants[i].Path < grants[j].Path
		}
		return grants[i].Kind < grants[j].Kind
	})
	return grants
}

// SnapshotReadGrants returns restorable grants with independently pinned
// filesystem identities. Unlike ReadGrants, it performs host I/O and every
// returned grant must be released by the caller (AddInspectedReadGrant consumes
// a grant automatically). This keeps ordinary prompt/TUI rendering pin-free.
func (a *Agent) SnapshotReadGrants() ([]ReadGrant, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	grants := make([]ReadGrant, 0, len(a.readRoots)+len(a.readFiles))
	release := func() {
		for _, grant := range grants {
			grant.Release()
		}
	}
	for path, root := range a.readRoots {
		identity, err := inspectReadPathIdentity(path, ReadGrantDirectory, root.info)
		if err != nil {
			release()
			return nil, fmt.Errorf("snapshot read-only root %q: %w", path, err)
		}
		grants = append(grants, ReadGrant{Path: path, Kind: ReadGrantDirectory, identity: identity})
	}
	for path, file := range a.readFiles {
		pinned, err := file.pin.Stat()
		if err != nil {
			release()
			return nil, fmt.Errorf("snapshot exact read file %q: %w", path, err)
		}
		identity, err := inspectReadPathIdentity(path, ReadGrantExactFile, pinned)
		if err != nil {
			release()
			return nil, fmt.Errorf("snapshot exact read file %q: %w", path, err)
		}
		grants = append(grants, ReadGrant{Path: path, Kind: ReadGrantExactFile, identity: identity})
	}
	sort.Slice(grants, func(i, j int) bool {
		if grants[i].Path != grants[j].Path {
			return grants[i].Path < grants[j].Path
		}
		return grants[i].Kind < grants[j].Kind
	})
	return grants, nil
}

func (a *Agent) closeReadRoots() {
	a.mu.Lock()
	roots := make([]*additionalReadRoot, 0, len(a.readRoots))
	for _, root := range a.readRoots {
		roots = append(roots, root)
	}
	files := make([]*additionalReadFile, 0, len(a.readFiles))
	for _, file := range a.readFiles {
		files = append(files, file)
	}
	a.readRoots = nil
	a.readFiles = nil
	a.mu.Unlock()
	for _, root := range roots {
		_ = root.root.Close()
	}
	for _, file := range files {
		_ = file.pin.Close()
		_ = file.root.Close()
	}
}

func (a *Agent) canonicalWorkDir() (string, error) {
	workDir := a.activeWorkDir()
	if workDir == "" {
		var err error
		workDir, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve workspace: %w", err)
		}
	}
	workDir, err := filepath.Abs(workDir)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(workDir); resolveErr == nil {
		workDir = resolved
	}
	return filepath.Clean(workDir), nil
}

func canonicalReadRootPath(path string) (string, error) {
	path, err := absoluteCleanReadPath(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve read-only root %q: %w", path, err)
	}
	return filepath.Clean(resolved), nil
}

func canonicalExistingReadPath(path string) (string, os.FileInfo, error) {
	absolute, err := absoluteCleanReadPath(path)
	if err != nil {
		return "", nil, err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", nil, fmt.Errorf("resolve read path %q: %w", absolute, err)
	}
	canonical = filepath.Clean(canonical)
	info, err := os.Stat(canonical)
	if err != nil {
		return "", nil, fmt.Errorf("inspect read path: %w", err)
	}
	return canonical, info, nil
}

func absoluteCleanReadPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("read-only root path is empty")
	}
	if path == "~" || strings.HasPrefix(path, "~"+string(filepath.Separator)) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		if path == "~" {
			path = home
		} else {
			path = filepath.Join(home, strings.TrimPrefix(path, "~"+string(filepath.Separator)))
		}
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve read-only root %q: %w", path, err)
	}
	return filepath.Clean(absolute), nil
}

func isFilesystemRoot(path string) bool {
	clean := filepath.Clean(path)
	return filepath.IsAbs(clean) && filepath.Dir(clean) == clean
}

// physicalPathRelative reports containment using filesystem identity instead
// of path spelling. This matters on case-insensitive or normalization-aware
// filesystems, where two different absolute strings can name the same directory
// hierarchy. Missing final components are retained after the closest existing
// ancestor so callers can also classify a read path that disappeared mid-check.
func physicalPathRelative(root, candidate string) (string, bool, error) {
	root = filepath.Clean(root)
	candidate = filepath.Clean(candidate)
	rootInfo, err := os.Stat(root)
	if err != nil {
		return "", false, fmt.Errorf("inspect containment root %q: %w", root, err)
	}
	if !rootInfo.IsDir() {
		return "", false, fmt.Errorf("containment root is not a directory: %s", root)
	}

	current := candidate
	components := make([]string, 0, 8)
	for {
		info, statErr := os.Stat(current)
		if statErr == nil {
			if info.IsDir() && os.SameFile(rootInfo, info) {
				for left, right := 0, len(components)-1; left < right; left, right = left+1, right-1 {
					components[left], components[right] = components[right], components[left]
				}
				if len(components) == 0 {
					return ".", true, nil
				}
				return filepath.Join(components...), true, nil
			}
		} else if !os.IsNotExist(statErr) {
			return "", false, fmt.Errorf("inspect containment candidate %q: %w", current, statErr)
		}

		parent := filepath.Dir(current)
		if parent == current {
			return "", false, nil
		}
		components = append(components, filepath.Base(current))
		current = parent
	}
}

// physicalCanonicalRelative additionally recovers the directory-entry spelling
// for every existing component. Ignore rules are string based, so applying them
// to an alternate casing or normalization would otherwise let a physical alias
// of an ignored object evade policy on filesystems that accept that alias.
func physicalCanonicalRelative(root, candidate string) (string, bool, error) {
	relative, inside, err := physicalPathRelative(root, candidate)
	if err != nil || !inside || relative == "." {
		return relative, inside, err
	}

	components := strings.Split(filepath.Clean(relative), string(filepath.Separator))
	canonical := make([]string, 0, len(components))
	current := filepath.Clean(root)
	for index, component := range components {
		requested := filepath.Join(current, component)
		info, statErr := os.Stat(requested)
		if os.IsNotExist(statErr) {
			canonical = append(canonical, components[index:]...)
			return filepath.Join(canonical...), true, nil
		}
		if statErr != nil {
			return "", false, fmt.Errorf("inspect contained path %q: %w", requested, statErr)
		}

		actual, nameErr := physicalEntryName(current, component, info)
		if nameErr != nil {
			return "", false, fmt.Errorf("resolve physical spelling for %q: %w", requested, nameErr)
		}
		canonical = append(canonical, actual)
		current = filepath.Join(current, actual)
	}
	return filepath.Join(canonical...), true, nil
}

func physicalEntryName(directory, requested string, expected os.FileInfo) (string, error) {
	opened, err := os.Open(directory)
	if err != nil {
		return "", err
	}
	defer func() { _ = opened.Close() }()

	var (
		matchingName  string
		matchingCount int
	)
	normalizedRequested := norm.NFC.String(requested)
	for {
		entries, readErr := opened.ReadDir(readDirectoryBatchSize)
		for _, entry := range entries {
			if entry.Name() == requested {
				info, infoErr := entry.Info()
				if infoErr != nil {
					return "", infoErr
				}
				if !os.SameFile(expected, info) {
					return "", errors.New("directory entry changed during physical resolution")
				}
				return requested, nil
			}
			// Identity checks may require a syscall for every entry. Most aliases
			// accepted by desktop filesystems differ only by case or Unicode
			// normalization, so filter by that inexpensive spelling relation first.
			if !strings.EqualFold(norm.NFC.String(entry.Name()), normalizedRequested) {
				continue
			}
			info, infoErr := entry.Info()
			if infoErr != nil || !os.SameFile(expected, info) {
				continue
			}
			matchingName = entry.Name()
			matchingCount++
		}
		switch {
		case errors.Is(readErr, io.EOF):
			if matchingCount == 1 {
				return matchingName, nil
			}
			if matchingCount > 1 {
				return "", errors.New("directory entry identity is ambiguous")
			}
			// Preserve compatibility with filesystem-specific alias rules that are
			// neither Unicode normalization nor case folding. This slower fallback
			// is exceptional; the common path avoids Info calls for unrelated names.
			return physicalEntryNameByIdentity(directory, expected)
		case readErr != nil:
			return "", readErr
		case len(entries) == 0:
			return "", io.ErrNoProgress
		}
	}
}

func physicalEntryNameByIdentity(directory string, expected os.FileInfo) (string, error) {
	opened, err := os.Open(directory)
	if err != nil {
		return "", err
	}
	defer func() { _ = opened.Close() }()

	var (
		identicalName  string
		identicalCount int
	)
	for {
		entries, readErr := opened.ReadDir(readDirectoryBatchSize)
		for _, entry := range entries {
			info, infoErr := entry.Info()
			if infoErr != nil || !os.SameFile(expected, info) {
				continue
			}
			identicalName = entry.Name()
			identicalCount++
		}
		switch {
		case errors.Is(readErr, io.EOF):
			if identicalCount == 1 {
				return identicalName, nil
			}
			if identicalCount == 0 {
				return "", errors.New("directory entry identity is unavailable")
			}
			return "", errors.New("directory entry identity is ambiguous")
		case readErr != nil:
			return "", readErr
		case len(entries) == 0:
			return "", io.ErrNoProgress
		}
	}
}

func physicalPathsOverlap(first, second string) (bool, error) {
	if _, inside, err := physicalPathRelative(first, second); err != nil {
		return false, err
	} else if inside {
		return true, nil
	}
	_, inside, err := physicalPathRelative(second, first)
	return inside, err
}

func sameExistingPath(first, second string) (bool, error) {
	firstInfo, err := os.Stat(first)
	if err != nil {
		return false, err
	}
	secondInfo, err := os.Stat(second)
	if err != nil {
		return false, err
	}
	return os.SameFile(firstInfo, secondInfo), nil
}

// sameExistingFileEntry distinguishes filesystem aliases for one directory
// entry from separate hard links to the same inode. Exact-file authority is
// path-entry scoped: alternate casing or Unicode normalization may identify
// the approved entry, but a sibling hard link must be approved separately.
func sameExistingFileEntry(first, second string) (bool, error) {
	first = filepath.Clean(first)
	second = filepath.Clean(second)
	firstParent := filepath.Dir(first)
	secondParent := filepath.Dir(second)
	parentsMatch, err := sameExistingPath(firstParent, secondParent)
	if err != nil || !parentsMatch {
		return false, err
	}

	firstInfo, err := os.Stat(first)
	if err != nil {
		return false, err
	}
	secondInfo, err := os.Stat(second)
	if err != nil {
		return false, err
	}
	if !os.SameFile(firstInfo, secondInfo) {
		return false, nil
	}
	firstName, err := physicalEntryName(firstParent, filepath.Base(first), firstInfo)
	if err != nil {
		return false, err
	}
	secondName, err := physicalEntryName(secondParent, filepath.Base(second), secondInfo)
	if err != nil {
		return false, err
	}
	return firstName == secondName, nil
}

func (a *Agent) resolveReadablePath(path string) (readablePath, error) {
	requested := path
	if path == "~" || strings.HasPrefix(path, "~"+string(filepath.Separator)) {
		var err error
		path, err = absoluteCleanReadPath(path)
		if err != nil {
			return readablePath{}, err
		}
	}
	pinnedWorkspace, err := a.openWorkspaceRoot()
	if err != nil {
		return readablePath{}, err
	}
	resolved, workspaceErr := a.resolveWorkspacePath(path)
	if workspaceErr == nil {
		relative, relativeErr := pinnedWorkspace.relative(resolved)
		if relativeErr != nil {
			_ = pinnedWorkspace.Close()
			return readablePath{}, relativeErr
		}
		return readablePath{absolute: resolved, relative: relative, workspace: pinnedWorkspace}, nil
	}
	_ = pinnedWorkspace.Close()

	// A temporary write authority necessarily includes read access to the same
	// exact scope so edit previews, grep/read tools, and post-write verification
	// can use the same pinned boundary. This does not make sibling paths under an
	// exact-file grant readable.
	if _, writeErr := a.resolveAdditionalWritePath(path); writeErr == nil {
		writable, absolute, relative, openErr := a.openWritableRootForPath(path)
		if openErr != nil {
			return readablePath{}, openErr
		}
		return readablePath{absolute: absolute, relative: relative, workspace: writable}, nil
	}

	workspace, err := a.canonicalWorkDir()
	if err != nil {
		return readablePath{}, err
	}
	candidate := path
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(workspace, candidate)
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil {
		return readablePath{}, fmt.Errorf("resolve readable path %q: %w", path, err)
	}
	candidate = filepath.Clean(candidate)
	candidate, err = resolveExistingAncestor(candidate)
	if err != nil {
		return readablePath{}, fmt.Errorf("resolve readable path %q: %w", path, err)
	}
	if _, insideWorkspace, relErr := physicalCanonicalRelative(workspace, candidate); relErr != nil {
		return readablePath{}, fmt.Errorf("compare readable path with writable workspace: %w", relErr)
	} else if insideWorkspace {
		// Never let an additional root bypass a workspace .agentignore or a
		// workspace symlink-escape rejection. Identity-based containment also
		// catches casing and normalization aliases on filesystems that accept
		// more than one spelling for the same object.
		return readablePath{}, workspaceErr
	}

	a.mu.RLock()
	exact := a.readFiles[candidate]
	var staleExactErr error
	if exact != nil {
		if _, currentErr := exact.currentInfo(); currentErr != nil {
			staleExactErr = currentErr
			exact = nil
		}
	}
	var grant *additionalReadRoot
	var relative string
	if exact == nil {
		if _, statErr := os.Stat(candidate); statErr == nil {
			for _, file := range a.readFiles {
				if !file.isCurrent() {
					continue
				}
				same, compareErr := sameExistingFileEntry(file.path, candidate)
				if compareErr == nil && same {
					exact = file
					break
				}
			}
		}
	}
	if exact == nil {
		for _, root := range a.readRoots {
			if !root.isCurrent() {
				continue
			}
			rel, inside, relErr := physicalCanonicalRelative(root.path, candidate)
			if relErr == nil && inside {
				grant = root
				relative = rel
				break
			}
		}
	}
	a.mu.RUnlock()
	if exact != nil {
		return readablePath{absolute: exact.path, file: exact}, nil
	}
	if grant == nil {
		if staleExactErr != nil {
			return readablePath{}, staleExactErr
		}
		return readablePath{}, fmt.Errorf("path %q is outside the writable workspace and active temporary read grants; authorize the exact file when prompted or use /scope add-read <directory>", requested)
	}
	if pathIgnoredWithContent(grant.ignoreContent, relative) {
		return readablePath{}, fmt.Errorf("path %q is excluded by %s/.agentignore", requested, grant.path)
	}
	return readablePath{absolute: filepath.Join(grant.path, relative), relative: relative, root: grant}, nil
}

func (path readablePath) close() error {
	if path.workspace == nil {
		return nil
	}
	return path.workspace.Close()
}

func (path readablePath) stat() (os.FileInfo, error) {
	if path.file != nil {
		return path.file.currentInfo()
	}
	if path.workspace != nil {
		return path.workspace.root.Stat(filepath.ToSlash(path.relative))
	}
	if path.root == nil {
		return nil, errors.New("read authority is unavailable")
	}
	return path.root.root.Stat(filepath.ToSlash(path.relative))
}

// readDirBounded returns the same lexicographically-first visible entries as a
// full os.ReadDir followed by a limit, while keeping memory bounded by that
// limit. External roots must not materialize an unbounded host directory before
// ls applies its result cap. The boolean reports whether the directory had any
// entries before ignore filtering, preserving the existing empty-directory
// distinction.
func (path readablePath) readDirBounded(ctx context.Context, limit int, include func(os.DirEntry) bool) ([]os.DirEntry, bool, error) {
	if path.file != nil {
		return nil, false, fmt.Errorf("exact read grant is not a directory: %s", path.absolute)
	}
	if limit <= 0 {
		return nil, false, errors.New("directory result limit must be positive")
	}

	var directory *os.File
	var err error
	if path.workspace != nil {
		directory, err = path.workspace.root.Open(filepath.ToSlash(path.relative))
	} else if path.root == nil {
		return nil, false, errors.New("read authority is unavailable")
	} else {
		directory, err = path.root.root.Open(filepath.ToSlash(path.relative))
	}
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = directory.Close() }()

	selected := make(maxNameDirEntryHeap, 0, min(limit, readDirectoryBatchSize))
	hadEntries := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, hadEntries, err
		}
		batch, readErr := directory.ReadDir(readDirectoryBatchSize)
		if len(batch) > 0 {
			hadEntries = true
		}
		for _, entry := range batch {
			if err := ctx.Err(); err != nil {
				return nil, hadEntries, err
			}
			if include != nil && !include(entry) {
				continue
			}
			if len(selected) < limit {
				heap.Push(&selected, entry)
				continue
			}
			if entry.Name() < selected[0].Name() {
				selected[0] = entry
				heap.Fix(&selected, 0)
			}
		}
		switch {
		case errors.Is(readErr, io.EOF):
			sort.Slice(selected, func(i, j int) bool { return selected[i].Name() < selected[j].Name() })
			return []os.DirEntry(selected), hadEntries, nil
		case readErr != nil:
			return nil, hadEntries, readErr
		case len(batch) == 0:
			return nil, hadEntries, io.ErrNoProgress
		}
	}
}

// maxNameDirEntryHeap keeps the lexicographically-largest selected name at the
// root so a streaming scan retains only the first N names.
type maxNameDirEntryHeap []os.DirEntry

func (entries maxNameDirEntryHeap) Len() int           { return len(entries) }
func (entries maxNameDirEntryHeap) Less(i, j int) bool { return entries[i].Name() > entries[j].Name() }
func (entries maxNameDirEntryHeap) Swap(i, j int)      { entries[i], entries[j] = entries[j], entries[i] }
func (entries *maxNameDirEntryHeap) Push(value any)    { *entries = append(*entries, value.(os.DirEntry)) }
func (entries *maxNameDirEntryHeap) Pop() any {
	old := *entries
	last := old[len(old)-1]
	*entries = old[:len(old)-1]
	return last
}

func (path readablePath) readBounded(limit int64) ([]byte, error) {
	return path.readBoundedAt(path.absolute, limit)
}

func (path readablePath) readBoundedAt(absolute string, limit int64) ([]byte, error) {
	if path.file != nil {
		if filepath.Clean(absolute) != path.file.path {
			return nil, fmt.Errorf("path escapes exact read-file grant: %s", absolute)
		}
		file, err := path.file.openVerified()
		if err != nil {
			return nil, err
		}
		return readBoundedOpenFile(file, limit)
	}
	if path.workspace != nil {
		relative, inside, err := workspaceRelative(path.workspace.path, absolute)
		if err != nil || !inside {
			return nil, fmt.Errorf("path escapes workspace root: %s", absolute)
		}
		rootRelative := filepath.ToSlash(relative)
		info, err := path.workspace.root.Stat(rootRelative)
		if err != nil {
			return nil, err
		}
		if err := validateBoundedFileInfo(info, limit); err != nil {
			return nil, err
		}
		file, err := path.workspace.root.Open(rootRelative)
		if err != nil {
			return nil, err
		}
		return readBoundedOpenFile(file, limit)
	}
	if path.root == nil {
		return nil, errors.New("read authority is unavailable")
	}
	relative, inside, err := workspaceRelative(path.root.path, absolute)
	if err != nil || !inside {
		relative, inside, err = physicalCanonicalRelative(path.root.path, absolute)
	}
	if err != nil || !inside {
		return nil, fmt.Errorf("path escapes read-only root: %s", absolute)
	}
	rootRelative := filepath.ToSlash(relative)
	info, err := path.root.root.Stat(rootRelative)
	if err != nil {
		return nil, err
	}
	if err := validateBoundedFileInfo(info, limit); err != nil {
		return nil, err
	}
	file, err := path.root.root.Open(rootRelative)
	if err != nil {
		return nil, err
	}
	return readBoundedOpenFile(file, limit)
}

func (path readablePath) ignored(a *Agent, absolute string) bool {
	if path.file != nil {
		return filepath.Clean(absolute) != path.file.path
	}
	if path.workspace != nil {
		relative, inside, err := workspaceRelative(path.workspace.path, absolute)
		return err != nil || !inside || a.pathIgnored(relative)
	}
	if path.root == nil {
		return true
	}
	relative, inside, err := workspaceRelative(path.root.path, absolute)
	if err != nil || !inside {
		relative, inside, err = physicalCanonicalRelative(path.root.path, absolute)
	}
	return err != nil || !inside || pathIgnoredWithContent(path.root.ignoreContent, relative)
}

func (path readablePath) walk(fn filepath.WalkFunc) error {
	if path.file != nil {
		info, err := path.file.currentInfo()
		return fn(path.file.path, info, err)
	}
	if path.workspace != nil {
		start := filepath.ToSlash(path.relative)
		return fs.WalkDir(path.workspace.root.FS(), start, func(relative string, entry fs.DirEntry, walkErr error) error {
			absolute := filepath.Join(path.workspace.path, filepath.FromSlash(relative))
			if walkErr != nil {
				return fn(absolute, nil, walkErr)
			}
			info, err := entry.Info()
			if err != nil {
				return fn(absolute, nil, err)
			}
			return fn(absolute, info, nil)
		})
	}
	if path.root == nil {
		return errors.New("read authority is unavailable")
	}
	start := filepath.ToSlash(path.relative)
	return fs.WalkDir(path.root.root.FS(), start, func(relative string, entry fs.DirEntry, walkErr error) error {
		absolute := filepath.Join(path.root.path, filepath.FromSlash(relative))
		if walkErr != nil {
			return fn(absolute, nil, walkErr)
		}
		info, err := entry.Info()
		if err != nil {
			return fn(absolute, nil, err)
		}
		return fn(absolute, info, nil)
	})
}

func (file *additionalReadFile) currentInfo() (os.FileInfo, error) {
	if file == nil || file.root == nil || file.pin == nil {
		return nil, errors.New("exact read file is unavailable")
	}
	pinned, err := file.pin.Stat()
	if err != nil {
		return nil, fmt.Errorf("exact read file identity is unavailable: %w", err)
	}
	current, err := file.root.Stat(filepath.ToSlash(file.name))
	if err != nil {
		return nil, err
	}
	pathInfo, err := os.Stat(file.path)
	if err != nil {
		return nil, err
	}
	if !current.Mode().IsRegular() || !os.SameFile(pinned, current) || !os.SameFile(pinned, pathInfo) {
		return nil, fmt.Errorf("exact read file changed after authorization: %s", file.path)
	}
	return current, nil
}

func (file *additionalReadFile) openVerified() (*os.File, error) {
	if _, err := file.currentInfo(); err != nil {
		return nil, err
	}
	opened, err := file.root.Open(filepath.ToSlash(file.name))
	if err != nil {
		return nil, err
	}
	info, err := opened.Stat()
	if err != nil {
		_ = opened.Close()
		return nil, err
	}
	pinned, pinErr := file.pin.Stat()
	if pinErr != nil || !info.Mode().IsRegular() || !os.SameFile(pinned, info) {
		_ = opened.Close()
		return nil, fmt.Errorf("exact read file changed after authorization: %s", file.path)
	}
	return opened, nil
}

func readBoundedOpenFile(file *os.File, limit int64) ([]byte, error) {
	if file == nil {
		return nil, errors.New("file is unavailable")
	}
	defer func() { _ = file.Close() }()
	if limit <= 0 {
		return nil, fmt.Errorf("invalid read limit %d", limit)
	}
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if err := validateBoundedFileInfo(info, limit); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file grew beyond %d-byte limit while reading", limit)
	}
	return data, nil
}

func validateBoundedFileInfo(info os.FileInfo, limit int64) error {
	if limit <= 0 {
		return fmt.Errorf("invalid read limit %d", limit)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("path is not a regular file (%s)", info.Mode().Type())
	}
	if info.Size() > limit {
		return fmt.Errorf("file is %d bytes; limit is %d bytes", info.Size(), limit)
	}
	return nil
}

func pathIgnoredWithContent(ignoreContent, path string) bool {
	workspacePolicy, hasHostDefaults := config.IgnorePolicyLayers(ignoreContent)
	if hasHostDefaults && config.HostSecretPathIgnored(path) {
		return true
	}
	if strings.TrimSpace(workspacePolicy) == "" {
		return false
	}
	cleanPath := strings.TrimSuffix(filepath.ToSlash(filepath.Clean(path)), "/")
	ancestors := []string{cleanPath}
	for parent := filepath.ToSlash(filepath.Dir(cleanPath)); parent != "." && parent != "/" && parent != ""; parent = filepath.ToSlash(filepath.Dir(parent)) {
		ancestors = append(ancestors, parent)
	}
	ignored := false
	for _, line := range strings.Split(workspacePolicy, "\n") {
		pattern := strings.TrimSpace(line)
		if pattern == "" || strings.HasPrefix(pattern, "#") {
			continue
		}
		negated := strings.HasPrefix(pattern, "!")
		pattern = strings.TrimSpace(strings.TrimPrefix(pattern, "!"))
		pattern = strings.TrimPrefix(strings.TrimSuffix(filepath.ToSlash(pattern), "/"), "/")
		if pattern == "" {
			continue
		}
		for _, candidate := range ancestors {
			if ignorePatternMatches(pattern, candidate) {
				ignored = !negated
				break
			}
		}
	}
	return ignored
}
