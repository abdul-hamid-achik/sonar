package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	executionpkg "github.com/abdul-hamid-achik/sonar/internal/execution"
	permissionpkg "github.com/abdul-hamid-achik/sonar/internal/permission"
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

// handleEdit edits one file in place under two mutually exclusive modes.
//
// The primary mode is exact string replacement: old_string is located
// verbatim in the current file and becomes new_string. It requires no line
// numbers, no hunk headers and no leading markers, which is exactly the
// bookkeeping models get wrong. An old_string that matches zero or several
// locations is refused with instructions rather than resolved by guessing.
//
// The unified-diff patch mode is retained for existing callers and for edits
// a model prefers to express as a diff. Both modes share this function's
// workspace containment, permission classification and atomic write.
func (a *Agent) handleEdit(args map[string]any) (string, bool) {
	requestedPath, _ := args["path"].(string)
	patch, _ := args["patch"].(string)
	edit, replacementMode := parseReplacementEdit(args)

	if requestedPath == "" {
		return "error: path is required", true
	}
	switch {
	case replacementMode && patch != "":
		return "error: pass either old_string/new_string or patch, not both", true
	case !replacementMode && patch == "":
		return "error: old_string and new_string are required (patch is accepted as an alternative)", true
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

	var (
		newContent string
		summary    string
	)
	if replacementMode {
		updated, replacements, replaceErr := applyReplacementEdit(string(oldContent), edit)
		if replaceErr != nil {
			return fmt.Sprintf("error editing file: %v", replaceErr), true
		}
		newContent = updated
		summary = fmt.Sprintf("Replaced %s in %s", pluralOccurrences(replacements), path)
	} else {
		// Apply the patch
		updated, patchErr := applyPatch(string(oldContent), patch)
		if patchErr != nil {
			return fmt.Sprintf("error applying patch: %v", patchErr), true
		}
		newContent = updated
		summary = fmt.Sprintf("Applied patch to %s", path)
	}

	if err := atomicWriteRoot(parent, name, []byte(newContent), info.Mode().Perm()); err != nil {
		return fmt.Sprintf("error writing file: %v", err), true
	}

	return fmt.Sprintf("%s (%d bytes)", summary, len(newContent)), false
}

// replacementEdit is the string-replacement form of an edit call: exact text
// in, exact text out. It carries no positional information because position
// is derived from the file itself.
type replacementEdit struct {
	oldString  string
	newString  string
	replaceAll bool
}

// parseReplacementEdit reports whether an edit call selected string
// replacement and returns its payload. The presence of either string field
// selects the mode, so a call that supplies only one of them still fails as a
// replacement (with a targeted message) instead of being read as a patch.
func parseReplacementEdit(args map[string]any) (replacementEdit, bool) {
	_, hasOld := args["old_string"]
	_, hasNew := args["new_string"]
	if !hasOld && !hasNew {
		return replacementEdit{}, false
	}
	oldString, _ := args["old_string"].(string)
	newString, _ := args["new_string"].(string)
	replaceAll, _ := args["replace_all"].(bool)
	return replacementEdit{oldString: oldString, newString: newString, replaceAll: replaceAll}, true
}

// validate rejects the two payloads that can never identify a single edit,
// independently of any file content, so the failure is reported before a
// dispatch record or an approval prompt is created.
func (e replacementEdit) validate() error {
	if e.oldString == "" {
		return errors.New("old_string is required and must not be empty: copy the exact text to replace from the current file, or use the write tool to create a whole file")
	}
	if e.oldString == e.newString {
		return errors.New("old_string and new_string are identical, so this edit would change nothing")
	}
	return nil
}

// maxReplacementNearMissCells bounds the whitespace-insensitive near-miss
// scan used only to explain a failed match.
const maxReplacementNearMissCells = 4_000_000

// applyReplacementEdit performs one exact, literal replacement and returns
// the new content plus the number of replaced occurrences. Nothing here is
// interpreted as a pattern: old_string is matched byte for byte, so regex
// metacharacters, tabs and non-ASCII text carry no special meaning.
//
// Ambiguity is a hard error. A target that appears in several places could be
// edited in the wrong one, and a silently misplaced edit is far more expensive
// than a refused call, so the model is told how to make the target unique
// instead of having one location chosen for it.
func applyReplacementEdit(content string, edit replacementEdit) (string, int, error) {
	if err := edit.validate(); err != nil {
		return "", 0, err
	}
	switch matches := countExactMatches(content, edit.oldString); {
	case matches == 0:
		return "", 0, fmt.Errorf("old_string was not found in the file%s. Read the file again and copy the exact text, including indentation and any change already applied earlier in this session. Received: %s",
			replacementNearMiss(content, edit.oldString), patchMismatchSnippet(edit.oldString))
	case matches > 1 && !edit.replaceAll:
		return "", 0, fmt.Errorf("old_string matches %d locations in the file, so the intended one is ambiguous and no edit was made. Extend old_string (and new_string) with surrounding lines until it matches exactly one location, or pass replace_all: true to change all %d",
			matches, matches)
	}
	// strings.Count is the non-overlapping count actually consumed by
	// ReplaceAll; countExactMatches above is deliberately overlap-aware so
	// that an overlapping target is refused as ambiguous rather than
	// silently resolved left to right.
	replacements := strings.Count(content, edit.oldString)
	return strings.ReplaceAll(content, edit.oldString, edit.newString), replacements, nil
}

// countExactMatches counts every offset where target occurs, including
// overlapping occurrences. Overlap matters for the uniqueness rule: two
// overlapping candidate locations are still two locations.
func countExactMatches(content, target string) int {
	if target == "" {
		return 0
	}
	count := 0
	for offset := 0; offset+len(target) <= len(content); {
		index := strings.Index(content[offset:], target)
		if index < 0 {
			break
		}
		count++
		offset += index + 1
	}
	return count
}

// replacementNearMiss explains a failed exact match when the file holds the
// same lines apart from leading or trailing whitespace, which is the common
// shape of a model-authored target. It never relaxes the match: the edit is
// still refused, the model is simply told that indentation rather than
// location is what differed.
func replacementNearMiss(content, target string) string {
	targetLines := strings.Split(target, "\n")
	sourceLines := strings.Split(content, "\n")
	if len(targetLines) > len(sourceLines) {
		return ""
	}
	if len(targetLines) > 0 && len(sourceLines) > maxReplacementNearMissCells/len(targetLines) {
		return ""
	}
	trimmedTarget := make([]string, len(targetLines))
	for index, line := range targetLines {
		trimmedTarget[index] = strings.TrimSpace(line)
	}
	found, line := 0, 0
	for start := 0; start+len(targetLines) <= len(sourceLines); start++ {
		matched := true
		for offset, want := range trimmedTarget {
			if strings.TrimSpace(sourceLines[start+offset]) != want {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		found++
		if found > 1 {
			return ""
		}
		line = start + 1
	}
	if found != 1 {
		return ""
	}
	return fmt.Sprintf(" (line %d differs only in leading or trailing whitespace; match the file's exact indentation)", line)
}

func pluralOccurrences(count int) string {
	if count == 1 {
		return "1 occurrence"
	}
	return fmt.Sprintf("%d occurrences", count)
}

// preflightEditArguments admits an edit call before it is recorded and
// dispatched. It accepts either edit mode and rejects payloads that cannot
// name a single, well-formed change under the mode they selected.
func preflightEditArguments(args map[string]any) error {
	edit, replacementMode := parseReplacementEdit(args)
	patch, _ := args["patch"].(string)
	if !replacementMode {
		if _, ok := args["patch"]; !ok {
			return errors.New("edit requires old_string and new_string (exact text replacement), or patch (a complete unified diff)")
		}
		return preflightRequiredString(args, "patch", false)
	}
	if patch != "" {
		return errors.New("edit accepts either old_string/new_string or patch, not both")
	}
	if _, ok := args["old_string"].(string); !ok {
		return errors.New("old_string must be a string")
	}
	if _, ok := args["new_string"]; !ok {
		return errors.New("new_string is required; pass an empty string to delete the matched text")
	}
	if _, ok := args["new_string"].(string); !ok {
		return errors.New("new_string must be a string")
	}
	return edit.validate()
}

// replacementEditPreview fills the approval preview for a string-replacement
// edit. The preview kind stays PreviewFilePatch so approval classification,
// session scoping and host rendering are unchanged; only the diff body is
// derived from the exact replacement rather than copied from a
// model-supplied patch.
func (a *Agent) replacementEditPreview(ctx context.Context, preview *permissionpkg.ApprovalPreview, edit replacementEdit) {
	preview.Kind = permissionpkg.PreviewFilePatch
	preview.Consequence = "Replaces the exact matched text in the target file."
	before, exists, reason := a.approvalExistingContent(preview.Path)
	if exists {
		preview.ExistingSHA256 = executionpkg.HashText(before)
	}
	if reason != "" {
		preview.DiffOmittedReason = reason
		return
	}
	after, _, err := applyReplacementEdit(before, edit)
	if err != nil {
		preview.DiffOmittedReason = fmt.Sprintf("replacement could not be applied for preview: %v", err)
		return
	}
	preview.ByteSize = int64(len(after))
	preview.ContentSHA256 = executionpkg.HashText(after)
	preview.Diff, preview.DiffTruncated, preview.DiffOmittedReason = approvalDiff(ctx, before, after)
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
		return "", errors.New("patch contains no hunks: no line matched the required \"@@ -start,count +start,count @@\" header. Prefer the old_string/new_string arguments, which replace exact text and need no headers or line numbers")
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
