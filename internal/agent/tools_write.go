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

// maxPatchMismatchSnippetBytes bounds how much of a source/patch line is
// echoed back in a mismatch error. These strings reach the model and the
// transcript, so the line is truncated before formatting rather than
// dumping unbounded file content.
const maxPatchMismatchSnippetBytes = 200

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

		// The @@ -a,b +c,d @@ header counts are metadata the model
		// frequently miscounts. Every context and removal line below is
		// independently verified against source as the body is walked, so
		// by the time a hunk finishes it has already been proven correct.
		// The counts are fully derivable from that walk; recompute rather
		// than reject a verified-correct edit over redundant metadata
		// (matches `patch(1)` and `git apply --recount`).
		for i < len(patchLines) && hunkHeaderPattern.FindStringSubmatch(patchLines[i]) == nil {
			line := patchLines[i]
			if strings.HasPrefix(line, "\\ No newline at end of file") {
				i++
				continue
			}
			if line == "" {
				if i == len(patchLines)-1 {
					break
				}
				// A context line whose content is empty should be a
				// single space in unified diff, but editors and models
				// routinely strip that trailing space. Treat it as one;
				// the context check below still requires the source to
				// actually have a blank line here, so this cannot apply
				// a wrong edit.
				line = " "
			}

			body := line[1:]
			switch line[0] {
			case ' ':
				if sourcePos >= len(source) || source[sourcePos] != body {
					return "", fmt.Errorf("patch context mismatch at old line %d: expected %s, found %s",
						sourcePos+1, patchMismatchSnippet(body), patchMismatchSourceLine(source, sourcePos))
				}
				result = append(result, body)
				sourcePos++
			case '-':
				if sourcePos >= len(source) || source[sourcePos] != body {
					return "", fmt.Errorf("patch removal mismatch at old line %d: expected %s, found %s",
						sourcePos+1, patchMismatchSnippet(body), patchMismatchSourceLine(source, sourcePos))
				}
				sourcePos++
			case '+':
				result = append(result, body)
			default:
				return "", fmt.Errorf("invalid patch line %q", line)
			}
			i++
		}
	}

	if !applied {
		return "", fmt.Errorf("patch contains no hunks")
	}
	result = append(result, source[sourcePos:]...)
	return strings.Join(result, "\n"), nil
}

// patchMismatchSnippet bounds a patch/source line before it is quoted into a
// mismatch error. %q (matching the "invalid patch line %q" error above)
// escapes control characters and preserves the exact whitespace that
// usually explains the mismatch.
func patchMismatchSnippet(value string) string {
	if value == "" {
		return "<empty line>"
	}
	return fmt.Sprintf("%q", truncateUTF8Bytes(value, maxPatchMismatchSnippetBytes))
}

// patchMismatchSourceLine is patchMismatchSnippet for a source position,
// reporting when the mismatch ran past the end of the file.
func patchMismatchSourceLine(source []string, pos int) string {
	if pos >= len(source) {
		return "<end of file>"
	}
	return patchMismatchSnippet(source[pos])
}
