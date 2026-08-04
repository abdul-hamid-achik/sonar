//go:build linux

package resource

import (
	"bytes"
	"context"
	"io"
	"os"
)

const (
	maxProcMeminfoBytes int64 = 128 << 10
	maxProcCgroupBytes  int64 = 1 << 20
	maxCgroupValueBytes int64 = 4 << 10
)

func probeSystemResources(ctx context.Context, logicalCPU int) (int, int64, int64, bool) {
	total, available, availableKnown := int64(0), int64(0), false
	if data, err := readLinuxResourceFile(ctx, "/proc/meminfo", maxProcMeminfoBytes); err == nil {
		if parsedTotal, parsedAvailable, parsedKnown, parseErr := parseProcMeminfoDetailed(bytes.NewReader(data)); parseErr == nil {
			total, available, availableKnown = parsedTotal, parsedAvailable, parsedKnown
		}
	}
	if err := ctx.Err(); err != nil {
		return logicalCPU, total, available, availableKnown
	}

	membership, membershipErr := readLinuxResourceFile(ctx, "/proc/self/cgroup", maxProcCgroupBytes)
	mountInfo, mountErr := readLinuxResourceFile(ctx, "/proc/self/mountinfo", maxProcCgroupBytes)
	if membershipErr != nil || mountErr != nil {
		return logicalCPU, total, available, availableKnown
	}

	readCgroup := func(name string) ([]byte, error) {
		return readLinuxResourceFile(ctx, name, maxCgroupValueBytes)
	}
	return intersectCgroupTelemetry(
		logicalCPU, total, available, availableKnown,
		membership, mountInfo, readCgroup,
	)
}

func readLinuxResourceFile(ctx context.Context, name string, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, io.ErrShortBuffer
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}
