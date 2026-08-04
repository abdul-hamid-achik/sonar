//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package config

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/safeio"
)

func TestFindAndReadConfigRejectsFIFOWithoutUnboundedStat(t *testing.T) {
	oldWorkDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWorkDir) })
	t.Setenv("HOME", t.TempDir())
	fifo := filepath.Join(dir, "sonar.yaml")
	if err := exec.Command("mkfifo", fifo).Run(); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}

	oldReader := configFileReader
	oldTimeout := configFileReadTimeout
	configFileReader = safeio.NewReader()
	configFileReadTimeout = 30 * time.Millisecond
	t.Cleanup(func() {
		configFileReader = oldReader
		configFileReadTimeout = oldTimeout
	})

	path, data, err := findAndReadConfigFile()
	if err == nil || !errors.Is(err, safeio.ErrNotRegular) || path != "" || data != nil {
		t.Fatalf("FIFO config result path=%q data=%q err=%v", path, data, err)
	}

	probe := filepath.Join(dir, "probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, readErr := configFileReader.ReadRegularFile(probe, 16, time.Second); readErr != nil {
		t.Fatalf("config reader did not recover immediately: %v", readErr)
	}
}
