package runtimepref

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abdul-hamid-achik/sonar/internal/safeio"
)

func TestManualModelRoundTripAndClear(t *testing.T) {
	path := filepath.Join(t.TempDir(), "preferences.json")
	store := NewStore(path)

	if model, ok, err := store.LoadManualModel(); err != nil || ok || model != "" {
		t.Fatalf("empty load model=%q ok=%v err=%v", model, ok, err)
	}
	if err := store.SetManualModel("  qwen3.5:4b  "); err != nil {
		t.Fatal(err)
	}
	if model, ok, err := NewStore(path).LoadManualModel(); err != nil || !ok || model != "qwen3.5:4b" {
		t.Fatalf("round trip model=%q ok=%v err=%v", model, ok, err)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("preference mode=%04o, want 0600", got)
	}
	if info, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	} else if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("preference dir mode=%04o, want 0700", got)
	}

	if err := store.ClearManualModel(); err != nil {
		t.Fatal(err)
	}
	if model, ok, err := NewStore(path).LoadManualModel(); err != nil || ok || model != "" {
		t.Fatalf("cleared load model=%q ok=%v err=%v", model, ok, err)
	}
}

func TestManualProviderPreservesModelPreference(t *testing.T) {
	path := filepath.Join(t.TempDir(), "preferences.json")
	store := NewStore(path)
	if err := store.SetManualModel("qwen3.5:2b"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetManualProvider("xai"); err != nil {
		t.Fatal(err)
	}
	if model, ok, err := NewStore(path).LoadManualModel(); err != nil || !ok || model != "qwen3.5:2b" {
		t.Fatalf("model after provider set = %q ok=%v err=%v", model, ok, err)
	}
	if provider, ok, err := NewStore(path).LoadManualProvider(); err != nil || !ok || provider != "xai" {
		t.Fatalf("provider = %q ok=%v err=%v", provider, ok, err)
	}
	if err := store.ClearManualModel(); err != nil {
		t.Fatal(err)
	}
	if provider, ok, err := NewStore(path).LoadManualProvider(); err != nil || !ok || provider != "xai" {
		t.Fatalf("provider after model clear = %q ok=%v err=%v", provider, ok, err)
	}
}

func TestMissingPreferenceDirectoryLoadsAsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-created", "preferences.json")
	model, ok, err := NewStore(path).LoadManualModel()
	if err != nil || ok || model != "" {
		t.Fatalf("missing directory model=%q ok=%v err=%v", model, ok, err)
	}
}

func TestManualModelPreferenceIsBoundedAndFailClosed(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "preferences.json"))
	for _, model := range []string{"", "bad\nmodel", strings.Repeat("m", maxPreferredModelBytes+1)} {
		if err := store.SetManualModel(model); err == nil {
			t.Fatalf("invalid model %q accepted", model)
		}
	}

	path := filepath.Join(t.TempDir(), "preferences.json")
	if err := os.WriteFile(path, []byte(`{"version":2,"manual_model":"qwen"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := NewStore(path).LoadManualModel(); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("future schema err=%v", err)
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"manual_model":"qwen","extra":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := NewStore(path).LoadManualModel(); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestManualModelPreferenceRejectsSymlinkDestination(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "preferences.json")
	if err := os.Symlink(victim, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	store := NewStore(path)
	if err := store.SetManualModel("qwen"); err == nil || !errors.Is(err, safeio.ErrSymlink) {
		t.Fatalf("symlink write err=%v", err)
	}
	if got, err := os.ReadFile(victim); err != nil || string(got) != "keep" {
		t.Fatalf("victim=%q err=%v", got, err)
	}
}
