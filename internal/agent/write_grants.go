package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/abdul-hamid-achik/sonar/internal/config"
)

const maxAdditionalWriteAuthorities = 4

var errOutsideAdditionalWriteAuthority = errors.New("outside active temporary write authorities")

// WriteGrantKind distinguishes an additional writable workspace from one
// exact writable file. Grants are process-local host capabilities; they are
// never inferred by Agent and are never serialized into session state.
type WriteGrantKind string

const (
	WriteGrantDirectory WriteGrantKind = "directory"
	WriteGrantExactFile WriteGrantKind = "exact_file"
)

// WriteGrant is the opaque commit value produced by WritePathInspection.
// identity is deliberately unavailable to model, MCP, transcript, and JSON
// boundaries.
type WriteGrant struct {
	Path     string
	Kind     WriteGrantKind
	identity *writePathIdentity
}

// WritePathInspection is a non-mutating host preflight. UI preflight may
// inspect a path explicitly named by the user, present its exact canonical
// scope, then commit only the opaque Grant value.
type WritePathInspection struct {
	Path            string
	Kind            WriteGrantKind
	External        bool
	AlreadyWritable bool
	identity        *writePathIdentity
}

type writePathIdentity struct {
	path string
	kind WriteGrantKind
	info os.FileInfo
	pin  *os.File
	once sync.Once
}

func (inspection *WritePathInspection) Grant() WriteGrant {
	if inspection == nil {
		return WriteGrant{}
	}
	identity := inspection.identity
	inspection.identity = nil
	return WriteGrant{Path: inspection.Path, Kind: inspection.Kind, identity: identity}
}

func (inspection *WritePathInspection) Release() {
	if inspection == nil || inspection.identity == nil {
		return
	}
	inspection.identity.release()
	inspection.identity = nil
}

func (grant WriteGrant) Release() {
	if grant.identity != nil {
		grant.identity.release()
	}
}

func (identity *writePathIdentity) release() {
	if identity == nil {
		return
	}
	identity.once.Do(func() {
		if identity.pin != nil {
			_ = identity.pin.Close()
		}
	})
}

type additionalWriteAuthority struct {
	path          string
	kind          WriteGrantKind
	rootPath      string
	exactName     string
	root          *os.Root
	rootInfo      os.FileInfo
	ignoreContent string
}

func (grant *additionalWriteAuthority) currentRootInfo() (os.FileInfo, error) {
	if grant == nil || grant.root == nil || grant.rootInfo == nil {
		return nil, errors.New("write authority is unavailable")
	}
	pinned, err := grant.root.Stat(".")
	if err != nil {
		return nil, fmt.Errorf("write authority root is unavailable: %w", err)
	}
	current, err := os.Stat(grant.rootPath)
	if err != nil {
		return nil, err
	}
	if !pinned.IsDir() || !os.SameFile(grant.rootInfo, pinned) || !os.SameFile(pinned, current) {
		return nil, fmt.Errorf("write authority root changed after authorization: %s", grant.rootPath)
	}
	return pinned, nil
}

func (grant *additionalWriteAuthority) isCurrent() bool {
	if _, err := grant.currentRootInfo(); err != nil {
		return false
	}
	return grant.validateExactTarget() == nil
}

func (grant *additionalWriteAuthority) validateExactTarget() error {
	if grant == nil || grant.kind != WriteGrantExactFile {
		return nil
	}
	info, err := grant.root.Lstat(grant.exactName)
	if os.IsNotExist(err) {
		// The authority is path-entry scoped. Recreating the exact file is safe
		// while its parent root remains pinned.
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("exact write target is not a regular file: %s", grant.path)
	}
	return nil
}

func inspectWritePathIdentity(canonical string, kind WriteGrantKind, expected os.FileInfo) (*writePathIdentity, error) {
	pin, err := os.Open(canonical)
	if err != nil {
		return nil, fmt.Errorf("pin write path identity: %w", err)
	}
	info, err := pin.Stat()
	if err != nil {
		_ = pin.Close()
		return nil, fmt.Errorf("inspect pinned write path identity: %w", err)
	}
	validKind := (kind == WriteGrantDirectory && info.IsDir()) ||
		(kind == WriteGrantExactFile && info.Mode().IsRegular())
	if !validKind || expected == nil || !os.SameFile(expected, info) {
		_ = pin.Close()
		return nil, fmt.Errorf("write path changed during inspection: %s", canonical)
	}
	return &writePathIdentity{path: canonical, kind: kind, info: info, pin: pin}, nil
}

func validateInspectedWriteIdentity(expected *writePathIdentity, canonical string, kind WriteGrantKind, current os.FileInfo) error {
	if expected == nil || expected.pin == nil || expected.info == nil {
		return errors.New("write grant preview identity is unavailable; inspect the path again")
	}
	pinned, err := expected.pin.Stat()
	if err != nil || current == nil || filepath.Clean(expected.path) != filepath.Clean(canonical) ||
		expected.kind != kind || !os.SameFile(expected.info, pinned) || !os.SameFile(pinned, current) {
		return fmt.Errorf("write path changed after approval preview; inspect it again: %s", canonical)
	}
	return nil
}

func writeGrantSensitivePath(path string) bool {
	if config.HostSecretPathIgnored(path) {
		return true
	}
	for _, part := range strings.Split(strings.ToLower(filepath.ToSlash(filepath.Clean(path))), "/") {
		switch part {
		case ".gnupg", ".kube", "keychains", "login.keychain-db":
			return true
		}
	}
	return false
}

func broadWriteDirectory(path string) bool {
	path = filepath.Clean(path)
	if isFilesystemRoot(path) || filepath.Dir(path) == filepath.Clean(string(filepath.Separator)) {
		return true
	}
	protectedRoots := []string{os.TempDir()}
	if filepath.IsAbs(path) {
		protectedRoots = append(protectedRoots,
			filepath.Join(string(filepath.Separator), "private", "tmp"),
			filepath.Join(string(filepath.Separator), "private", "var"),
			filepath.Join(string(filepath.Separator), "private", "etc"),
			filepath.Join(string(filepath.Separator), "Users", "Shared"),
		)
	}
	for _, protected := range protectedRoots {
		protected = filepath.Clean(protected)
		if resolved, err := filepath.EvalSymlinks(protected); err == nil {
			protected = resolved
		}
		if same, err := sameExistingPath(protected, path); err == nil && same {
			return true
		}
	}
	if filepath.IsAbs(path) {
		privateVar := filepath.Join(string(filepath.Separator), "private", "var")
		if resolved, err := filepath.EvalSymlinks(privateVar); err == nil {
			privateVar = resolved
		}
		if relative, inside, err := physicalPathRelative(privateVar, path); err == nil && inside &&
			(relative == "." || filepath.Dir(relative) == ".") {
			return true
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		home = filepath.Clean(home)
	}
	if same, err := sameExistingPath(home, path); err == nil && same {
		return true
	}
	if relative, inside, err := physicalPathRelative(home, path); err == nil && inside {
		return relative == "." || filepath.Dir(relative) == "."
	}
	return false
}

func (a *Agent) absoluteWriteIntentPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("write path is empty")
	}
	if filepath.IsAbs(path) || path == "~" || strings.HasPrefix(path, "~"+string(filepath.Separator)) {
		return absoluteCleanReadPath(path)
	}
	base := strings.TrimSpace(a.activeWorkDir())
	if base == "" {
		var err error
		base, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve write intent workspace: %w", err)
		}
	}
	absolute, err := filepath.Abs(filepath.Join(base, path))
	if err != nil {
		return "", fmt.Errorf("resolve write path %q: %w", path, err)
	}
	return filepath.Clean(absolute), nil
}

// InspectWritePath canonicalizes one existing host directory or regular file
// without changing authority. It rejects broad and secret-bearing roots before
// returning an opaque identity for UI confirmation.
func (a *Agent) InspectWritePath(path string) (WritePathInspection, error) {
	requested, err := a.absoluteWriteIntentPath(path)
	if err != nil {
		return WritePathInspection{}, err
	}
	if info, lstatErr := os.Lstat(requested); lstatErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return WritePathInspection{}, fmt.Errorf("refusing a symlink as write authority: %s", requested)
	}
	canonical, info, err := canonicalExistingReadPath(requested)
	if err != nil {
		return WritePathInspection{}, err
	}
	kind := WriteGrantDirectory
	switch {
	case info.IsDir():
		if broadWriteDirectory(canonical) {
			return WritePathInspection{}, fmt.Errorf("refusing broad directory as write authority: %s", canonical)
		}
	case info.Mode().IsRegular():
		kind = WriteGrantExactFile
	default:
		return WritePathInspection{}, fmt.Errorf("write path is neither a regular file nor directory: %s", canonical)
	}
	if writeGrantSensitivePath(canonical) {
		return WritePathInspection{}, fmt.Errorf("refusing secret-bearing path as write authority: %s", canonical)
	}

	workspace, err := a.canonicalWorkDir()
	if err != nil {
		return WritePathInspection{}, err
	}
	if _, inside, relErr := physicalPathRelative(workspace, canonical); relErr != nil {
		return WritePathInspection{}, fmt.Errorf("compare write path with workspace: %w", relErr)
	} else if inside {
		return WritePathInspection{Path: canonical, Kind: kind, AlreadyWritable: true}, nil
	}
	if kind == WriteGrantDirectory {
		if overlaps, overlapErr := physicalPathsOverlap(workspace, canonical); overlapErr != nil {
			return WritePathInspection{}, fmt.Errorf("compare write root with workspace: %w", overlapErr)
		} else if overlaps {
			return WritePathInspection{}, fmt.Errorf("write root %q ambiguously overlaps workspace %q", canonical, workspace)
		}
	}

	a.mu.RLock()
	for _, existing := range a.writeAuthorities {
		if !existing.isCurrent() {
			continue
		}
		if existing.kind == WriteGrantDirectory {
			if _, inside, relErr := physicalPathRelative(existing.path, canonical); relErr == nil && inside {
				a.mu.RUnlock()
				return WritePathInspection{Path: canonical, Kind: kind, AlreadyWritable: true}, nil
			}
		}
		if existing.kind == WriteGrantExactFile && kind == WriteGrantExactFile && filepath.Clean(existing.path) == canonical {
			a.mu.RUnlock()
			return WritePathInspection{Path: canonical, Kind: kind, AlreadyWritable: true}, nil
		}
		if kind == WriteGrantDirectory {
			if overlaps, overlapErr := physicalPathsOverlap(existing.rootPath, canonical); overlapErr != nil {
				a.mu.RUnlock()
				return WritePathInspection{}, fmt.Errorf("compare write authorities: %w", overlapErr)
			} else if overlaps {
				a.mu.RUnlock()
				return WritePathInspection{}, fmt.Errorf("write authority %q ambiguously overlaps existing scope %q", canonical, existing.path)
			}
		}
	}
	a.mu.RUnlock()

	identity, err := inspectWritePathIdentity(canonical, kind, info)
	if err != nil {
		return WritePathInspection{}, err
	}
	return WritePathInspection{Path: canonical, Kind: kind, External: true, identity: identity}, nil
}

// AddInspectedWriteGrant commits only a capability returned by
// InspectWritePath. Agent never accepts a raw model-supplied path as authority.
func (a *Agent) AddInspectedWriteGrant(grant WriteGrant) (string, error) {
	defer grant.Release()
	if grant.identity == nil {
		return "", errors.New("write grant preview identity is unavailable; inspect the path again")
	}
	canonical, info, err := canonicalExistingReadPath(grant.Path)
	if err != nil {
		return "", err
	}
	if writeGrantSensitivePath(canonical) || (grant.Kind == WriteGrantDirectory && broadWriteDirectory(canonical)) {
		return "", fmt.Errorf("write path is not eligible for temporary authority: %s", canonical)
	}
	if err := validateInspectedWriteIdentity(grant.identity, canonical, grant.Kind, info); err != nil {
		return "", err
	}
	if (grant.Kind == WriteGrantDirectory && !info.IsDir()) ||
		(grant.Kind == WriteGrantExactFile && !info.Mode().IsRegular()) {
		return "", fmt.Errorf("write grant kind no longer matches path: %s", canonical)
	}

	workspace, err := a.canonicalWorkDir()
	if err != nil {
		return "", err
	}
	if overlaps, overlapErr := physicalPathsOverlap(workspace, canonical); overlapErr == nil && overlaps {
		return "", fmt.Errorf("write authority %q overlaps writable workspace %q", canonical, workspace)
	} else if overlapErr != nil && grant.Kind == WriteGrantDirectory {
		return "", fmt.Errorf("compare write authority with workspace: %w", overlapErr)
	}

	rootPath := canonical
	exactName := ""
	if grant.Kind == WriteGrantExactFile {
		rootPath = filepath.Dir(canonical)
		exactName = filepath.Base(canonical)
	}
	ignore, err := config.LoadIgnoreFileWithError(rootPath)
	if err != nil {
		return "", fmt.Errorf("load write authority ignore policy: %w", err)
	}
	ignoreContent := ignore.EffectiveRaw()
	relative := "."
	if exactName != "" {
		relative = exactName
	}
	if pathIgnoredWithContent(ignoreContent, relative) {
		return "", fmt.Errorf("write path %q is excluded by host or .agentignore policy", canonical)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return "", fmt.Errorf("open write authority root: %w", err)
	}
	pinned, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return "", fmt.Errorf("inspect write authority root: %w", err)
	}
	currentRoot, err := os.Stat(rootPath)
	if err != nil || !pinned.IsDir() || !os.SameFile(pinned, currentRoot) {
		_ = root.Close()
		return "", fmt.Errorf("write authority root changed during authorization: %s", rootPath)
	}
	if grant.Kind == WriteGrantExactFile {
		current, currentErr := root.Lstat(exactName)
		if currentErr != nil || !current.Mode().IsRegular() || !os.SameFile(current, info) {
			_ = root.Close()
			return "", fmt.Errorf("exact write file changed during authorization: %s", canonical)
		}
	}
	authority := &additionalWriteAuthority{
		path: canonical, kind: grant.Kind, rootPath: rootPath, exactName: exactName,
		root: root, rootInfo: pinned, ignoreContent: ignoreContent,
	}

	a.turnMu.Lock()
	defer a.turnMu.Unlock()
	if a.closed {
		_ = root.Close()
		return "", errors.New("agent is closed")
	}
	if a.turnRunning.Load() {
		_ = root.Close()
		return "", errors.New("write authority cannot change during an active agent turn")
	}

	a.mu.Lock()
	a.pruneStaleWriteAuthoritiesLocked()
	if existing := a.writeAuthorities[canonical]; existing != nil {
		a.mu.Unlock()
		_ = root.Close()
		return existing.path, nil
	}
	for _, existing := range a.writeAuthorities {
		if existing.kind == WriteGrantDirectory {
			if _, inside, relErr := physicalPathRelative(existing.path, canonical); relErr == nil && inside {
				a.mu.Unlock()
				_ = root.Close()
				return existing.path, nil
			}
		}
		if grant.Kind == WriteGrantDirectory {
			overlaps, overlapErr := physicalPathsOverlap(existing.rootPath, canonical)
			if overlapErr != nil || overlaps {
				a.mu.Unlock()
				_ = root.Close()
				if overlapErr != nil {
					return "", fmt.Errorf("compare write authorities: %w", overlapErr)
				}
				return "", fmt.Errorf("write authority %q ambiguously overlaps existing scope %q", canonical, existing.path)
			}
		}
	}
	if len(a.writeAuthorities) >= maxAdditionalWriteAuthorities {
		a.mu.Unlock()
		_ = root.Close()
		return "", fmt.Errorf("write authority limit reached (%d)", maxAdditionalWriteAuthorities)
	}
	if a.writeAuthorities == nil {
		a.writeAuthorities = make(map[string]*additionalWriteAuthority)
	}
	a.writeAuthorities[canonical] = authority
	a.mu.Unlock()
	return canonical, nil
}

func (a *Agent) pruneStaleWriteAuthoritiesLocked() {
	for path, authority := range a.writeAuthorities {
		if authority.isCurrent() {
			continue
		}
		delete(a.writeAuthorities, path)
		_ = authority.root.Close()
	}
}

// WriteGrants returns a stable display-only projection. No identity handles
// cross this boundary.
func (a *Agent) WriteGrants() []WriteGrant {
	a.mu.RLock()
	grants := make([]WriteGrant, 0, len(a.writeAuthorities))
	for _, authority := range a.writeAuthorities {
		grants = append(grants, WriteGrant{Path: authority.path, Kind: authority.kind})
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

// RemoveWritePath revokes one exact temporary write authority. The caller must
// pass the canonical path returned by AddInspectedWriteGrant (or exposed by
// WriteGrants); a child path never implicitly revokes a containing directory.
// This narrow host API exists so a failed multi-grant UI transaction can roll
// back only the capabilities it introduced.
func (a *Agent) RemoveWritePath(path string) (WriteGrant, error) {
	a.turnMu.Lock()
	defer a.turnMu.Unlock()
	if a.closed {
		return WriteGrant{}, errors.New("agent is closed")
	}
	if a.turnRunning.Load() {
		return WriteGrant{}, errors.New("write authority cannot change during an active agent turn")
	}
	requested, err := a.absoluteWriteIntentPath(path)
	if err != nil {
		return WriteGrant{}, err
	}
	requested = filepath.Clean(requested)

	a.mu.Lock()
	authority := a.writeAuthorities[requested]
	if authority == nil {
		a.pruneStaleWriteAuthoritiesLocked()
		a.mu.Unlock()
		return WriteGrant{}, fmt.Errorf("temporary write authority not found: %s", requested)
	}
	delete(a.writeAuthorities, requested)
	a.mu.Unlock()
	if err := authority.root.Close(); err != nil {
		return WriteGrant{Path: authority.path, Kind: authority.kind}, fmt.Errorf("close write authority: %w", err)
	}
	return WriteGrant{Path: authority.path, Kind: authority.kind}, nil
}

// ClearWriteGrants revokes every additional write capability. They are never
// persisted, and Close calls the same cleanup boundary.
func (a *Agent) ClearWriteGrants() (int, error) {
	a.turnMu.Lock()
	defer a.turnMu.Unlock()
	if a.closed {
		return 0, errors.New("agent is closed")
	}
	if a.turnRunning.Load() {
		return 0, errors.New("write authority cannot change during an active agent turn")
	}
	a.mu.Lock()
	authorities := make([]*additionalWriteAuthority, 0, len(a.writeAuthorities))
	for _, authority := range a.writeAuthorities {
		authorities = append(authorities, authority)
	}
	a.writeAuthorities = make(map[string]*additionalWriteAuthority)
	a.mu.Unlock()
	var closeErr error
	for _, authority := range authorities {
		closeErr = errors.Join(closeErr, authority.root.Close())
	}
	if closeErr != nil {
		return len(authorities), fmt.Errorf("close write authorities: %w", closeErr)
	}
	return len(authorities), nil
}

func (a *Agent) closeWriteGrants() {
	a.mu.Lock()
	authorities := make([]*additionalWriteAuthority, 0, len(a.writeAuthorities))
	for _, authority := range a.writeAuthorities {
		authorities = append(authorities, authority)
	}
	a.writeAuthorities = nil
	a.mu.Unlock()
	for _, authority := range authorities {
		_ = authority.root.Close()
	}
}

func (a *Agent) resolveAdditionalWritePath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("write path is empty")
	}
	requested, err := a.absoluteWriteIntentPath(path)
	if err != nil {
		return "", err
	}
	requestedEntry := requested
	if parent, parentErr := filepath.EvalSymlinks(filepath.Dir(requested)); parentErr == nil {
		requestedEntry = filepath.Join(parent, filepath.Base(requested))
	}

	// Exact-file authority is attached to the approved directory entry. Check
	// its lexical canonical spelling before resolving the final component so a
	// later symlink replacement cannot redirect comparison to its target.
	a.mu.RLock()
	for _, authority := range a.writeAuthorities {
		if authority.kind != WriteGrantExactFile || filepath.Clean(requestedEntry) != authority.path {
			continue
		}
		if _, currentErr := authority.currentRootInfo(); currentErr != nil {
			a.mu.RUnlock()
			return "", currentErr
		}
		if exactErr := authority.validateExactTarget(); exactErr != nil {
			a.mu.RUnlock()
			return "", exactErr
		}
		a.mu.RUnlock()
		return authority.path, nil
	}
	a.mu.RUnlock()

	candidate := requested
	candidate, err = resolveExistingAncestor(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve additional write path %q: %w", path, err)
	}

	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, authority := range a.writeAuthorities {
		if _, currentErr := authority.currentRootInfo(); currentErr != nil {
			continue
		}
		if authority.kind == WriteGrantExactFile {
			if filepath.Clean(candidate) != authority.path {
				continue
			}
			if err := authority.validateExactTarget(); err != nil {
				return "", err
			}
			return authority.path, nil
		}
		relative, inside, relErr := physicalCanonicalRelative(authority.path, candidate)
		if relErr != nil || !inside {
			continue
		}
		if pathIgnoredWithContent(authority.ignoreContent, relative) {
			return "", fmt.Errorf("path %q is excluded by %s/.agentignore", path, authority.path)
		}
		return filepath.Join(authority.path, relative), nil
	}
	return "", fmt.Errorf("%w: path %q", errOutsideAdditionalWriteAuthority, path)
}

func (a *Agent) additionalWriteAllowsTool(resolved, tool string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, authority := range a.writeAuthorities {
		if !authority.isCurrent() {
			continue
		}
		if authority.kind == WriteGrantExactFile {
			if filepath.Clean(resolved) == authority.path {
				return tool == "write" || tool == "edit"
			}
			continue
		}
		if _, inside, err := physicalPathRelative(authority.path, resolved); err == nil && inside {
			return tool == "write" || tool == "edit" || tool == "mkdir"
		}
	}
	return false
}

func (a *Agent) additionalWriteAllowsWorkspace(resolved string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, authority := range a.writeAuthorities {
		if authority.kind != WriteGrantDirectory || !authority.isCurrent() {
			continue
		}
		if _, inside, err := physicalPathRelative(authority.path, resolved); err == nil && inside {
			return true
		}
	}
	return false
}

// openWritableRootForPath returns an independently closable pinned root for
// either the primary workspace or one exact temporary write authority.
func (a *Agent) openWritableRootForPath(requested string) (*workspaceRoot, string, string, error) {
	resolved, err := a.resolvePath(requested)
	if err != nil {
		return nil, "", "", err
	}
	workspace, workspaceErr := a.openWorkspaceRoot()
	if workspaceErr == nil {
		if relative, relativeErr := workspace.relative(resolved); relativeErr == nil {
			return workspace, resolved, relative, nil
		}
		_ = workspace.Close()
	}

	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, authority := range a.writeAuthorities {
		if _, currentErr := authority.currentRootInfo(); currentErr != nil {
			continue
		}
		allowed := false
		if authority.kind == WriteGrantExactFile {
			allowed = filepath.Clean(resolved) == authority.path && authority.validateExactTarget() == nil
		} else if _, inside, insideErr := physicalPathRelative(authority.path, resolved); insideErr == nil {
			allowed = inside
		}
		if !allowed {
			continue
		}
		root, openErr := authority.root.OpenRoot(".")
		if openErr != nil {
			return nil, "", "", openErr
		}
		pinned, pinErr := root.Stat(".")
		if pinErr != nil || !os.SameFile(authority.rootInfo, pinned) {
			_ = root.Close()
			if pinErr != nil {
				return nil, "", "", pinErr
			}
			return nil, "", "", errors.New("write authority changed while opening")
		}
		writable := &workspaceRoot{path: authority.rootPath, root: root, info: pinned}
		relative, relativeErr := writable.relative(resolved)
		if relativeErr != nil {
			_ = writable.Close()
			return nil, "", "", relativeErr
		}
		return writable, resolved, relative, nil
	}
	if workspaceErr != nil {
		return nil, "", "", workspaceErr
	}
	return nil, "", "", fmt.Errorf("path %q has no writable root", requested)
}
