package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// configFileCandidates honours XDG_CONFIG_HOME; the agents directory did not.
// On a machine where XDG_CONFIG_HOME points somewhere other than ~/.config,
// sonar found its config and silently failed to find its agents and skills —
// two halves of one convention disagreeing.
func TestAgentsDirHonoursXDGConfigHome(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())

	wanted := filepath.Join(xdg, "agents")
	if err := os.MkdirAll(filepath.Join(wanted, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := FindAgentsDir(); got != wanted {
		t.Errorf("FindAgentsDir() = %q, want the XDG location %q", got, wanted)
	}
}

// ~/.agents keeps precedence: it is where existing installs live, and an
// XDG variable set for something else must not silently relocate them.
func TestHomeAgentsDirOutranksXDG(t *testing.T) {
	home := t.TempDir()
	xdg := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)

	if err := os.MkdirAll(filepath.Join(home, ".agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(xdg, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, want := FindAgentsDir(), filepath.Join(home, ".agents"); got != want {
		t.Errorf("FindAgentsDir() = %q, want %q", got, want)
	}
}

// A relative XDG_CONFIG_HOME is not a location; the config loader ignores one
// and this must agree, or the two disagree again in the other direction.
func TestRelativeXDGConfigHomeIsIgnored(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "relative/agents")

	wanted := filepath.Join(home, ".config", "agents")
	if err := os.MkdirAll(wanted, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := FindAgentsDir(); got != wanted {
		t.Errorf("FindAgentsDir() = %q, want the ~/.config fallback %q", got, wanted)
	}
}

// Creating targets ~/.agents rather than inventing a directory under a
// variable the user set for something else.
func TestCreateTargetsTheHomeLocation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	created, err := FindAgentsDirWithCreate()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".agents"); created != want {
		t.Errorf("created %q, want %q", created, want)
	}
}

// AGENTS.md is the cross-harness convention and the name `sonar init` writes.
// The loader only looked for lowercase spellings, which worked on
// case-insensitive macOS and silently ignored a correct file on Linux.
func TestGlobalInstructionsReadCanonicalAgentsFile(t *testing.T) {
	for _, name := range []string{"AGENTS.md", "agents.md", "instructions.md"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			body := "global instruction from " + name
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			loaded, err := LoadAgentsDir(dir)
			if err != nil {
				t.Fatalf("load agents dir: %v", err)
			}
			if !strings.Contains(loaded.GetGlobalInstructions(), body) {
				t.Errorf("%s was not read as global instructions: %q",
					name, loaded.GetGlobalInstructions())
			}
		})
	}
}

// The canonical spelling wins over a legacy one, so a directory migrating from
// the old name does not keep serving it.
//
// Only instructions.md is used as the loser here: on a case-insensitive
// filesystem AGENTS.md and agents.md are the same file, so writing both tests
// the filesystem rather than the precedence list — which is the very quirk
// that let the missing uppercase spelling go unnoticed on macOS.
func TestCanonicalGlobalInstructionsOutrankLegacySpellings(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "instructions.md"), []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("canonical"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadAgentsDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(loaded.GetGlobalInstructions()); got != "canonical" {
		t.Errorf("global instructions = %q, want the canonical AGENTS.md", got)
	}
}
