package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func searchCoverageAgent(t *testing.T) (*Agent, string) {
	t.Helper()
	workspace := t.TempDir()
	workspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	ag := New(nil, nil, 0)
	ag.SetWorkDir(workspace)
	t.Cleanup(ag.Close)
	return ag, workspace
}

// A file over the read ceiling is skipped silently. Without a coverage note the
// model is handed "No matches found" as proof the symbol does not exist, and
// the loop records the call as a completed execution.
func TestGrepQualifiesResultsWhenAFileWasTooLargeToRead(t *testing.T) {
	ag, workspace := searchCoverageAgent(t)

	oversized := filepath.Join(workspace, "bundle.min.js")
	body := append([]byte("NEEDLE_IN_A_HUGE_FILE\n"), make([]byte, maxFileReadBytes+1024)...)
	if err := os.WriteFile(oversized, body, 0o644); err != nil {
		t.Fatal(err)
	}

	result, isErr := ag.handleGrep(context.Background(), map[string]any{
		"path": workspace, "pattern": "NEEDLE_IN_A_HUGE_FILE",
	})
	if isErr {
		t.Fatalf("grep reported an error: %q", result)
	}
	if !strings.Contains(result, "not proof of absence") {
		t.Fatalf("a negative built from a partial scan was left unqualified:\n%s", result)
	}
	if !strings.Contains(result, "read limit") {
		t.Fatalf("coverage note does not say why the file was skipped:\n%s", result)
	}
}

// A malformed include glob made filepath.Match fail for every file, and the
// error shared a branch with "did not match" — so every search returned a
// confident zero-result instead of reporting the bad pattern.
func TestGrepRejectsAMalformedIncludePatternInsteadOfReturningNoMatches(t *testing.T) {
	ag, workspace := searchCoverageAgent(t)
	if err := os.WriteFile(filepath.Join(workspace, "a.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, isErr := ag.handleGrep(context.Background(), map[string]any{
		"path": workspace, "pattern": "package", "include": "[abc",
	})
	if !isErr {
		t.Fatalf("malformed include was accepted and reported as a result: %q", result)
	}
	if !strings.Contains(result, "include pattern") {
		t.Fatalf("error does not name the offending argument: %q", result)
	}
}

// A complete search must stay clean: the note is a qualifier, not decoration.
func TestSearchesStaySilentWhenCoverageIsComplete(t *testing.T) {
	ag, workspace := searchCoverageAgent(t)
	if err := os.WriteFile(filepath.Join(workspace, "a.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for name, run := range map[string]func() (string, bool){
		"grep hit": func() (string, bool) {
			return ag.handleGrep(context.Background(), map[string]any{"path": workspace, "pattern": "package"})
		},
		"grep miss": func() (string, bool) {
			return ag.handleGrep(context.Background(), map[string]any{"path": workspace, "pattern": "zzz-absent"})
		},
		"glob": func() (string, bool) {
			return ag.handleGlob(context.Background(), map[string]any{"path": workspace, "pattern": "*.go"})
		},
		"find": func() (string, bool) {
			return ag.handleFind(context.Background(), map[string]any{"path": workspace, "name": "*.go"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			result, isErr := run()
			if isErr {
				t.Fatalf("unexpected error: %q", result)
			}
			if strings.Contains(result, "not proof of absence") {
				t.Fatalf("a fully covered search was qualified anyway:\n%s", result)
			}
		})
	}
}
