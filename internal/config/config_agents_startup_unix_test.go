//go:build !windows

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigLoadFailsClosedForInvalidSharedSkillMetadata(t *testing.T) {
	for _, test := range []struct {
		name  string
		build func(*testing.T, string)
	}{
		{
			name: "symlink",
			build: func(t *testing.T, skillPath string) {
				t.Helper()
				target := filepath.Join(t.TempDir(), "target.md")
				if err := os.WriteFile(target, []byte("private"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, skillPath); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "oversize",
			build: func(t *testing.T, skillPath string) {
				t.Helper()
				file, err := os.OpenFile(skillPath, os.O_CREATE|os.O_WRONLY, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				if err := file.Truncate(maxStartupConfigBytes + 1); err != nil {
					_ = file.Close()
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			agentsRoot := filepath.Join(t.TempDir(), ".agents")
			skillDir := filepath.Join(agentsRoot, "skills", "invalid")
			if err := os.MkdirAll(skillDir, 0o700); err != nil {
				t.Fatal(err)
			}
			test.build(t, filepath.Join(skillDir, "SKILL.md"))

			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("SONAR_AGENTS_DIR", agentsRoot)

			if _, _, err := LoadWithAgentsDir(); err == nil || !strings.Contains(err.Error(), "load skills") {
				t.Fatalf("LoadWithAgentsDir error = %v, want fail-closed skill error", err)
			}
		})
	}
}

func TestLoadWithAgentsDirUsesEnvironmentSelectedRoot(t *testing.T) {
	agentsRoot := filepath.Join(t.TempDir(), ".agents")
	if err := os.MkdirAll(agentsRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("SONAR_AGENTS_DIR", agentsRoot)

	cfg, agents, err := LoadWithAgentsDir()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Agents.Dir != agentsRoot || agents == nil || agents.Path != agentsRoot {
		t.Fatalf("selected root: config=%q agents=%#v, want %q", cfg.Agents.Dir, agents, agentsRoot)
	}
}

func TestLoadWithAgentsDirUsesCanonicalRootOnFreshInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("SONAR_AGENTS_DIR", "")

	_, agents, err := LoadWithAgentsDir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".agents")
	if agents == nil || agents.Path != want {
		t.Fatalf("agents = %#v, want root %q", agents, want)
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Fatalf("configuration load created %q: %v", want, err)
	}
}
