package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/config"
)

func TestResolveICEStorePathConfinesConfiguredPath(t *testing.T) {
	home := useICEStoreHome(t)
	workspace := t.TempDir()

	got, err := resolveICEStorePath(workspace, "conversations.json")
	if err != nil {
		t.Fatal(err)
	}
	canonicalWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	workspaceHash := sha256.Sum256([]byte(canonicalWorkspace))
	want := filepath.Join(home, filepath.FromSlash(managedICEStoreDir), fmt.Sprintf("%x", workspaceHash[:8]), "conversations.json")
	if got != want {
		t.Fatalf("resolved path = %q, want %q", got, want)
	}
	if got, err := resolveICEStorePath(workspace, ""); err != nil || got != "" {
		t.Fatalf("managed default = %q, %v", got, err)
	}

	absolute := filepath.Join(t.TempDir(), "outside.json")
	if _, err := resolveICEStorePath(workspace, absolute); err == nil || !strings.Contains(err.Error(), "relative") {
		t.Fatalf("absolute path error = %v", err)
	}
	if _, err := resolveICEStorePath(workspace, filepath.Join("..", "..", "outside.json")); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("traversal path error = %v", err)
	}
}

func TestResolvedICEEngineConfigNeverReusesRawStorePath(t *testing.T) {
	cfg := &config.Config{
		ICE:    config.ICEConfig{EmbedModel: "embed", StorePath: "/raw/outside.json"},
		Ollama: config.OllamaConfig{NumCtx: 4096},
	}
	resolved := filepath.Join(t.TempDir(), ".sonar", "ice", "store.json")
	engineCfg := resolvedICEEngineConfig(cfg, "/workspace", resolved)
	if engineCfg.StorePath != resolved || engineCfg.StorePath == cfg.ICE.StorePath {
		t.Fatalf("engine store path = %q, raw config = %q", engineCfg.StorePath, cfg.ICE.StorePath)
	}
}

func TestResolveICEStorePathRejectsSymlinkParent(t *testing.T) {
	home := useICEStoreHome(t)
	workspace := t.TempDir()
	outside := t.TempDir()
	canonicalWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	workspaceHash := sha256.Sum256([]byte(canonicalWorkspace))
	managedRoot := filepath.Join(home, filepath.FromSlash(managedICEStoreDir), fmt.Sprintf("%x", workspaceHash[:8]))
	if err := os.MkdirAll(managedRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(managedRoot, "history")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := resolveICEStorePath(workspace, filepath.Join("history", "conversations.json")); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink-parent error = %v", err)
	}
}

func TestResolveICEStorePathAllowsExistingRegularFile(t *testing.T) {
	home := useICEStoreHome(t)
	workspace := t.TempDir()
	canonicalWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	workspaceHash := sha256.Sum256([]byte(canonicalWorkspace))
	managedRoot := filepath.Join(home, filepath.FromSlash(managedICEStoreDir), fmt.Sprintf("%x", workspaceHash[:8]))
	if err := os.MkdirAll(managedRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(managedRoot, "conversations.json")
	if err := os.WriteFile(path, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := resolveICEStorePath(workspace, "conversations.json")
	if err != nil || got != filepath.Join(managedRoot, "conversations.json") {
		t.Fatalf("existing regular path = %q, %v", got, err)
	}
}

func TestResolveICEStorePathUsesStableCanonicalWorkspaceHash(t *testing.T) {
	useICEStoreHome(t)
	workspace := t.TempDir()
	link := filepath.Join(t.TempDir(), "workspace-link")
	if err := os.Symlink(workspace, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	direct, err := resolveICEStorePath(workspace, "store.json")
	if err != nil {
		t.Fatal(err)
	}
	throughLink, err := resolveICEStorePath(link, "store.json")
	if err != nil {
		t.Fatal(err)
	}
	if direct != throughLink {
		t.Fatalf("canonical workspace paths differ: direct=%q linked=%q", direct, throughLink)
	}

	other, err := resolveICEStorePath(t.TempDir(), "store.json")
	if err != nil {
		t.Fatal(err)
	}
	if other == direct {
		t.Fatalf("different workspaces shared managed store path %q", direct)
	}
}

func useICEStoreHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	canonicalHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	previous := iceStoreHomeDir
	iceStoreHomeDir = func() (string, error) { return home, nil }
	t.Cleanup(func() { iceStoreHomeDir = previous })
	return canonicalHome
}
