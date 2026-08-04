//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package memory

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/safeio"
)

func TestLegacyMigrationRejectsFIFOInputsWithoutBlocking(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, legacyPath, targetPath, workspace string)
	}{
		{
			name: "source",
			setup: func(t *testing.T, legacyPath, _, _ string) {
				t.Helper()
				if err := exec.Command("mkfifo", legacyPath).Run(); err != nil {
					t.Skipf("mkfifo unavailable: %v", err)
				}
			},
		},
		{
			name: "marker",
			setup: func(t *testing.T, legacyPath, _, _ string) {
				t.Helper()
				if err := os.WriteFile(legacyPath, []byte("[]"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := exec.Command("mkfifo", legacyPath+".workspace-claim.json").Run(); err != nil {
					t.Skipf("mkfifo unavailable: %v", err)
				}
			},
		},
		{
			name: "target",
			setup: func(t *testing.T, legacyPath, targetPath, _ string) {
				t.Helper()
				if err := os.WriteFile(legacyPath, []byte("[]"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := exec.Command("mkfifo", targetPath).Run(); err != nil {
					t.Skipf("mkfifo unavailable: %v", err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			legacyPath := filepath.Join(dir, "memories.json")
			targetPath := filepath.Join(dir, "target.json")
			workspace := t.TempDir()
			test.setup(t, legacyPath, targetPath, workspace)
			started := time.Now()
			_, err := previewLegacyFileForWorkspace(legacyPath, targetPath, workspace)
			if !errors.Is(err, safeio.ErrNotRegular) {
				t.Fatalf("FIFO %s error = %v", test.name, err)
			}
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Fatalf("FIFO %s read took %s", test.name, elapsed)
			}
		})
	}
}
