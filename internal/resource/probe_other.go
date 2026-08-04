//go:build !linux && !darwin

package resource

import "context"

func probeSystemResources(_ context.Context, logicalCPU int) (int, int64, int64, bool) {
	return logicalCPU, 0, 0, false
}
