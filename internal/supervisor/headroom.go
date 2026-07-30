package supervisor

import (
	"os"
	"strconv"
	"strings"
)

// memoryHeadroomMB reports free and total memory for the task, for admission
// control (section 2.4).
//
// The supervisor is the honest source because it is the only thing inside the
// cgroup: a Fargate task's size is fixed at launch and cannot grow, so the
// control plane has to refuse a session rather than discover the limit by OOM
// -- which would kill every session in the workspace, including a sibling's
// unattended overnight run.
//
// Returns (0, 0) where cgroup data is unavailable, e.g. on darwin during local
// development. Callers treat a zero limit as "unknown", not as "no headroom".
func memoryHeadroomMB() (freeMB, limitMB int) {
	limit, okLimit := readCgroupBytes(
		"/sys/fs/cgroup/memory.max",                   // cgroup v2
		"/sys/fs/cgroup/memory/memory.limit_in_bytes", // cgroup v1
	)
	used, okUsed := readCgroupBytes(
		"/sys/fs/cgroup/memory.current",
		"/sys/fs/cgroup/memory/memory.usage_in_bytes",
	)
	if !okLimit || !okUsed || limit == 0 {
		return 0, 0
	}
	free := limit - used
	if free < 0 {
		free = 0
	}
	return int(free >> 20), int(limit >> 20)
}

func readCgroupBytes(paths ...string) (int64, bool) {
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		s := strings.TrimSpace(string(b))
		// cgroup v2 writes "max" for an unlimited cgroup, and v1 writes a
		// sentinel close to 2^63; both mean "no limit set", not a real number.
		if s == "max" {
			return 0, true
		}
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			continue
		}
		if v > (1 << 62) {
			return 0, true
		}
		return v, true
	}
	return 0, false
}
