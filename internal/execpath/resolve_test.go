package execpath

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestResolveRejectsEmptyCommand(t *testing.T) {
	if _, err := Resolve(""); err == nil || !strings.Contains(err.Error(), "empty command") {
		t.Fatalf("Resolve(empty) error = %v", err)
	}
}

func TestResolveUsesExplicitExecutablePath(t *testing.T) {
	path := writeExecutable(t, filepath.Join(t.TempDir(), "bin", "explicit-tool"))
	t.Setenv("PATH", "")

	got, err := Resolve(path)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", path, err)
	}
	if got != path {
		t.Fatalf("resolved explicit path = %q, want %q", got, path)
	}
}

func TestResolveUsesFirstPATHExecutable(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	name := "execpath-path-order-tool"
	want := writeExecutable(t, filepath.Join(first, name))
	_ = writeExecutable(t, filepath.Join(second, name))
	t.Setenv("PATH", strings.Join([]string{first, second}, string(os.PathListSeparator)))

	got, err := Resolve(name)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", name, err)
	}
	if got != want {
		t.Fatalf("resolved PATH executable = %q, want first entry %q", got, want)
	}
}

func TestResolveFallsBackToOrderedHomeToolDirectories(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", "")
	name := "execpath-standard-dir-fallback-tool"
	localCandidate := filepath.Join(home, ".local", "bin", name)
	if err := os.MkdirAll(filepath.Dir(localCandidate), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(localCandidate, []byte("not executable"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := writeExecutable(t, filepath.Join(home, "go", "bin", name))
	_ = writeExecutable(t, filepath.Join(home, ".bun", "bin", name))

	got, err := Resolve(name)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", name, err)
	}
	if got != want {
		t.Fatalf("resolved fallback executable = %q, want %q", got, want)
	}
}

func TestResolveRejectsNonExecutableFilesAndDirectories(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", directory)
	for _, test := range []struct {
		name string
		make func(string) error
	}{
		{
			name: "non-executable",
			make: func(path string) error {
				return os.WriteFile(path, []byte("not executable"), 0o600)
			},
		},
		{
			name: "directory",
			make: func(path string) error {
				return os.Mkdir(path, 0o700)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			name := "execpath-reject-" + test.name
			if err := test.make(filepath.Join(directory, name)); err != nil {
				t.Fatal(err)
			}
			if _, err := Resolve(name); err == nil || !strings.Contains(err.Error(), "not found") {
				t.Fatalf("Resolve(%q) error = %v, want not found", name, err)
			}
		})
	}
}

func TestStandardDirsAreStableUniqueAndHomeAware(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dirs := StandardDirs()
	if len(dirs) == 0 {
		t.Fatal("StandardDirs returned no directories")
	}

	seen := make(map[string]struct{}, len(dirs))
	for _, dir := range dirs {
		if dir == "" {
			t.Fatal("StandardDirs retained an empty directory")
		}
		if _, duplicate := seen[dir]; duplicate {
			t.Fatalf("StandardDirs retained duplicate %q: %#v", dir, dirs)
		}
		seen[dir] = struct{}{}
	}
	for _, expected := range []string{
		"/opt/homebrew/bin",
		"/usr/local/bin",
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, "go", "bin"),
		filepath.Join(home, ".bun", "bin"),
	} {
		if _, ok := seen[expected]; !ok {
			t.Fatalf("StandardDirs missing %q: %#v", expected, dirs)
		}
	}
	if local, goBin, bun := indexOf(dirs, filepath.Join(home, ".local", "bin")), indexOf(dirs, filepath.Join(home, "go", "bin")), indexOf(dirs, filepath.Join(home, ".bun", "bin")); local >= goBin || goBin >= bun {
		t.Fatalf("home tool directory order = %#v", dirs)
	}

	dirs[0] = "caller mutation"
	if again := StandardDirs(); len(again) == 0 || again[0] == "caller mutation" {
		t.Fatalf("caller mutated later StandardDirs result: %#v", again)
	}
}

func TestUniquePreservesFirstOccurrenceOrder(t *testing.T) {
	got := unique([]string{"", "b", "a", "b", "c", "a", ""})
	want := []string{"b", "a", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unique() = %#v, want %#v", got, want)
	}
}

func writeExecutable(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func indexOf(values []string, target string) int {
	for index, value := range values {
		if value == target {
			return index
		}
	}
	return -1
}
