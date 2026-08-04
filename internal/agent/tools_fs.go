package agent

import (
	"fmt"
	"os"
)

func (a *Agent) handleMkdir(args map[string]any) (string, bool) {
	requestedPath, _ := args["path"].(string)
	if requestedPath == "" {
		return "error: path is required", true
	}
	workspace, path, relative, err := a.openWritableRootForPath(requestedPath)
	if err != nil {
		return fmt.Sprintf("error: %v", err), true
	}
	defer func() { _ = workspace.Close() }()
	if err := workspace.mkdirAll(relative); err != nil {
		return fmt.Sprintf("error creating directory: %v", err), true
	}

	return fmt.Sprintf("Created directory: %s", path), false
}

func (a *Agent) handleRemove(args map[string]any) (string, bool) {
	requestedPath, _ := args["path"].(string)
	if requestedPath == "" {
		return "error: path is required", true
	}
	workspace, err := a.openWorkspaceRoot()
	if err != nil {
		return fmt.Sprintf("error: %v", err), true
	}
	defer func() { _ = workspace.Close() }()
	path, relative, err := workspace.resolve(a, requestedPath, true)
	if err != nil {
		return fmt.Sprintf("error: %v", err), true
	}
	if relative == "." {
		return "error: refusing to remove the workspace root", true
	}
	parent, name, err := workspace.openParent(relative, false)
	if err != nil {
		if os.IsNotExist(err) && a.getArgBool(args, "force", false) {
			return "Removed (ignored nonexistent)", false
		}
		return fmt.Sprintf("error: %v", err), true
	}
	defer func() { _ = parent.Close() }()

	recursive := a.getArgBool(args, "recursive", false)
	force := a.getArgBool(args, "force", false)

	info, err := parent.Lstat(name)
	if err != nil {
		if os.IsNotExist(err) {
			if force {
				return "Removed (ignored nonexistent)", false
			}
			return fmt.Sprintf("error: path does not exist: %s", path), true
		}
		return fmt.Sprintf("error: %v", err), true
	}

	if info.IsDir() {
		if recursive {
			err = parent.RemoveAll(name)
		} else {
			err = parent.Remove(name)
		}
	} else {
		err = parent.Remove(name)
	}

	if err != nil {
		return fmt.Sprintf("error removing: %v", err), true
	}
	return fmt.Sprintf("Removed: %s", path), false
}

func (a *Agent) handleCopy(args map[string]any) (string, bool) {
	source, _ := args["source"].(string)
	destination, _ := args["destination"].(string)

	if source == "" || destination == "" {
		return "error: source and destination are required", true
	}
	workspace, destination, destinationRelative, err := a.openWritableRootForPath(destination)
	if err != nil {
		return fmt.Sprintf("error: destination: %v", err), true
	}
	defer func() { _ = workspace.Close() }()

	readableSource, err := a.resolveReadablePath(source)
	if err != nil {
		return fmt.Sprintf("error: source: %v", err), true
	}
	defer func() { _ = readableSource.close() }()
	source = readableSource.absolute
	info, err := readableSource.stat()
	if err != nil {
		return fmt.Sprintf("error: %v", err), true
	}

	if info.IsDir() {
		return "error: copying directories not supported (use bash with cp -r)", true
	}

	srcData, err := readableSource.readBounded(maxCopyBytes)
	if err != nil {
		return fmt.Sprintf("error reading source: %v", err), true
	}

	parent, name, err := workspace.openParent(destinationRelative, true)
	if err != nil {
		return fmt.Sprintf("error creating destination directory: %v", err), true
	}
	defer func() { _ = parent.Close() }()

	err = atomicWriteRoot(parent, name, srcData, info.Mode().Perm())
	if err != nil {
		return fmt.Sprintf("error writing destination: %v", err), true
	}

	return fmt.Sprintf("Copied: %s -> %s", source, destination), false
}

func (a *Agent) handleMove(args map[string]any) (string, bool) {
	source, _ := args["source"].(string)
	destination, _ := args["destination"].(string)

	if source == "" || destination == "" {
		return "error: source and destination are required", true
	}
	workspace, err := a.openWorkspaceRoot()
	if err != nil {
		return fmt.Sprintf("error: %v", err), true
	}
	defer func() { _ = workspace.Close() }()
	source, sourceRelative, err := workspace.resolve(a, source, true)
	if err != nil {
		return fmt.Sprintf("error: source: %v", err), true
	}
	if sourceRelative == "." {
		return "error: refusing to move the workspace root", true
	}
	destination, destinationRelative, err := workspace.resolve(a, destination, true)
	if err != nil {
		return fmt.Sprintf("error: destination: %v", err), true
	}
	sourceParent, _, err := workspace.openParent(sourceRelative, false)
	if err != nil {
		return fmt.Sprintf("error: source: %v", err), true
	}
	defer func() { _ = sourceParent.Close() }()
	destinationParent, _, err := workspace.openParent(destinationRelative, true)
	if err != nil {
		return fmt.Sprintf("error creating destination directory: %v", err), true
	}
	defer func() { _ = destinationParent.Close() }()

	err = workspace.root.Rename(sourceRelative, destinationRelative)
	if err != nil {
		return fmt.Sprintf("error moving: %v", err), true
	}

	return fmt.Sprintf("Moved: %s -> %s", source, destination), false
}
