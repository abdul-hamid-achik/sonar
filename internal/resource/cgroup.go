package resource

import (
	"math"
	"path"
	"sort"
	"strconv"
	"strings"
)

type cgroupFileReader func(string) ([]byte, error)

type cgroupMembership struct {
	unified    string
	controller map[string]string
}

type cgroupMount struct {
	root        string
	point       string
	filesystem  string
	controllers map[string]struct{}
}

type cgroupMemory struct {
	total          int64
	available      int64
	availableKnown bool
}

// intersectCgroupTelemetry applies every discoverable cgroup constraint as an
// upper bound on host telemetry. It is platform-neutral so hierarchy parsing
// can be tested without relying on the machine running the test suite.
func intersectCgroupTelemetry(
	hostCPU int,
	hostTotal, hostAvailable int64,
	hostAvailableKnown bool,
	membershipData, mountInfoData []byte,
	readFile cgroupFileReader,
) (int, int64, int64, bool) {
	if readFile == nil {
		return hostCPU, hostTotal, hostAvailable, hostAvailableKnown
	}
	memberships := parseCgroupMembership(membershipData)
	mounts := parseCgroupMounts(mountInfoData)

	logicalCPU := hostCPU
	if limit := cgroupCPULimit(memberships, mounts, readFile); limit > 0 && (logicalCPU <= 0 || limit < logicalCPU) {
		logicalCPU = limit
	}

	memory := cgroupMemoryLimit(memberships, mounts, hostTotal, readFile)
	total := minPositiveInt64(hostTotal, memory.total)
	available := hostAvailable
	availableKnown := hostAvailableKnown
	if memory.availableKnown {
		if !availableKnown || memory.available < available {
			available = memory.available
		}
		availableKnown = true
	}
	if availableKnown && total > 0 && available > total {
		available = total
	}
	return logicalCPU, total, available, availableKnown
}

func parseCgroupMembership(data []byte) cgroupMembership {
	result := cgroupMembership{controller: make(map[string]string)}
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(parts) != 3 || parts[2] == "" {
			continue
		}
		memberPath := cleanCgroupPath(parts[2])
		if parts[0] == "0" && parts[1] == "" {
			result.unified = memberPath
			continue
		}
		for _, controller := range strings.Split(parts[1], ",") {
			controller = strings.TrimSpace(controller)
			if controller != "" {
				result.controller[controller] = memberPath
			}
		}
	}
	return result
}

func parseCgroupMounts(data []byte) []cgroupMount {
	var result []cgroupMount
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		separator := -1
		for index, field := range fields {
			if field == "-" {
				separator = index
				break
			}
		}
		if separator < 6 || separator+3 >= len(fields) {
			continue
		}
		filesystem := fields[separator+1]
		if filesystem != "cgroup" && filesystem != "cgroup2" {
			continue
		}
		mount := cgroupMount{
			root:        cleanCgroupPath(unescapeMountInfo(fields[3])),
			point:       cleanCgroupPath(unescapeMountInfo(fields[4])),
			filesystem:  filesystem,
			controllers: make(map[string]struct{}),
		}
		if filesystem == "cgroup" {
			for _, option := range strings.Split(fields[separator+3], ",") {
				mount.controllers[option] = struct{}{}
			}
		}
		result = append(result, mount)
	}
	return result
}

func unescapeMountInfo(value string) string {
	replacer := strings.NewReplacer(
		`\040`, " ",
		`\011`, "\t",
		`\012`, "\n",
		`\134`, `\`,
	)
	return replacer.Replace(value)
}

func cleanCgroupPath(value string) string {
	return path.Clean("/" + strings.TrimPrefix(strings.TrimSpace(value), "/"))
}

func resolveCgroupDirectory(mount cgroupMount, membership string) (string, bool) {
	root := cleanCgroupPath(mount.root)
	member := cleanCgroupPath(membership)
	relative := ""
	switch {
	case root == "/":
		relative = strings.TrimPrefix(member, "/")
	case member == root:
	case strings.HasPrefix(member, root+"/"):
		relative = strings.TrimPrefix(member, root+"/")
	default:
		return "", false
	}
	point := cleanCgroupPath(mount.point)
	directory := path.Join(point, relative)
	if directory != point && !strings.HasPrefix(directory, point+"/") {
		return "", false
	}
	return directory, true
}

func cgroupHierarchy(mount cgroupMount, membership string) []string {
	directory, ok := resolveCgroupDirectory(mount, membership)
	if !ok {
		return nil
	}
	root := cleanCgroupPath(mount.point)
	result := make([]string, 0, 4)
	for {
		result = append(result, directory)
		if directory == root {
			return result
		}
		parent := path.Dir(directory)
		if parent == directory || parent != root && !strings.HasPrefix(parent, root+"/") {
			return result
		}
		directory = parent
	}
}

func cgroupCPULimit(memberships cgroupMembership, mounts []cgroupMount, readFile cgroupFileReader) int {
	limit := 0
	apply := func(candidate int) {
		if candidate > 0 && (limit == 0 || candidate < limit) {
			limit = candidate
		}
	}
	for _, mount := range mounts {
		switch mount.filesystem {
		case "cgroup2":
			if memberships.unified == "" {
				continue
			}
			for _, directory := range cgroupHierarchy(mount, memberships.unified) {
				if value, ok := readCgroupValue(readFile, path.Join(directory, "cpu.max")); ok {
					apply(parseCgroupV2CPUQuota(value))
				}
				if value, ok := readCgroupValue(readFile, path.Join(directory, "cpuset.cpus.effective")); ok {
					apply(parseCPUSet(value))
				} else if value, ok := readCgroupValue(readFile, path.Join(directory, "cpuset.cpus")); ok {
					apply(parseCPUSet(value))
				}
			}
		case "cgroup":
			if _, ok := mount.controllers["cpu"]; ok {
				membership := memberships.controller["cpu"]
				if membership != "" {
					for _, directory := range cgroupHierarchy(mount, membership) {
						quota, quotaOK := readCgroupValue(readFile, path.Join(directory, "cpu.cfs_quota_us"))
						period, periodOK := readCgroupValue(readFile, path.Join(directory, "cpu.cfs_period_us"))
						if quotaOK && periodOK {
							apply(parseCgroupV1CPUQuota(quota, period))
						}
					}
				}
			}
			if _, ok := mount.controllers["cpuset"]; ok {
				membership := memberships.controller["cpuset"]
				if membership != "" {
					for _, directory := range cgroupHierarchy(mount, membership) {
						if value, found := readCgroupValue(readFile, path.Join(directory, "cpuset.cpus")); found {
							apply(parseCPUSet(value))
						}
					}
				}
			}
		}
	}
	return limit
}

func parseCgroupV2CPUQuota(value string) int {
	fields := strings.Fields(value)
	if len(fields) != 2 || fields[0] == "max" {
		return 0
	}
	quota, quotaErr := strconv.ParseInt(fields[0], 10, 64)
	period, periodErr := strconv.ParseInt(fields[1], 10, 64)
	if quotaErr != nil || periodErr != nil || quota <= 0 || period <= 0 {
		return 0
	}
	return quotaCPUs(quota, period)
}

func parseCgroupV1CPUQuota(quotaValue, periodValue string) int {
	quota, quotaErr := strconv.ParseInt(strings.TrimSpace(quotaValue), 10, 64)
	period, periodErr := strconv.ParseInt(strings.TrimSpace(periodValue), 10, 64)
	if quotaErr != nil || periodErr != nil || quota <= 0 || period <= 0 {
		return 0
	}
	return quotaCPUs(quota, period)
}

func quotaCPUs(quota, period int64) int {
	value := quota / period
	if value < 1 {
		return 1
	}
	if value > int64(math.MaxInt) {
		return math.MaxInt
	}
	return int(value)
}

type cpuInterval struct {
	first int64
	last  int64
}

func parseCPUSet(value string) int {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	intervals := make([]cpuInterval, 0, strings.Count(value, ",")+1)
	for _, token := range strings.Split(value, ",") {
		bounds := strings.SplitN(strings.TrimSpace(token), "-", 2)
		first, err := strconv.ParseInt(bounds[0], 10, 64)
		if err != nil || first < 0 {
			return 0
		}
		last := first
		if len(bounds) == 2 {
			last, err = strconv.ParseInt(bounds[1], 10, 64)
			if err != nil || last < first {
				return 0
			}
		}
		intervals = append(intervals, cpuInterval{first: first, last: last})
	}
	sort.Slice(intervals, func(i, j int) bool { return intervals[i].first < intervals[j].first })
	count := int64(0)
	current := intervals[0]
	for _, interval := range intervals[1:] {
		if interval.first <= current.last || current.last < math.MaxInt64 && interval.first == current.last+1 {
			if interval.last > current.last {
				current.last = interval.last
			}
			continue
		}
		count = saturatingCPUCount(count, current)
		current = interval
	}
	count = saturatingCPUCount(count, current)
	if count > int64(math.MaxInt) {
		return math.MaxInt
	}
	return int(count)
}

func saturatingCPUCount(count int64, interval cpuInterval) int64 {
	width := interval.last - interval.first + 1
	if width <= 0 || count > int64(math.MaxInt)-width {
		return int64(math.MaxInt)
	}
	return count + width
}

func cgroupMemoryLimit(memberships cgroupMembership, mounts []cgroupMount, hostTotal int64, readFile cgroupFileReader) cgroupMemory {
	result := cgroupMemory{}
	apply := func(limit int64, usage string, usageKnown bool) {
		if limit <= 0 || hostTotal > 0 && limit > hostTotal {
			return
		}
		result.total = minPositiveInt64(result.total, limit)
		available := int64(0)
		if usageKnown {
			if parsed, err := strconv.ParseUint(strings.TrimSpace(usage), 10, 64); err == nil {
				if parsed < uint64(limit) {
					available = limit - int64(parsed)
				}
			}
		}
		if !result.availableKnown || available < result.available {
			result.available = available
		}
		result.availableKnown = true
	}

	for _, mount := range mounts {
		var membership, limitName, usageName string
		switch mount.filesystem {
		case "cgroup2":
			membership = memberships.unified
			limitName, usageName = "memory.max", "memory.current"
		case "cgroup":
			if _, ok := mount.controllers["memory"]; !ok {
				continue
			}
			membership = memberships.controller["memory"]
			limitName, usageName = "memory.limit_in_bytes", "memory.usage_in_bytes"
		}
		if membership == "" {
			continue
		}
		for _, directory := range cgroupHierarchy(mount, membership) {
			limitValue, ok := readCgroupValue(readFile, path.Join(directory, limitName))
			if !ok {
				continue
			}
			limit, finite := parseCgroupMemoryLimit(limitValue, mount.filesystem == "cgroup")
			if !finite {
				continue
			}
			usage, usageKnown := readCgroupValue(readFile, path.Join(directory, usageName))
			apply(limit, usage, usageKnown)
		}
	}
	return result
}

func parseCgroupMemoryLimit(value string, v1 bool) (int64, bool) {
	value = strings.TrimSpace(value)
	if value == "" || value == "max" {
		return 0, false
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil || parsed == 0 || parsed > math.MaxInt64 {
		return 0, false
	}
	// Linux cgroup v1 represents unlimited memory with a page-aligned value near
	// MaxInt64. Treat implausible exabyte-scale values as the sentinel.
	if v1 && parsed >= 1<<60 {
		return 0, false
	}
	return int64(parsed), true
}

func readCgroupValue(readFile cgroupFileReader, name string) (string, bool) {
	data, err := readFile(name)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(data)), true
}

func minPositiveInt64(left, right int64) int64 {
	switch {
	case left <= 0:
		return right
	case right <= 0:
		return left
	case left < right:
		return left
	default:
		return right
	}
}
