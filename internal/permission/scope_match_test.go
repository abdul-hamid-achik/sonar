package permission

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDeriveBashPrefix(t *testing.T) {
	tests := []struct {
		command string
		want    string
		ok      bool
	}{
		{command: "go test ./...", want: "go test", ok: true},
		{command: "npm run build", want: "npm run", ok: true},
		{command: "ls -la", want: "ls", ok: true},
		{command: "true", want: "true", ok: true},
		{command: "go test ./... && rm -rf /", want: "", ok: false},
		{command: "echo $HOME", want: "", ok: false},
		{command: "", want: "", ok: false},
	}
	for _, tt := range tests {
		got, ok := DeriveBashPrefix(tt.command)
		if ok != tt.ok || got != tt.want {
			t.Fatalf("DeriveBashPrefix(%q) = %q, %v; want %q, %v", tt.command, got, ok, tt.want, tt.ok)
		}
	}
}

func TestBashPrefixMatches(t *testing.T) {
	if !BashPrefixMatches("go test ./internal/agent", "go test") {
		t.Fatal("expected prefix match")
	}
	if !BashPrefixMatches("go test", "go test") {
		t.Fatal("expected exact match")
	}
	if BashPrefixMatches("gotest ./x", "go test") {
		t.Fatal("should require word boundary via space")
	}
	if BashPrefixMatches("go test ./x && true", "go test") {
		t.Fatal("compound commands must not match")
	}
}

func TestBashPatternMatchesTrailingGlob(t *testing.T) {
	if !BashPatternMatches("git status", "git status *") {
		t.Fatal("exact head should match trailing glob")
	}
	if !BashPatternMatches("git status -sb", "git status *") {
		t.Fatal("args should match trailing glob")
	}
	if BashPatternMatches("git log", "git status *") {
		t.Fatal("different subcommand must not match")
	}
	if BashPatternMatches("git status && rm -rf /", "git status *") {
		t.Fatal("compound must not match")
	}
	if !BashPatternMatches("go test ./...", "go test") {
		t.Fatal("literal prefix via pattern matcher")
	}
	if _, ok := NormalizeBashPattern("*"); ok {
		t.Fatal("bare * rejected")
	}
	if _, ok := NormalizeBashPattern("* status"); ok {
		t.Fatal("leading * rejected")
	}
	if _, ok := NormalizeBashPattern("git * status"); ok {
		t.Fatal("mid * rejected")
	}
	if got, ok := NormalizeBashPattern("git status *"); !ok || got != "git status *" {
		t.Fatalf("normalize = %q, %v", got, ok)
	}
}

func TestNormalizeMCPToolName(t *testing.T) {
	if name, ok := NormalizeMCPToolName("mcphub__mcphub_list_servers"); !ok || name != "mcphub__mcphub_list_servers" {
		t.Fatalf("got %q, %v", name, ok)
	}
	if _, ok := NormalizeMCPToolName("mcphub_list_servers"); ok {
		t.Fatal("bare names must fail")
	}
}

func TestNormalizeWritePathAndMatch(t *testing.T) {
	ws := t.TempDir()
	dir := filepath.Join(ws, "src")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "main.go")
	if err := os.WriteFile(target, []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rel, ok := NormalizeWritePath(ws, target)
	if !ok || rel != "src/main.go" {
		t.Fatalf("rel = %q, %v", rel, ok)
	}
	if !WritePathMatches(ws, target, "src/main.go") {
		t.Fatal("expected path match")
	}
	if WritePathMatches(ws, filepath.Join(ws, "other.go"), "src/main.go") {
		t.Fatal("different path must not match")
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if _, ok := NormalizeWritePath(ws, outside); ok {
		t.Fatal("outside workspace rejected")
	}
}
