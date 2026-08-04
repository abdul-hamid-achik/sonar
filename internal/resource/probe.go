package resource

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strconv"
	"strings"
)

var errNilProbeFunc = errors.New("resource probe function is required")

// SystemProbe observes the capacity available to this process, intersecting
// host telemetry with Linux cgroup constraints when present. It never launches
// subprocesses or inspects model state; callers provide model facts separately.
type SystemProbe struct{}

func (SystemProbe) Snapshot(ctx context.Context) (HostSnapshot, error) {
	if ctx == nil {
		return HostSnapshot{}, errors.New("resource probe context is required")
	}
	if err := ctx.Err(); err != nil {
		return HostSnapshot{}, err
	}
	logicalCPU, total, available, availableKnown := probeSystemResources(ctx, runtime.NumCPU())
	if err := ctx.Err(); err != nil {
		return HostSnapshot{}, err
	}
	return HostSnapshot{
		LogicalCPU: logicalCPU, TotalRAMBytes: total, AvailableRAMBytes: available,
		AvailableRAMKnown: availableKnown,
	}, nil
}

func parseProcMeminfo(reader io.Reader) (total, available int64, err error) {
	total, available, _, err = parseProcMeminfoDetailed(reader)
	return total, available, err
}

func parseProcMeminfoDetailed(reader io.Reader) (total, available int64, availableKnown bool, err error) {
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 || fields[2] != "kB" {
			continue
		}
		if fields[0] != "MemTotal:" && fields[0] != "MemAvailable:" {
			continue
		}
		kilobytes, parseErr := strconv.ParseInt(fields[1], 10, 64)
		if parseErr != nil || kilobytes < 0 || fields[0] == "MemTotal:" && kilobytes == 0 {
			return 0, 0, false, fmt.Errorf("parse %s from /proc/meminfo", fields[0])
		}
		bytes, multiplyErr := checkedMultiply(kilobytes, 1024)
		if multiplyErr != nil {
			return 0, 0, false, fmt.Errorf("convert %s from /proc/meminfo: %w", fields[0], multiplyErr)
		}
		switch fields[0] {
		case "MemTotal:":
			total = bytes
		case "MemAvailable:":
			available = bytes
			availableKnown = true
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		return 0, 0, false, scanErr
	}
	if total == 0 {
		return 0, 0, false, errors.New("MemTotal is unavailable in /proc/meminfo")
	}
	if available > total {
		return 0, 0, false, errors.New("MemAvailable exceeds MemTotal in /proc/meminfo")
	}
	return total, available, availableKnown, nil
}
