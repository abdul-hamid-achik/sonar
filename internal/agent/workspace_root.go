package agent

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// workspaceRoot pins the workspace directory for one host operation. Policy
// resolution remains path based, but execution is relative to this descriptor,
// so swapping a parent symlink cannot redirect I/O outside the workspace.
type workspaceRoot struct {
	path      string
	root      *os.Root
	info      os.FileInfo
	closeOnce sync.Once
	closeErr  error
}

func (a *Agent) openWorkspaceRoot() (*workspaceRoot, error) {
	path, err := a.canonicalWorkDir()
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect workspace root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace root is not a directory: %s", path)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("open workspace root: %w", err)
	}
	pinned, err := root.Stat(".")
	if err != nil || !pinned.IsDir() || !os.SameFile(info, pinned) {
		_ = root.Close()
		if err != nil {
			return nil, fmt.Errorf("inspect opened workspace root: %w", err)
		}
		return nil, errors.New("workspace root changed while it was being opened")
	}
	return &workspaceRoot{path: path, root: root, info: pinned}, nil
}

func (workspace *workspaceRoot) Close() error {
	if workspace == nil || workspace.root == nil {
		return nil
	}
	workspace.closeOnce.Do(func() { workspace.closeErr = workspace.root.Close() })
	return workspace.closeErr
}

func (workspace *workspaceRoot) validate() error {
	if workspace == nil || workspace.root == nil {
		return errors.New("workspace root is unavailable")
	}
	current, err := os.Stat(workspace.path)
	if err != nil {
		return fmt.Errorf("revalidate workspace root: %w", err)
	}
	pinned, err := workspace.root.Stat(".")
	if err != nil {
		return fmt.Errorf("inspect pinned workspace root: %w", err)
	}
	if !os.SameFile(workspace.info, pinned) || !os.SameFile(pinned, current) {
		return errors.New("workspace root changed during operation")
	}
	return nil
}

func (workspace *workspaceRoot) relative(absolute string) (string, error) {
	if err := workspace.validate(); err != nil {
		return "", err
	}
	relative, inside, err := workspaceRelative(workspace.path, absolute)
	if err != nil {
		return "", err
	}
	if !inside {
		return "", fmt.Errorf("path %q escapes workspace %q", absolute, workspace.path)
	}
	return filepath.Clean(relative), nil
}

func (workspace *workspaceRoot) resolve(a *Agent, requested string, destructive bool) (absolute, relative string, err error) {
	if destructive {
		absolute, err = a.resolveDestructivePath(requested)
	} else {
		absolute, err = a.resolvePath(requested)
	}
	if err != nil {
		return "", "", err
	}
	relative, err = workspace.relative(absolute)
	if err != nil {
		return "", "", err
	}
	return absolute, relative, nil
}

// openParent pins the destination parent and rejects symlink components. When
// create is true, missing parents are created one component at a time.
func (workspace *workspaceRoot) openParent(relative string, create bool) (*os.Root, string, error) {
	relative = filepath.Clean(relative)
	if relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, "", fmt.Errorf("invalid workspace-relative path %q", relative)
	}
	parentName := filepath.Dir(relative)
	baseName := filepath.Base(relative)
	current, err := workspace.root.OpenRoot(".")
	if err != nil {
		return nil, "", err
	}
	if parentName == "." {
		return current, baseName, nil
	}
	for _, component := range strings.Split(parentName, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		info, statErr := current.Lstat(component)
		if os.IsNotExist(statErr) && create {
			if mkdirErr := current.Mkdir(component, 0o755); mkdirErr != nil && !os.IsExist(mkdirErr) {
				_ = current.Close()
				return nil, "", mkdirErr
			}
			info, statErr = current.Lstat(component)
		}
		if statErr != nil {
			_ = current.Close()
			return nil, "", statErr
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			_ = current.Close()
			return nil, "", fmt.Errorf("workspace parent component is not a real directory: %s", component)
		}
		next, openErr := current.OpenRoot(component)
		if openErr != nil {
			_ = current.Close()
			return nil, "", openErr
		}
		pinned, pinErr := next.Stat(".")
		if pinErr != nil || !os.SameFile(info, pinned) {
			_ = next.Close()
			_ = current.Close()
			if pinErr != nil {
				return nil, "", pinErr
			}
			return nil, "", fmt.Errorf("workspace parent component changed while opening: %s", component)
		}
		_ = current.Close()
		current = next
	}
	return current, baseName, nil
}

func (workspace *workspaceRoot) mkdirAll(relative string) error {
	if relative == "." {
		return nil
	}
	parent, name, err := workspace.openParent(filepath.Join(relative, ".keep"), true)
	if err != nil {
		return err
	}
	_ = name
	return parent.Close()
}

func atomicWriteRoot(parent *os.Root, name string, data []byte, mode os.FileMode) error {
	if parent == nil || filepath.Base(name) != name || name == "." || name == ".." {
		return errors.New("invalid atomic-write destination")
	}
	if info, err := parent.Lstat(name); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to replace symlink destination: %s", name)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if mode.Perm() == 0 {
		mode = 0o644
	}

	var random [8]byte
	for attempt := 0; attempt < 32; attempt++ {
		if _, err := rand.Read(random[:]); err != nil {
			return err
		}
		temporary := ".sonar-write-" + hex.EncodeToString(random[:])
		file, err := parent.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		cleanup := func() { _ = parent.Remove(temporary) }
		if err := file.Chmod(mode.Perm()); err != nil {
			_ = file.Close()
			cleanup()
			return err
		}
		if _, err := file.Write(data); err != nil {
			_ = file.Close()
			cleanup()
			return err
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			cleanup()
			return err
		}
		if err := file.Close(); err != nil {
			cleanup()
			return err
		}
		if err := parent.Rename(temporary, name); err != nil {
			cleanup()
			return err
		}
		return nil
	}
	return errors.New("could not allocate a unique atomic-write temporary file")
}

func readPinnedRootFile(parent *os.Root, name string, limit int64) ([]byte, os.FileInfo, error) {
	if parent == nil || filepath.Base(name) != name || name == "." || name == ".." {
		return nil, nil, errors.New("invalid pinned read target")
	}
	info, err := parent.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("path is not a regular file (%s)", info.Mode().Type())
	}
	file, err := parent.Open(name)
	if err != nil {
		return nil, nil, err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		_ = file.Close()
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, errors.New("file changed while it was being opened")
	}
	data, err := readBoundedOpenFile(file, limit)
	return data, opened, err
}
