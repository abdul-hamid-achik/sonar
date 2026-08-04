package resource

import (
	"context"
	"strings"
	"testing"
)

func TestParseProcMeminfo(t *testing.T) {
	t.Parallel()

	total, available, err := parseProcMeminfo(strings.NewReader(`MemTotal:       16384 kB
MemFree:         1024 kB
MemAvailable:    8192 kB
`))
	if err != nil {
		t.Fatalf("parseProcMeminfo() error = %v", err)
	}
	if total != 16*mib || available != 8*mib {
		t.Errorf("parseProcMeminfo() = %d, %d; want %d, %d", total, available, 16*mib, 8*mib)
	}
}

func TestParseProcMeminfoAllowsMissingAvailable(t *testing.T) {
	t.Parallel()

	total, available, err := parseProcMeminfo(strings.NewReader("MemTotal: 4096 kB\n"))
	if err != nil {
		t.Fatalf("parseProcMeminfo() error = %v", err)
	}
	if total != 4*mib || available != 0 {
		t.Errorf("parseProcMeminfo() = %d, %d; want %d, 0", total, available, 4*mib)
	}
}

func TestParseProcMeminfoDistinguishesKnownZeroAvailable(t *testing.T) {
	t.Parallel()

	total, available, known, err := parseProcMeminfoDetailed(strings.NewReader("MemTotal: 4096 kB\nMemAvailable: 0 kB\n"))
	if err != nil {
		t.Fatal(err)
	}
	if total != 4*mib || available != 0 || !known {
		t.Fatalf("detailed meminfo = total %d available %d known %v", total, available, known)
	}
}

func TestParseProcMeminfoRejectsInvalidData(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data string
	}{
		{name: "missing total", data: "MemAvailable: 1 kB\n"},
		{name: "invalid total", data: "MemTotal: nope kB\n"},
		{name: "zero total", data: "MemTotal: 0 kB\n"},
		{name: "available exceeds total", data: "MemTotal: 1 kB\nMemAvailable: 2 kB\n"},
		{name: "overflow", data: "MemTotal: 9223372036854775807 kB\n"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, _, err := parseProcMeminfo(strings.NewReader(test.data)); err == nil {
				t.Fatal("parseProcMeminfo() error = nil, want error")
			}
		})
	}
}

func TestSystemProbe(t *testing.T) {
	t.Parallel()

	snapshot, err := (SystemProbe{}).Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if snapshot.LogicalCPU <= 0 {
		t.Errorf("LogicalCPU = %d, want positive", snapshot.LogicalCPU)
	}
	if snapshot.TotalRAMBytes < 0 || snapshot.AvailableRAMBytes < 0 {
		t.Errorf("negative RAM snapshot: %+v", snapshot)
	}
	if snapshot.TotalRAMBytes > 0 && snapshot.AvailableRAMBytes > snapshot.TotalRAMBytes {
		t.Errorf("invalid RAM snapshot: %+v", snapshot)
	}
}

func TestSystemProbeHonorsContext(t *testing.T) {
	t.Parallel()

	//nolint:staticcheck // The public boundary must reject a nil context explicitly.
	if _, err := (SystemProbe{}).Snapshot(nil); err == nil {
		t.Fatal("Snapshot(nil) error = nil, want error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (SystemProbe{}).Snapshot(ctx); err != context.Canceled {
		t.Fatalf("Snapshot(canceled) error = %v, want context.Canceled", err)
	}
}
