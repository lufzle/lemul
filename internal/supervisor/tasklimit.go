package supervisor

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"sync"
	"time"
)

// The task's memory limit, from the platform rather than from the cgroup.
//
// resources.go opens by saying everything is read from cgroups and /proc, "with
// no ECS task-metadata special case. Fargate is Linux with the same cgroup
// files". That was measured false on 2026-08-03, inside a real Fargate task:
//
//	$ cat /sys/fs/cgroup/memory/memory.limit_in_bytes
//	9223372036854771712
//	$ curl -s $ECS_CONTAINER_METADATA_URI_V4/task
//	..."Limits":{"CPU":2,"Memory":8192}...
//
// The 8 GiB cap is real -- exceeding it kills the task -- but it is enforced at
// a cgroup ABOVE the one the container can see, so the file the container reads
// honestly reports "unlimited". readCgroupBytes maps that sentinel to "no
// limit" correctly, and everything downstream then drew the wrong conclusion:
//
//   - Admission control went INERT on the one substrate it exists for.
//     memoryHeadroomMB returned (0, 0), and decision #9's "unknown headroom
//     admits" rule -- written so our own operators on darwin are not refused --
//     then admitted every session regardless of memory. Section 2.4's rules were
//     all still implemented correctly; they simply never fired.
//   - The console reported the HOST's memory as the task's. With no cgroup
//     limit, resources.go falls back to /proc/meminfo, which inside a container
//     reports the machine. On a 2 vCPU / 8 GiB task that read 15,765 MB against
//     a real limit of 8,192 -- wrong rather than absent, which is worse, because
//     a number that looks plausible does not get questioned.
//
// So the cgroup stays the first source, because it is the honest one wherever it
// is set -- docker with --memory, and any local run. The task metadata is the
// fallback for exactly the case where the cgroup says "no limit" while the
// platform is enforcing one. Section 2.1 already makes the sandbox reaching
// 169.254.170.2 *intended*: the task role is precisely the credential it should
// hold, so reading its own limits there is consistent with the split rather than
// a hole in it.
const ecsMetadataURIEnv = "ECS_CONTAINER_METADATA_URI_V4"

// metadataTimeout is short on purpose. This sits behind memoryHeadroomMB, which
// is on the 30 s report path; a link-local endpoint that does not answer
// promptly is not answering, and reporting "unknown" is the documented,
// survivable outcome.
const metadataTimeout = 2 * time.Second

// taskLimit caches the platform limit. A Fargate task's size is FIXED at launch
// (which is the whole reason admission control exists), so this is asked once
// and never changes underneath us. Cached even when the lookup fails, so a task
// running outside ECS does not pay an HTTP timeout every 30 s forever.
var taskLimit struct {
	sync.Mutex
	done  bool
	bytes int64
	cores float64
}

// taskMemoryLimitBytes is the limit anything reasoning about headroom should
// use: the cgroup where it is set, the platform's own answer where it is not.
//
// Returns 0 for "genuinely unknown", which callers must keep treating as
// unknown rather than as zero headroom.
func taskMemoryLimitBytes() int64 {
	if v, ok := readCgroupMemLimit(); ok && v > 0 {
		return v
	}
	bytes, _ := ecsTaskLimits()
	return bytes
}

// taskCPULimitCores is the same arrangement for CPU, and it exists for the same
// measured reason one field over: the container's own cgroup reports no quota
// while the platform enforces one, so a 2 vCPU task rendered as "unlimited".
//
// Returns 0 for "genuinely no quota", which is what the cgroup means when it
// says max -- callers must not read it as "no CPU allowed".
func taskCPULimitCores() float64 {
	if v, ok := readCgroupCPUMax(); ok && v > 0 {
		return v
	}
	_, cores := ecsTaskLimits()
	return cores
}

func ecsTaskLimits() (int64, float64) {
	taskLimit.Lock()
	defer taskLimit.Unlock()
	if taskLimit.done {
		return taskLimit.bytes, taskLimit.cores
	}
	taskLimit.done = true
	taskLimit.bytes, taskLimit.cores = fetchECSTaskLimits(os.Getenv(ecsMetadataURIEnv))
	return taskLimit.bytes, taskLimit.cores
}

// fetchECSTaskLimits reads Limits.Memory (MiB) and Limits.CPU (cores) off the
// task metadata.
//
// TASK level, not container level. A container limit is optional and often
// absent, while the task limit is what Fargate actually enforces and what an
// OOM is measured against -- and an OOM takes the whole task, every member's
// sessions with it (section 2.4).
//
// One request for both, because they arrive in the same document and the
// second caller would otherwise pay a second round trip for a field already
// fetched and thrown away.
func fetchECSTaskLimits(base string) (int64, float64) {
	if base == "" {
		return 0, 0
	}
	// Its own deadline rather than the caller's: this sits under
	// memoryHeadroomMB, which has no context and must not grow one just to
	// reach a link-local endpoint that either answers at once or is not there.
	ctx, cancel := context.WithTimeout(context.Background(), metadataTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/task", nil)
	if err != nil {
		return 0, 0
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, 0
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, 0
	}
	var doc struct {
		Limits struct {
			Memory int64   `json:"Memory"`
			CPU    float64 `json:"CPU"`
		} `json:"Limits"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return 0, 0
	}
	// Reported independently: a document carrying one and not the other should
	// yield the one it has rather than discarding both.
	var bytes int64
	if doc.Limits.Memory > 0 {
		bytes = doc.Limits.Memory << 20
	}
	var cores float64
	if doc.Limits.CPU > 0 {
		cores = doc.Limits.CPU
	}
	return bytes, cores
}
