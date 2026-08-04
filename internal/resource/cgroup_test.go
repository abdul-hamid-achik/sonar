package resource

import (
	"errors"
	"testing"
)

func TestIntersectCgroupV2TelemetryAcrossHierarchy(t *testing.T) {
	t.Parallel()

	files := map[string]string{
		"/sys/fs/cgroup/job/cpu.max":               "max 100000\n",
		"/sys/fs/cgroup/job/cpuset.cpus.effective": "0-7\n",
		"/sys/fs/cgroup/job/memory.max":            "8589934592\n",
		"/sys/fs/cgroup/job/memory.current":        "3221225472\n",
		"/sys/fs/cgroup/cpu.max":                   "250000 100000\n",
		"/sys/fs/cgroup/cpuset.cpus.effective":     "0-15\n",
		"/sys/fs/cgroup/memory.max":                "12884901888\n",
		"/sys/fs/cgroup/memory.current":            "6442450944\n",
	}
	readFile := mapCgroupReader(files)
	cpu, total, available, known := intersectCgroupTelemetry(
		16, 32*gib, 24*gib, true,
		[]byte("0::/tenant/job\n"),
		[]byte("36 25 0:32 /tenant /sys/fs/cgroup rw - cgroup2 cgroup rw\n"),
		readFile,
	)
	if cpu != 2 || total != 8*gib || available != 5*gib || !known {
		t.Fatalf("v2 intersection = cpu=%d total=%d available=%d known=%v", cpu, total, available, known)
	}
}

func TestIntersectCgroupV1Telemetry(t *testing.T) {
	t.Parallel()

	files := map[string]string{
		"/sys/fs/cgroup/cpu/a/cpu.cfs_quota_us":         "150000\n",
		"/sys/fs/cgroup/cpu/a/cpu.cfs_period_us":        "100000\n",
		"/sys/fs/cgroup/cpu/cpu.cfs_quota_us":           "400000\n",
		"/sys/fs/cgroup/cpu/cpu.cfs_period_us":          "100000\n",
		"/sys/fs/cgroup/cpuset/a/cpuset.cpus":           "2-3,6\n",
		"/sys/fs/cgroup/memory/a/memory.limit_in_bytes": "4294967296\n",
		"/sys/fs/cgroup/memory/a/memory.usage_in_bytes": "1073741824\n",
	}
	mountInfo := []byte(
		"31 25 0:28 /docker /sys/fs/cgroup/cpu rw - cgroup cgroup rw,cpu,cpuacct\n" +
			"32 25 0:29 /docker /sys/fs/cgroup/cpuset rw - cgroup cgroup rw,cpuset\n" +
			"33 25 0:30 /docker /sys/fs/cgroup/memory rw - cgroup cgroup rw,memory\n",
	)
	membership := []byte(
		"2:cpu,cpuacct:/docker/a\n" +
			"3:cpuset:/docker/a\n" +
			"4:memory:/docker/a\n",
	)
	cpu, total, available, known := intersectCgroupTelemetry(
		12, 16*gib, 12*gib, true, membership, mountInfo, mapCgroupReader(files),
	)
	if cpu != 1 || total != 4*gib || available != 3*gib || !known {
		t.Fatalf("v1 intersection = cpu=%d total=%d available=%d known=%v", cpu, total, available, known)
	}
}

func TestIntersectCgroupMemoryKnownZeroFailsClosed(t *testing.T) {
	t.Parallel()

	files := map[string]string{
		"/sys/fs/cgroup/memory.max":     "2147483648\n",
		"/sys/fs/cgroup/memory.current": "3221225472\n",
	}
	cpu, total, available, known := intersectCgroupTelemetry(
		8, 16*gib, 8*gib, true,
		[]byte("0::/\n"),
		[]byte("36 25 0:32 / /sys/fs/cgroup rw - cgroup2 cgroup rw\n"),
		mapCgroupReader(files),
	)
	if cpu != 8 || total != 2*gib || available != 0 || !known {
		t.Fatalf("exhausted cgroup = cpu=%d total=%d available=%d known=%v", cpu, total, available, known)
	}
}

func TestIntersectCgroupMemoryMissingUsageFailsClosed(t *testing.T) {
	t.Parallel()

	files := map[string]string{"/sys/fs/cgroup/memory.max": "2147483648\n"}
	_, total, available, known := intersectCgroupTelemetry(
		8, 16*gib, 8*gib, true,
		[]byte("0::/\n"),
		[]byte("36 25 0:32 / /sys/fs/cgroup rw - cgroup2 cgroup rw\n"),
		mapCgroupReader(files),
	)
	if total != 2*gib || available != 0 || !known {
		t.Fatalf("missing usage = total=%d available=%d known=%v", total, available, known)
	}
}

func TestIntersectCgroupWithoutConstraintsPreservesHost(t *testing.T) {
	t.Parallel()

	cpu, total, available, known := intersectCgroupTelemetry(
		8, 16*gib, 6*gib, true, nil, nil, mapCgroupReader(nil),
	)
	if cpu != 8 || total != 16*gib || available != 6*gib || !known {
		t.Fatalf("unconstrained intersection = cpu=%d total=%d available=%d known=%v", cpu, total, available, known)
	}
}

func TestParseCPUSetCountsUnion(t *testing.T) {
	t.Parallel()

	for value, want := range map[string]int{
		"0-3,6,8-9": 7,
		"0-3,2-5":   6,
		"7":         1,
		"":          0,
		"3-1":       0,
	} {
		if got := parseCPUSet(value); got != want {
			t.Errorf("parseCPUSet(%q) = %d, want %d", value, got, want)
		}
	}
}

func mapCgroupReader(files map[string]string) cgroupFileReader {
	return func(name string) ([]byte, error) {
		value, ok := files[name]
		if !ok {
			return nil, errors.New("not found")
		}
		return []byte(value), nil
	}
}
