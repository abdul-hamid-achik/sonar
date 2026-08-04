package agent

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

func (a *Agent) handleWrite(args map[string]any) (string, bool) {
	requestedPath, _ := args["path"].(string)
	content, _ := args["content"].(string)

	if requestedPath == "" {
		return "error: path is required", true
	}
	workspace, path, relative, err := a.openWritableRootForPath(requestedPath)
	if err != nil {
		return fmt.Sprintf("error: %v", err), true
	}
	defer func() { _ = workspace.Close() }()
	parent, name, err := workspace.openParent(relative, true)
	if err != nil {
		return fmt.Sprintf("error creating directory: %v", err), true
	}
	defer func() { _ = parent.Close() }()

	mode := os.FileMode(0o644)
	if info, statErr := parent.Stat(name); statErr == nil {
		mode = info.Mode().Perm()
	}
	if err := atomicWriteRoot(parent, name, []byte(content), mode); err != nil {
		return fmt.Sprintf("error writing file: %v", err), true
	}

	return fmt.Sprintf("Written to %s (%d bytes)", path, len(content)), false
}

func (a *Agent) handleEdit(args map[string]any) (string, bool) {
	requestedPath, _ := args["path"].(string)
	patch, _ := args["patch"].(string)

	if requestedPath == "" {
		return "error: path is required", true
	}
	if patch == "" {
		return "error: patch is required", true
	}

	workspace, path, relative, err := a.openWritableRootForPath(requestedPath)
	if err != nil {
		return fmt.Sprintf("error: %v", err), true
	}
	defer func() { _ = workspace.Close() }()
	parent, name, err := workspace.openParent(relative, false)
	if err != nil {
		return fmt.Sprintf("error reading file: %v", err), true
	}
	defer func() { _ = parent.Close() }()

	// Read current content
	oldContent, info, err := readPinnedRootFile(parent, name, maxFileReadBytes)
	if err != nil {
		return fmt.Sprintf("error reading file: %v", err), true
	}

	// Apply the patch
	newContent, err := applyPatch(string(oldContent), patch)
	if err != nil {
		return fmt.Sprintf("error applying patch: %v", err), true
	}

	if err := atomicWriteRoot(parent, name, []byte(newContent), info.Mode().Perm()); err != nil {
		return fmt.Sprintf("error writing file: %v", err), true
	}

	return fmt.Sprintf("Applied patch to %s (%d bytes)", path, len(newContent)), false
}

var hunkHeaderPattern = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// applyPatch applies validated unified-diff hunks while preserving every
// untouched prefix and suffix. Context and removed lines must match exactly;
// a stale model-generated patch therefore fails instead of corrupting a file.
func applyPatch(content, patch string) (string, error) {
	source := strings.Split(content, "\n")
	patchLines := strings.Split(patch, "\n")
	result := make([]string, 0, len(source))
	sourcePos := 0
	applied := false

	for i := 0; i < len(patchLines); {
		match := hunkHeaderPattern.FindStringSubmatch(patchLines[i])
		if match == nil {
			i++
			continue
		}
		applied = true
		oldStart, _ := strconv.Atoi(match[1])
		oldCount := 1
		if match[2] != "" {
			oldCount, _ = strconv.Atoi(match[2])
		}
		newCount := 1
		if match[4] != "" {
			newCount, _ = strconv.Atoi(match[4])
		}

		hunkStart := oldStart
		if hunkStart > 0 {
			hunkStart--
		}
		if hunkStart < sourcePos || hunkStart > len(source) {
			return "", fmt.Errorf("invalid or overlapping hunk at old line %d", oldStart)
		}
		result = append(result, source[sourcePos:hunkStart]...)
		sourcePos = hunkStart
		i++

		oldSeen, newSeen := 0, 0
		for i < len(patchLines) && hunkHeaderPattern.FindStringSubmatch(patchLines[i]) == nil {
			line := patchLines[i]
			if strings.HasPrefix(line, "\\ No newline at end of file") {
				i++
				continue
			}
			if line == "" && i == len(patchLines)-1 {
				break
			}
			if line == "" {
				return "", fmt.Errorf("invalid empty patch line in hunk")
			}

			body := line[1:]
			switch line[0] {
			case ' ':
				if sourcePos >= len(source) || source[sourcePos] != body {
					return "", fmt.Errorf("patch context mismatch at old line %d", sourcePos+1)
				}
				result = append(result, body)
				sourcePos++
				oldSeen++
				newSeen++
			case '-':
				if sourcePos >= len(source) || source[sourcePos] != body {
					return "", fmt.Errorf("patch removal mismatch at old line %d", sourcePos+1)
				}
				sourcePos++
				oldSeen++
			case '+':
				result = append(result, body)
				newSeen++
			default:
				return "", fmt.Errorf("invalid patch line %q", line)
			}
			i++
		}
		if oldSeen != oldCount || newSeen != newCount {
			return "", fmt.Errorf("hunk count mismatch: saw -%d/+%d, header declares -%d/+%d", oldSeen, newSeen, oldCount, newCount)
		}
	}

	if !applied {
		return "", fmt.Errorf("patch contains no hunks")
	}
	result = append(result, source[sourcePos:]...)
	return strings.Join(result, "\n"), nil
}
