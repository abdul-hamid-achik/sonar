package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/llm"
	permissionpkg "github.com/abdul-hamid-achik/sonar/internal/permission"
)

// TestApplyReplacementEdit covers the string-replacement primitive itself:
// exact literal matching, refusal of every ambiguous target, and the content
// classes a line-oriented patch parser handles badly.
func TestApplyReplacementEdit(t *testing.T) {
	tests := []struct {
		name         string
		content      string
		edit         replacementEdit
		want         string
		wantCount    int
		wantErr      bool
		wantErrParts []string
	}{
		{
			name:      "unique single-line replacement",
			content:   "alpha\nbeta\ngamma\n",
			edit:      replacementEdit{oldString: "beta", newString: "BETA"},
			want:      "alpha\nBETA\ngamma\n",
			wantCount: 1,
		},
		{
			name:      "multi-line replacement spanning several lines",
			content:   "func main() {\n\tfmt.Println(\"a\")\n\tfmt.Println(\"b\")\n}\n",
			edit:      replacementEdit{oldString: "\tfmt.Println(\"a\")\n\tfmt.Println(\"b\")\n", newString: "\tfmt.Println(\"a\", \"b\")\n"},
			want:      "func main() {\n\tfmt.Println(\"a\", \"b\")\n}\n",
			wantCount: 1,
		},
		{
			name:      "replacement that expands one line into many",
			content:   "start\nmiddle\nend\n",
			edit:      replacementEdit{oldString: "middle\n", newString: "one\ntwo\nthree\n"},
			want:      "start\none\ntwo\nthree\nend\n",
			wantCount: 1,
		},
		{
			name:      "empty new_string deletes the matched text",
			content:   "keep\ndrop me\nkeep\n",
			edit:      replacementEdit{oldString: "drop me\n", newString: ""},
			want:      "keep\nkeep\n",
			wantCount: 1,
		},
		{
			name:      "regex metacharacters are matched literally",
			content:   "if m := re.FindString(`^a.*b$`); m != \"\" {\n",
			edit:      replacementEdit{oldString: "`^a.*b$`", newString: "`^a[0-9]+b$`"},
			want:      "if m := re.FindString(`^a[0-9]+b$`); m != \"\" {\n",
			wantCount: 1,
		},
		{
			name:      "a dot in old_string does not match an arbitrary character",
			content:   "value := a.b\nother := axb\n",
			edit:      replacementEdit{oldString: "a.b", newString: "a.c"},
			want:      "value := a.c\nother := axb\n",
			wantCount: 1,
		},
		{
			name:      "unicode and emoji are replaced byte-exactly",
			content:   "greeting := \"hola señor 🌮\"\n",
			edit:      replacementEdit{oldString: "\"hola señor 🌮\"", newString: "\"hola señora 🌯\""},
			want:      "greeting := \"hola señora 🌯\"\n",
			wantCount: 1,
		},
		{
			// The two lines are the same word in NFC and NFD form. A
			// normalizing matcher would see two matches and either refuse
			// or change the wrong line; byte-exact matching sees one.
			name:      "combining characters are not normalized away",
			content:   "caf\u00e9\ncafe\u0301\n",
			edit:      replacementEdit{oldString: "caf\u00e9\n", newString: "coffee\n"},
			want:      "coffee\ncafe\u0301\n",
			wantCount: 1,
		},
		{
			name:      "replace_all changes every occurrence",
			content:   "old\nkeep\nold\nold\n",
			edit:      replacementEdit{oldString: "old", newString: "new", replaceAll: true},
			want:      "new\nkeep\nnew\nnew\n",
			wantCount: 3,
		},
		{
			name:         "zero matches is refused with repair instructions",
			content:      "alpha\nbeta\n",
			edit:         replacementEdit{oldString: "delta", newString: "DELTA"},
			wantErr:      true,
			wantErrParts: []string{"old_string was not found", "Read the file again", `"delta"`},
		},
		{
			name:         "several matches are refused instead of guessing",
			content:      "count++\nsomething\ncount++\n",
			edit:         replacementEdit{oldString: "count++", newString: "count += 2"},
			wantErr:      true,
			wantErrParts: []string{"matches 2 locations", "ambiguous", "Extend old_string", "replace_all: true"},
		},
		{
			name:         "overlapping matches count as several locations",
			content:      "aaa\n",
			edit:         replacementEdit{oldString: "aa", newString: "b"},
			wantErr:      true,
			wantErrParts: []string{"matches 2 locations"},
		},
		{
			name:         "empty old_string is refused",
			content:      "alpha\n",
			edit:         replacementEdit{oldString: "", newString: "beta"},
			wantErr:      true,
			wantErrParts: []string{"old_string is required", "write tool"},
		},
		{
			name:         "identical strings are refused as a no-op",
			content:      "alpha\n",
			edit:         replacementEdit{oldString: "alpha", newString: "alpha"},
			wantErr:      true,
			wantErrParts: []string{"identical"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, count, err := applyReplacementEdit(tt.content, tt.edit)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got content %q", got)
				}
				for _, part := range tt.wantErrParts {
					if !strings.Contains(err.Error(), part) {
						t.Fatalf("error %q does not contain %q", err.Error(), part)
					}
				}
				if got != "" || count != 0 {
					t.Fatalf("failed edit returned content %q and count %d, want no output", got, count)
				}
				return
			}
			if err != nil {
				t.Fatalf("applyReplacementEdit: unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("content = %q, want %q", got, tt.want)
			}
			if count != tt.wantCount {
				t.Fatalf("replacement count = %d, want %d", count, tt.wantCount)
			}
		})
	}
}

// TestReplacementEditRefusesAmbiguityUntilUnique is the disambiguation
// contract in one flow: the same target is refused while it is ambiguous and
// accepted once the model widens it with surrounding context, exactly as the
// error message instructs.
func TestReplacementEditRefusesAmbiguityUntilUnique(t *testing.T) {
	content := "func a() {\n\treturn nil\n}\n\nfunc b() {\n\treturn nil\n}\n"

	if _, _, err := applyReplacementEdit(content, replacementEdit{oldString: "\treturn nil\n", newString: "\treturn errNotReady\n"}); err == nil {
		t.Fatal("ambiguous target was applied instead of refused")
	}

	got, count, err := applyReplacementEdit(content, replacementEdit{
		oldString: "func b() {\n\treturn nil\n}\n",
		newString: "func b() {\n\treturn errNotReady\n}\n",
	})
	if err != nil {
		t.Fatalf("widened target failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("replacement count = %d, want 1", count)
	}
	want := "func a() {\n\treturn nil\n}\n\nfunc b() {\n\treturn errNotReady\n}\n"
	if got != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
}

// TestReplacementEditReportsWhitespaceNearMiss proves a failed match explains
// itself when only indentation differs. It is a diagnostic, never a relaxed
// match: the edit is still refused.
func TestReplacementEditReportsWhitespaceNearMiss(t *testing.T) {
	content := "func run() {\n\treturn writeSnapshot(providers, path)\n}\n"
	_, _, err := applyReplacementEdit(content, replacementEdit{
		oldString: "    return writeSnapshot(providers, path)",
		newString: "    return writeSnapshot(providers, path, force)",
	})
	if err == nil {
		t.Fatal("whitespace-only near miss was applied instead of refused")
	}
	for _, want := range []string{"line 2", "leading or trailing whitespace"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err.Error(), want)
		}
	}
}

// TestReplacementEditErrorsAreBounded keeps a failed edit from echoing an
// unbounded model-supplied target back into the transcript.
func TestReplacementEditErrorsAreBounded(t *testing.T) {
	long := strings.Repeat("x", maxPatchMismatchSnippetBytes*4)
	_, _, err := applyReplacementEdit("alpha\n", replacementEdit{oldString: long, newString: "beta"})
	if err == nil {
		t.Fatal("expected a missing target to fail")
	}
	if strings.Contains(err.Error(), long) {
		t.Fatalf("error echoed the full unbounded target (%d bytes)", len(long))
	}
	if len(err.Error()) > maxPatchMismatchSnippetBytes*3 {
		t.Fatalf("error message not bounded: %d bytes", len(err.Error()))
	}
}

func TestParseReplacementEdit(t *testing.T) {
	tests := []struct {
		name     string
		args     map[string]any
		wantMode bool
		want     replacementEdit
	}{
		{
			name:     "patch only stays in patch mode",
			args:     map[string]any{"path": "f.go", "patch": "@@ -1,1 +1,1 @@\n-a\n+b"},
			wantMode: false,
		},
		{
			name:     "both strings select replacement mode",
			args:     map[string]any{"path": "f.go", "old_string": "a", "new_string": "b"},
			wantMode: true,
			want:     replacementEdit{oldString: "a", newString: "b"},
		},
		{
			name:     "empty new_string still selects replacement mode",
			args:     map[string]any{"path": "f.go", "old_string": "a", "new_string": ""},
			wantMode: true,
			want:     replacementEdit{oldString: "a", newString: ""},
		},
		{
			name:     "replace_all is carried through",
			args:     map[string]any{"path": "f.go", "old_string": "a", "new_string": "b", "replace_all": true},
			wantMode: true,
			want:     replacementEdit{oldString: "a", newString: "b", replaceAll: true},
		},
		{
			name:     "a partial payload is still replacement mode so it fails as one",
			args:     map[string]any{"path": "f.go", "new_string": "b"},
			wantMode: true,
			want:     replacementEdit{newString: "b"},
		},
		{
			name:     "non-boolean replace_all does not enable a blanket rewrite",
			args:     map[string]any{"path": "f.go", "old_string": "a", "new_string": "b", "replace_all": "yes"},
			wantMode: true,
			want:     replacementEdit{oldString: "a", newString: "b"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			edit, mode := parseReplacementEdit(tt.args)
			if mode != tt.wantMode {
				t.Fatalf("replacement mode = %v, want %v", mode, tt.wantMode)
			}
			if mode && edit != tt.want {
				t.Fatalf("edit = %+v, want %+v", edit, tt.want)
			}
		})
	}
}

// TestPreflightEditArguments pins what may reach dispatch. Both modes are
// admitted; a call that names neither, or both, is refused with a message
// that names the reliable path.
func TestPreflightEditArguments(t *testing.T) {
	tests := []struct {
		name     string
		args     map[string]any
		wantErr  string
		wantPass bool
	}{
		{
			name:     "replacement payload is admitted",
			args:     map[string]any{"old_string": "a", "new_string": "b"},
			wantPass: true,
		},
		{
			name:     "deletion payload is admitted",
			args:     map[string]any{"old_string": "a\n", "new_string": ""},
			wantPass: true,
		},
		{
			name:     "patch payload is still admitted",
			args:     map[string]any{"patch": "@@ -1,1 +1,1 @@\n-a\n+b"},
			wantPass: true,
		},
		{
			name:    "neither mode is refused with instructions",
			args:    map[string]any{},
			wantErr: "old_string and new_string",
		},
		{
			name:    "both modes at once are refused",
			args:    map[string]any{"old_string": "a", "new_string": "b", "patch": "@@ -1,1 +1,1 @@\n-a\n+b"},
			wantErr: "not both",
		},
		{
			name:    "missing new_string is refused",
			args:    map[string]any{"old_string": "a"},
			wantErr: "new_string is required",
		},
		{
			name:    "non-string new_string is refused",
			args:    map[string]any{"old_string": "a", "new_string": 7},
			wantErr: "new_string must be a string",
		},
		{
			name:    "non-string old_string is refused",
			args:    map[string]any{"old_string": 7, "new_string": "b"},
			wantErr: "old_string must be a string",
		},
		{
			name:    "empty old_string is refused",
			args:    map[string]any{"old_string": "", "new_string": "b"},
			wantErr: "old_string is required",
		},
		{
			name:    "empty patch is refused",
			args:    map[string]any{"patch": ""},
			wantErr: "patch is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := preflightEditArguments(tt.args)
			if tt.wantPass {
				if err != nil {
					t.Fatalf("unexpected preflight error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected a preflight error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func newEditWorkspace(t *testing.T, name, content string) (*Agent, string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	ag := New(nil, nil, 0)
	ag.SetWorkDir(root)
	t.Cleanup(ag.Close)
	return ag, path
}

func TestHandleEditReplacesExactText(t *testing.T) {
	ag, path := newEditWorkspace(t, "config.go", "package main\n\nconst mode = \"session\"\n")

	result, isErr := ag.handleEdit(map[string]any{
		"path":       "config.go",
		"old_string": "const mode = \"session\"",
		"new_string": "const mode = \"execution\"",
	})
	if isErr {
		t.Fatalf("edit failed: %s", result)
	}
	if !strings.Contains(result, "Replaced 1 occurrence") {
		t.Fatalf("result = %q, want a replacement receipt", result)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "package main\n\nconst mode = \"execution\"\n"; string(data) != want {
		t.Fatalf("file = %q, want %q", data, want)
	}
}

func TestHandleEditRefusesAmbiguousTargetWithoutTouchingTheFile(t *testing.T) {
	original := "count++\nkeep\ncount++\n"
	ag, path := newEditWorkspace(t, "counter.go", original)

	result, isErr := ag.handleEdit(map[string]any{
		"path":       "counter.go",
		"old_string": "count++",
		"new_string": "count += 2",
	})
	if !isErr {
		t.Fatalf("ambiguous edit succeeded: %s", result)
	}
	if !strings.Contains(result, "matches 2 locations") || !strings.Contains(result, "replace_all") {
		t.Fatalf("result = %q, want an ambiguity error naming the repair", result)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != original {
		t.Fatalf("refused edit still changed the file: %q", data)
	}

	result, isErr = ag.handleEdit(map[string]any{
		"path": "counter.go", "old_string": "count++", "new_string": "count += 2", "replace_all": true,
	})
	if isErr {
		t.Fatalf("replace_all edit failed: %s", result)
	}
	if !strings.Contains(result, "Replaced 2 occurrences") {
		t.Fatalf("result = %q, want a two-occurrence receipt", result)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "count += 2\nkeep\ncount += 2\n"; string(data) != want {
		t.Fatalf("file = %q, want %q", data, want)
	}
}

// TestHandleEditRejectsMalformedModeSelection keeps the two modes disjoint:
// a call may not smuggle both, and a call that names neither is told which
// one to use rather than silently doing nothing.
func TestHandleEditRejectsMalformedModeSelection(t *testing.T) {
	original := "alpha\n"
	ag, path := newEditWorkspace(t, "a.txt", original)

	for _, tt := range []struct {
		name    string
		args    map[string]any
		wantMsg string
	}{
		{
			name:    "both modes",
			args:    map[string]any{"path": "a.txt", "old_string": "alpha", "new_string": "beta", "patch": "@@ -1,1 +1,1 @@\n-alpha\n+beta"},
			wantMsg: "not both",
		},
		{
			name:    "neither mode",
			args:    map[string]any{"path": "a.txt"},
			wantMsg: "old_string and new_string are required",
		},
		{
			name:    "missing path",
			args:    map[string]any{"old_string": "alpha", "new_string": "beta"},
			wantMsg: "path is required",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result, isErr := ag.handleEdit(tt.args)
			if !isErr {
				t.Fatalf("malformed edit succeeded: %s", result)
			}
			if !strings.Contains(result, tt.wantMsg) {
				t.Fatalf("result = %q, want %q", result, tt.wantMsg)
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != original {
				t.Fatalf("file changed by a refused edit: %q err=%v", data, err)
			}
		})
	}
}

// TestHandleEditPatchModeStillApplies is the regression guard for existing
// callers: the unified-diff path keeps its behavior and its receipt.
func TestHandleEditPatchModeStillApplies(t *testing.T) {
	ag, path := newEditWorkspace(t, "b.txt", "one\ntwo\nthree\n")

	result, isErr := ag.handleEdit(map[string]any{
		"path": "b.txt", "patch": "@@ -2,1 +2,1 @@\n-two\n+TWO",
	})
	if isErr {
		t.Fatalf("patch edit failed: %s", result)
	}
	if !strings.HasPrefix(result, "Applied patch to ") || !strings.Contains(result, "bytes)") {
		t.Fatalf("result = %q, want the unchanged patch receipt", result)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "one\nTWO\nthree\n"; string(data) != want {
		t.Fatalf("file = %q, want %q", data, want)
	}
}

// TestHandleEditReplacementHonorsWorkspaceContainment proves the new mode
// inherits the same containment as the patch mode: it cannot reach a file
// outside the workspace, and the outside file is left untouched.
func TestHandleEditReplacementHonorsWorkspaceContainment(t *testing.T) {
	ag, _ := newEditWorkspace(t, "inside.txt", "inside\n")
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outside, []byte("canonical\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, target := range []string{outside, "../" + filepath.Base(outsideDir) + "/secret.txt"} {
		result, isErr := ag.handleEdit(map[string]any{
			"path": target, "old_string": "canonical", "new_string": "mutated",
		})
		if !isErr {
			t.Fatalf("edit escaped the workspace: %s", result)
		}
		if data, err := os.ReadFile(outside); err != nil || string(data) != "canonical\n" {
			t.Fatalf("outside file changed: %q err=%v", data, err)
		}
	}
}

// TestHandleEditPreservesFileMode keeps the atomic-write contract: an edited
// file keeps its permissions rather than adopting a fresh default.
func TestHandleEditPreservesFileMode(t *testing.T) {
	ag, path := newEditWorkspace(t, "script.sh", "#!/bin/sh\necho old\n")
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if result, isErr := ag.handleEdit(map[string]any{
		"path": "script.sh", "old_string": "echo old", "new_string": "echo new",
	}); isErr {
		t.Fatalf("edit failed: %s", result)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v, want 0755", info.Mode().Perm())
	}
}

// TestReplacementEditApprovalPreviewIsDerivedAndStable checks the approval
// contract for the new mode: the same preview kind as a patch edit, hashes
// bound to the exact before/after content, a diff derived from the
// replacement, and byte-identical output on rebuild so revalidation cannot
// spuriously invalidate an open request.
func TestReplacementEditApprovalPreviewIsDerivedAndStable(t *testing.T) {
	ag, _ := newEditWorkspace(t, "preview.go", "package main\n\nconst mode = \"session\"\n")
	call := llm.ToolCall{
		ID:   "edit-1",
		Name: "edit",
		Arguments: map[string]any{
			"path":       "preview.go",
			"old_string": "\"session\"",
			"new_string": "\"execution\"",
		},
	}

	preview := ag.buildApprovalPreview(context.Background(), call, "args-hash")
	if preview.Kind != permissionpkg.PreviewFilePatch {
		t.Fatalf("preview kind = %q, want %q", preview.Kind, permissionpkg.PreviewFilePatch)
	}
	if preview.DiffOmittedReason != "" {
		t.Fatalf("diff omitted: %s", preview.DiffOmittedReason)
	}
	if !strings.Contains(preview.Diff, "-const mode = \"session\"") || !strings.Contains(preview.Diff, "+const mode = \"execution\"") {
		t.Fatalf("preview diff does not describe the replacement: %q", preview.Diff)
	}
	if preview.ExistingSHA256 == "" || preview.ContentSHA256 == "" || preview.ExistingSHA256 == preview.ContentSHA256 {
		t.Fatalf("preview hashes = existing %q content %q", preview.ExistingSHA256, preview.ContentSHA256)
	}
	if want := int64(len("package main\n\nconst mode = \"execution\"\n")); preview.ByteSize != want {
		t.Fatalf("preview byte size = %d, want %d", preview.ByteSize, want)
	}
	if again := ag.buildApprovalPreview(context.Background(), call, "args-hash"); again != preview {
		t.Fatal("preview is not stable across rebuilds; revalidation would reject an unchanged request")
	}
}

// TestReplacementEditApprovalPreviewReportsUnappliableEdit keeps an
// impossible edit visible at the approval boundary instead of presenting an
// empty diff as if nothing would change.
func TestReplacementEditApprovalPreviewReportsUnappliableEdit(t *testing.T) {
	ag, _ := newEditWorkspace(t, "preview.go", "package main\n")
	preview := ag.buildApprovalPreview(context.Background(), llm.ToolCall{
		ID:        "edit-2",
		Name:      "edit",
		Arguments: map[string]any{"path": "preview.go", "old_string": "absent", "new_string": "present"},
	}, "args-hash")
	if preview.Diff != "" {
		t.Fatalf("preview diff = %q, want none", preview.Diff)
	}
	if !strings.Contains(preview.DiffOmittedReason, "old_string was not found") {
		t.Fatalf("omitted reason = %q, want the replacement failure", preview.DiffOmittedReason)
	}
}

// TestReplacementEditRecoversRealPatchFailures reproduces the failure shapes
// recorded verbatim in the durable ledger for the unified-diff edit tool.
// Each case asserts both halves of the claim: the patch the model actually
// produced still fails (these were real, not parser bugs), and the same
// intent expressed as an exact string replacement succeeds.
func TestReplacementEditRecoversRealPatchFailures(t *testing.T) {
	// A provider snapshot file with the tab-indented return the ledger
	// quoted, and two switch arms one word apart.
	source := strings.Join([]string{
		"package catalog",
		"",
		"func refresh(providers []Provider, path string) error {",
		"\tif len(providers) == 0 {",
		"\t\treturn nil",
		"\t}",
		"\treturn writeSnapshot(providers, path)",
		"}",
		"",
		"func classify(kind string) string {",
		"\tswitch kind {",
		"\tcase \"execution\":",
		"\t\treturn \"exec\"",
		"\tcase \"session\":",
		"\t\treturn \"sess\"",
		"\t}",
		"\treturn \"\"",
		"}",
		"",
	}, "\n")

	tests := []struct {
		name       string
		patch      string
		wantErr    string
		edit       replacementEdit
		wantInFile string
	}{
		{
			// "error applying patch: patch contains no hunks" (4x, the
			// dominant failure): the model emitted a diff body with no
			// @@ header at all, so nothing was applied.
			name:    "patch contains no hunks",
			patch:   "-\treturn writeSnapshot(providers, path)\n+\treturn writeSnapshot(providers, path, force)",
			wantErr: "patch contains no hunks",
			edit: replacementEdit{
				oldString: "\treturn writeSnapshot(providers, path)\n",
				newString: "\treturn writeSnapshot(providers, path, force)\n",
			},
			wantInFile: "\treturn writeSnapshot(providers, path, force)\n",
		},
		{
			// patch context mismatch at old line 83: expected
			// "\treturn writeSnapshot(providers, path)", found <empty line>.
			// The model counted from a stale copy of the file, so its
			// hunk landed on a blank line.
			name:    "stale line number lands on a blank line",
			patch:   "@@ -9,2 +9,2 @@\n \treturn writeSnapshot(providers, path)\n-}\n+} // closed",
			wantErr: "patch context mismatch at old line 9: expected \"\\treturn writeSnapshot(providers, path)\", found <empty line>",
			edit: replacementEdit{
				oldString: "\treturn writeSnapshot(providers, path)\n",
				newString: "\treturn writeSnapshot(providers, path, force)\n",
			},
			wantInFile: "\treturn writeSnapshot(providers, path, force)\n",
		},
		{
			// patch context mismatch at old line 61: expected
			// "\t\tcase \"session\":", found "\t\tcase \"execution\":".
			// Two near-identical switch arms; the line number picked the
			// wrong one. The string target names the arm unambiguously.
			name:    "stale line number selects the wrong switch arm",
			patch:   "@@ -12,2 +12,2 @@\n \tcase \"session\":\n-\t\treturn \"sess\"\n+\t\treturn \"session\"",
			wantErr: "patch context mismatch",
			edit: replacementEdit{
				oldString: "\tcase \"session\":\n\t\treturn \"sess\"\n",
				newString: "\tcase \"session\":\n\t\treturn \"session\"\n",
			},
			wantInFile: "\tcase \"session\":\n\t\treturn \"session\"\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := applyPatch(source, tt.patch); err == nil {
				t.Fatal("the recorded patch unexpectedly applied; the failure shape is no longer reproduced")
			} else if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("patch error = %q, want %q", err.Error(), tt.wantErr)
			}

			got, count, err := applyReplacementEdit(source, tt.edit)
			if err != nil {
				t.Fatalf("string replacement failed where it should succeed: %v", err)
			}
			if count != 1 {
				t.Fatalf("replacement count = %d, want 1", count)
			}
			if !strings.Contains(got, tt.wantInFile) {
				t.Fatalf("result does not contain %q:\n%s", tt.wantInFile, got)
			}
		})
	}
}

// TestReplacementEditSurvivesItsOwnEarlierEdit is the line-number failure
// mode at its root: after one edit shifts the file, a second replacement
// still targets text rather than positions, so it needs no re-read to stay
// correct.
func TestReplacementEditSurvivesItsOwnEarlierEdit(t *testing.T) {
	ag, path := newEditWorkspace(t, "seq.go", "package main\n\nfunc a() {}\n\nfunc b() {}\n")

	if result, isErr := ag.handleEdit(map[string]any{
		"path":       "seq.go",
		"old_string": "func a() {}\n",
		"new_string": "func a() {\n\t// first edit inserts lines\n\treturn\n}\n",
	}); isErr {
		t.Fatalf("first edit failed: %s", result)
	}
	// The second edit was planned against the original file. A patch would
	// now be four lines stale; the exact target is unaffected.
	if result, isErr := ag.handleEdit(map[string]any{
		"path":       "seq.go",
		"old_string": "func b() {}\n",
		"new_string": "func b() { return }\n",
	}); isErr {
		t.Fatalf("second edit failed after the file shifted: %s", result)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "package main\n\nfunc a() {\n\t// first edit inserts lines\n\treturn\n}\n\nfunc b() { return }\n"
	if string(data) != want {
		t.Fatalf("file = %q, want %q", data, want)
	}
}

// TestNoHunksErrorNamesTheReliablePath keeps the dominant patch failure from
// being a dead end: the model that emitted an unparseable patch is told, in
// the error it reads, which arguments to use instead.
func TestNoHunksErrorNamesTheReliablePath(t *testing.T) {
	_, err := applyPatch("alpha\n", "just some prose, not a diff")
	if err == nil {
		t.Fatal("expected a header-less patch to fail")
	}
	for _, want := range []string{"patch contains no hunks", "old_string", "new_string"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err.Error(), want)
		}
	}
}
