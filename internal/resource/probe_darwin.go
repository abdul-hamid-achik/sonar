//go:build darwin

package resource

import (
	"context"
	"math"

	"golang.org/x/sys/unix"
)

func probeSystemResources(ctx context.Context, logicalCPU int) (int, int64, int64, bool) {
	if err := ctx.Err(); err != nil {
		return logicalCPU, 0, 0, false
	}
	total, err := unix.SysctlUint64("hw.memsize")
	if err != nil || total > math.MaxInt64 {
		return logicalCPU, 0, 0, false
	}
	// Darwin does not expose a stable MemAvailable equivalent through sysctl.
	// Keep it unknown so BuildProfile applies its total-only 25% cap.
	return logicalCPU, int64(total), 0, false
}
