package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestConfigFileCandidatesPrecedenceAndDeduplication(t *testing.T) {
	home := t.TempDir()
	xdg := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)

	want := []string{
		"sonar.yaml",
		"sonar.yml",
		filepath.Join(xdg, "sonar", "config.yaml"),
		filepath.Join(xdg, "sonar", "config.yml"),
		filepath.Join(home, ".config", "sonar", "config.yaml"),
		filepath.Join(home, ".config", "sonar", "config.yml"),
	}
	if got := configFileCandidates(); !reflect.DeepEqual(got, want) {
		t.Fatalf("config candidates = %#v, want %#v", got, want)
	}

	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	want = []string{
		"sonar.yaml",
		"sonar.yml",
		filepath.Join(home, ".config", "sonar", "config.yaml"),
		filepath.Join(home, ".config", "sonar", "config.yml"),
	}
	if got := configFileCandidates(); !reflect.DeepEqual(got, want) {
		t.Fatalf("deduplicated config candidates = %#v, want %#v", got, want)
	}
}

func TestConfigFileCandidatesIgnoreRelativeXDGHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", "relative-config")

	want := []string{
		"sonar.yaml",
		"sonar.yml",
		filepath.Join(home, ".config", "sonar", "config.yaml"),
		filepath.Join(home, ".config", "sonar", "config.yml"),
	}
	if got := configFileCandidates(); !reflect.DeepEqual(got, want) {
		t.Fatalf("relative-XDG config candidates = %#v, want %#v", got, want)
	}
}

func TestFindAndReadConfigFilePrecedence(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	xdg := t.TempDir()
	oldWorkDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workspace); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWorkDir) })
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)

	homePath := filepath.Join(home, ".config", "sonar", "config.yaml")
	xdgPath := filepath.Join(xdg, "sonar", "config.yml")
	writeConfigFixture(t, homePath, "home")
	writeConfigFixture(t, xdgPath, "xdg")

	path, data, err := findAndReadConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if path != xdgPath || string(data) != "xdg" {
		t.Fatalf("XDG selection path=%q data=%q, want path=%q data=%q", path, data, xdgPath, "xdg")
	}

	localPath := filepath.Join(workspace, "sonar.yml")
	writeConfigFixture(t, localPath, "local")
	path, data, err = findAndReadConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if path != "sonar.yml" || string(data) != "local" {
		t.Fatalf("local selection path=%q data=%q, want path=%q data=%q", path, data, "sonar.yml", "local")
	}
}

func TestFindAndReadConfigFileFallsBackToHomeConfig(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	oldWorkDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workspace); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWorkDir) })
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	homePath := filepath.Join(home, ".config", "sonar", "config.yml")
	writeConfigFixture(t, homePath, "home")
	path, data, err := findAndReadConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if path != homePath || string(data) != "home" {
		t.Fatalf("home fallback path=%q data=%q, want path=%q data=%q", path, data, homePath, "home")
	}
}

func TestLoadRecordsAbsoluteSelectedConfigPathWithoutSerializingIt(t *testing.T) {
	workspace := t.TempDir()
	oldWorkDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workspace); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWorkDir) })
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	path := filepath.Join(workspace, "sonar.yaml")
	writeConfigFixture(t, path, "privacy:\n  local_only: false\n")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	wantPath, err := filepath.Abs("sonar.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SourcePath != wantPath {
		t.Fatalf("SourcePath = %q, want %q", cfg.SourcePath, wantPath)
	}
	encoded, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "sourcepath") || strings.Contains(string(encoded), cfg.SourcePath) {
		t.Fatalf("runtime source path was serialized: %s", encoded)
	}
	encoded, err = json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "SourcePath") || strings.Contains(string(encoded), cfg.SourcePath) {
		t.Fatalf("runtime source path was JSON serialized: %s", encoded)
	}
}

func TestRepositoryConfigRequiresDigestBoundTrustForSTDIO(t *testing.T) {
	workspace := enterIsolatedConfigWorkspace(t)
	executable := filepath.Join(workspace, "bin", "repo-mcp")
	writeExecutableFixture(t, executable, "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", filepath.Dir(executable))
	path := filepath.Join(workspace, "sonar.yaml")
	writeConfigFixture(t, path, `servers:
  - name: repo-tools
    command: repo-mcp
    args: [serve]
    env: [MODE=read-only]
`)

	_, err := Load()
	var trustErr *RepoMCPTrustError
	if !errors.As(err, &trustErr) {
		t.Fatalf("Load error = %v, want RepoMCPTrustError", err)
	}
	wantSourcePath, absErr := filepath.Abs("sonar.yaml")
	if absErr != nil {
		t.Fatal(absErr)
	}
	if trustErr.SourcePath != wantSourcePath || trustErr.ServerCount != 1 {
		t.Fatalf("trust error = %#v, want source %q and one server", trustErr, wantSourcePath)
	}
	if !strings.HasPrefix(trustErr.Digest, "sha256:") {
		t.Fatalf("trust digest = %q, want sha256 digest", trustErr.Digest)
	}

	t.Setenv(repoMCPTrustEnv, trustErr.Digest)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load with exact repository MCP trust: %v", err)
	}
	pinnedExecutable, err := filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Servers) != 1 || cfg.Servers[0].Command != pinnedExecutable || cfg.Servers[0].ExecutableSHA256 == "" {
		t.Fatalf("trusted servers = %#v, want pinned command %q", cfg.Servers, pinnedExecutable)
	}

	writeExecutableFixture(t, executable, "#!/bin/sh\n# changed after approval\nexit 0\n")
	_, err = Load()
	var executableChangedErr *RepoMCPTrustError
	if !errors.As(err, &executableChangedErr) {
		t.Fatalf("Load after executable change error = %v, want RepoMCPTrustError", err)
	}
	if executableChangedErr.Digest == trustErr.Digest {
		t.Fatalf("changed executable content retained digest %q", executableChangedErr.Digest)
	}
	t.Setenv(repoMCPTrustEnv, executableChangedErr.Digest)

	writeConfigFixture(t, path, `servers:
  - name: repo-tools
    command: repo-mcp
    args: [serve, --write]
    env: [MODE=read-write]
`)
	_, err = Load()
	var changedErr *RepoMCPTrustError
	if !errors.As(err, &changedErr) {
		t.Fatalf("Load after authority change error = %v, want RepoMCPTrustError", err)
	}
	if changedErr.Digest == executableChangedErr.Digest {
		t.Fatalf("changed executable authority retained digest %q", changedErr.Digest)
	}
}

func TestRepositoryMCPDigestBindsCanonicalEffectiveTrust(t *testing.T) {
	workspace := enterIsolatedConfigWorkspace(t)
	executable := filepath.Join(workspace, "bin", "mcphub")
	writeExecutableFixture(t, executable, "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", filepath.Dir(executable))
	path := filepath.Join(workspace, "sonar.yaml")
	write := func(routes string) {
		writeConfigFixture(t, path, `servers:
  - name: gateway
    command: mcphub
    trust:
      local_owner: mcphub
      gateway: mcphub
      read_only:
`+routes)
	}
	write("        - mcphub_stats\n        - mcphub_list_servers\n")

	_, err := Load()
	var initial *RepoMCPTrustError
	if !errors.As(err, &initial) {
		t.Fatalf("initial Load error = %v, want RepoMCPTrustError", err)
	}

	// Contract order is not authority and therefore must not churn consent.
	write("        - mcphub_list_servers\n        - mcphub_stats\n")
	t.Setenv(repoMCPTrustEnv, initial.Digest)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("reordered equivalent trust changed digest: %v", err)
	}
	resolved, err := ResolveMCPTrust(cfg.Servers[0])
	if err != nil || resolved == nil || strings.Join(resolved.ReadOnly, ",") != "mcphub_list_servers,mcphub_stats" {
		t.Fatalf("resolved canonical trust = %#v, %v", resolved, err)
	}

	write("        - bob__bob_plan\n        - mcphub_list_servers\n        - mcphub_stats\n")
	_, err = Load()
	var changed *RepoMCPTrustError
	if !errors.As(err, &changed) {
		t.Fatalf("changed trust Load error = %v, want RepoMCPTrustError", err)
	}
	if changed.Digest == initial.Digest {
		t.Fatalf("widened MCP trust retained digest %q", changed.Digest)
	}
}

func TestRepositoryConfigAllowsNonExecutableMCPWithoutTrust(t *testing.T) {
	workspace := enterIsolatedConfigWorkspace(t)
	writeConfigFixture(t, filepath.Join(workspace, "sonar.yml"), `servers:
  - name: local-http
    transport: streamable-http
    url: http://127.0.0.1:8812/mcp
`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load repository-local HTTP MCP server: %v", err)
	}
	if len(cfg.Servers) != 1 || cfg.Servers[0].Transport != "streamable-http" {
		t.Fatalf("HTTP servers = %#v, want one streamable-http server", cfg.Servers)
	}
}

func TestUserConfigSTDIOBehaviorDoesNotRequireRepositoryTrust(t *testing.T) {
	workspace := enterIsolatedConfigWorkspace(t)
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	path := filepath.Join(xdg, "sonar", "config.yaml")
	writeConfigFixture(t, path, `servers:
  - name: user-tools
    command: user-mcp
    args: [serve]
`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load user-wide STDIO MCP config: %v", err)
	}
	if cfg.SourcePath != path || len(cfg.Servers) != 1 || cfg.Servers[0].Command != "user-mcp" {
		t.Fatalf("workspace=%q config = %#v, want trusted user config %q", workspace, cfg, path)
	}
}

func TestRepositorySelectedAgentsDirSTDIORequiresTrust(t *testing.T) {
	workspace := enterIsolatedConfigWorkspace(t)
	executable := filepath.Join(workspace, "bin", "repo-agents-mcp")
	writeExecutableFixture(t, executable, "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", filepath.Dir(executable))
	agentsRoot := filepath.Join(workspace, "repo-agents")
	writeConfigFixture(t, filepath.Join(agentsRoot, "mcp.json"), `{
  "servers": [{"name":"repo-agents-tools","command":"repo-agents-mcp","args":["serve"]}]
}`)
	writeConfigFixture(t, filepath.Join(workspace, "sonar.yaml"), fmt.Sprintf("agents:\n  dir: %q\n  auto_load: true\n", agentsRoot))

	_, err := Load()
	var trustErr *RepoMCPTrustError
	if !errors.As(err, &trustErr) {
		t.Fatalf("Load repository-selected agents root error = %v, want RepoMCPTrustError", err)
	}
	if trustErr.ServerCount != 1 {
		t.Fatalf("repository-selected agents trust count = %d, want 1", trustErr.ServerCount)
	}
}

func TestDefaultUserAgentsSTDIOBehaviorDoesNotRequireRepositoryTrust(t *testing.T) {
	workspace := enterIsolatedConfigWorkspace(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	writeConfigFixture(t, filepath.Join(home, ".agents", "mcp.json"), `{
  "servers": [{"name":"user-agents-tools","command":"user-agents-mcp","args":["serve"]}]
}`)
	writeConfigFixture(t, filepath.Join(workspace, "sonar.yaml"), "privacy:\n  local_only: true\n")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load default user agents STDIO config: %v", err)
	}
	if len(cfg.Servers) != 1 || cfg.Servers[0].Command != "user-agents-mcp" {
		t.Fatalf("default user agents servers = %#v, want user-agents-mcp", cfg.Servers)
	}
}

func TestEnvironmentSelectedAgentsSTDIOBehaviorDoesNotRequireRepositoryTrust(t *testing.T) {
	workspace := enterIsolatedConfigWorkspace(t)
	environmentRoot := t.TempDir()
	writeConfigFixture(t, filepath.Join(environmentRoot, "mcp.json"), `{
  "servers": [{"name":"environment-tools","command":"environment-mcp","args":["serve"]}]
}`)
	writeConfigFixture(t, filepath.Join(workspace, "sonar.yaml"), "agents:\n  dir: ./repository-agents\n  auto_load: true\n")
	t.Setenv("SONAR_AGENTS_DIR", environmentRoot)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load environment-selected agents STDIO config: %v", err)
	}
	if len(cfg.Servers) != 1 || cfg.Servers[0].Command != "environment-mcp" {
		t.Fatalf("environment-selected agents servers = %#v, want environment-mcp", cfg.Servers)
	}
}

func enterIsolatedConfigWorkspace(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()
	home := t.TempDir()
	oldWorkDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workspace); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWorkDir) })
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv(repoMCPTrustEnv, "")
	return workspace
}

func writeConfigFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeExecutableFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
}
