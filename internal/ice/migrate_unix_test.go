//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package ice

import (
	"errors"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/abdul-hamid-achik/sonar/internal/safeio"
)

func TestLegacyICEPreviewRejectsFIFOMarkerWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conversations.json")
	store := NewStore(path)
	if err := exec.Command("mkfifo", path+".workspace-claim.json").Run(); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	started := time.Now()
	_, err := store.PreviewLegacyEntries("project")
	if !errors.Is(err, safeio.ErrNotRegular) {
		t.Fatalf("FIFO ICE marker error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("FIFO ICE marker read took %s", elapsed)
	}
}
