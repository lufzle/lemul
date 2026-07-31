package supervisor

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Resource sampling for the console's explorer.
//
// The ring lives HERE rather than in the console for one reason: CPU, network
// and disk IO are rates, and a rate needs two samples a known interval apart. If
// the console differenced two polls it would report garbage whenever a tab was
// backgrounded, a request was slow, or two operators watched at once. The
// supervisor samples on a fixed tick and serves the already-computed rate, so
// the numbers are the same no matter who asks or how often.
//
// Everything is read from cgroups and /proc, with no ECS task-metadata special
// case. Fargate is Linux with the same cgroup files, and section 13's whole
// mitigation for losing data-plane observability is that our debugging
// environment shares a code path with the product -- a metrics path that only
// works on ECS would be untested everywhere we can actually watch it.
//
// Every group carries an explicit Available flag. headroom.go already records
// that cgroup data is absent on darwin, where the local driver runs, and a zero
// there means "we could not look", not "nothing is being used". The console must
// be able to tell those apart or it will confidently display a lie.

const (
	sampleInterval = 2 * time.Second
	sampleRingSize = 60 // two minutes of history
)

// ResourceUsage is one point-in-time answer, with a short history for sparklines.
type ResourceUsage struct {
	CPU     CPUUsage     `json:"cpu"`
	Memory  MemoryUsage  `json:"memory"`
	Disk    DiskUsage    `json:"disk"`
	Network NetworkUsage `json:"network"`
	// SampledAt is when the newest sample was taken, so a stalled sampler is
	// visible rather than looking like steady load.
	SampledAt string `json:"sampled_at,omitempty"`
}

type CPUUsage struct {
	Available bool `json:"available"`
	// Cores is CPU-seconds consumed per wall second: 1.0 means one core
	// saturated. Not a percentage, because a task may have several cores.
	Cores float64 `json:"cores"`
	// Limit is the cgroup quota in cores, 0 when unlimited.
	Limit   float64   `json:"limit"`
	History []float64 `json:"history,omitempty"`
}

type MemoryUsage struct {
	Available bool      `json:"available"`
	UsedMB    int       `json:"used_mb"`
	LimitMB   int       `json:"limit_mb"`
	History   []float64 `json:"history,omitempty"`
}

type DiskUsage struct {
	Available bool   `json:"available"`
	UsedMB    int64  `json:"used_mb"`
	TotalMB   int64  `json:"total_mb"`
	Path      string `json:"path,omitempty"`
}

type NetworkUsage struct {
	Available bool `json:"available"`
	// Rates in bytes per second, totalled across interfaces except loopback --
	// loopback is excluded because in gateway mode every model call goes over it
	// to the supervisor's own broker, which would swamp the real figure.
	RxBytesPerSec float64   `json:"rx_bytes_per_sec"`
	TxBytesPerSec float64   `json:"tx_bytes_per_sec"`
	RxTotal       int64     `json:"rx_total"`
	TxTotal       int64     `json:"tx_total"`
	History       []float64 `json:"history,omitempty"`
}

type sample struct {
	at       time.Time
	cpuUsec  int64
	memUsed  int64
	rxBytes  int64
	txBytes  int64
	cpuCores float64
	memMB    float64
	netBps   float64
	ok       bool
}

// sampler holds the ring and the previous raw counters.
type sampler struct {
	root string

	mu   sync.Mutex
	ring []sample
	prev sample
}

func newSampler(root string) *sampler {
	return &sampler{root: root, ring: make([]sample, 0, sampleRingSize)}
}

// run samples until ctx-less shutdown; the supervisor's lifetime is the task's.
func (s *sampler) run(stop <-chan struct{}) {
	t := time.NewTicker(sampleInterval)
	defer t.Stop()
	s.tick() // seed the counters so the first served rate is real, not zero
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			s.tick()
		}
	}
}

func (s *sampler) tick() {
	cur := sample{at: time.Now(), ok: true}
	cur.cpuUsec, _ = readCgroupCPUUsec()
	cur.memUsed, _ = readCgroupMemUsed()
	cur.rxBytes, cur.txBytes, _ = readNetDev()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.prev.ok {
		if elapsed := cur.at.Sub(s.prev.at).Seconds(); elapsed > 0 {
			if cur.cpuUsec > 0 && s.prev.cpuUsec > 0 {
				cur.cpuCores = float64(cur.cpuUsec-s.prev.cpuUsec) / 1e6 / elapsed
			}
			delta := float64((cur.rxBytes - s.prev.rxBytes) + (cur.txBytes - s.prev.txBytes))
			if delta >= 0 {
				cur.netBps = delta / elapsed
			}
		}
	}
	cur.memMB = float64(cur.memUsed >> 20)
	s.prev = cur
	s.ring = append(s.ring, cur)
	if len(s.ring) > sampleRingSize {
		s.ring = s.ring[len(s.ring)-sampleRingSize:]
	}
}

// usage renders the current answer.
func (s *sampler) usage() ResourceUsage {
	s.mu.Lock()
	ring := append([]sample(nil), s.ring...)
	s.mu.Unlock()

	var out ResourceUsage
	if n := len(ring); n > 0 {
		out.SampledAt = ring[n-1].at.UTC().Format(time.RFC3339)
	}

	quota, quotaOK := readCgroupCPUMax()
	if _, ok := readCgroupCPUUsec(); ok {
		out.CPU.Available = true
		if n := len(ring); n > 0 {
			out.CPU.Cores = ring[n-1].cpuCores
		}
		if quotaOK {
			out.CPU.Limit = quota
		}
		out.CPU.History = seriesOf(ring, func(s sample) float64 { return s.cpuCores })
	}

	// Availability keys off USAGE, not the limit. A container started without
	// --memory reports memory.max as "max", and headroom.go treats that as
	// (0, 0) because admission control genuinely has no headroom figure to work
	// with. The explorer is a different question: usage is right there in
	// memory.current, and answering "unknown" while holding the number would be
	// the console admitting to an ignorance it does not have. A zero limit means
	// "no quota", exactly as it does for CPU above.
	if used, ok := readCgroupMemUsed(); ok {
		limit, _ := readCgroupMemLimit()
		out.Memory = MemoryUsage{
			Available: true,
			UsedMB:    int(used >> 20),
			LimitMB:   int(limit >> 20),
			History:   seriesOf(ring, func(s sample) float64 { return s.memMB }),
		}
	}

	out.Disk = diskUsage(s.root)

	if rx, tx, ok := readNetDev(); ok {
		out.Network = NetworkUsage{
			Available: true,
			RxTotal:   rx,
			TxTotal:   tx,
			History:   seriesOf(ring, func(s sample) float64 { return s.netBps }),
		}
		if n := len(ring); n > 0 && n >= 2 {
			elapsed := ring[n-1].at.Sub(ring[n-2].at).Seconds()
			if elapsed > 0 {
				out.Network.RxBytesPerSec = float64(ring[n-1].rxBytes-ring[n-2].rxBytes) / elapsed
				out.Network.TxBytesPerSec = float64(ring[n-1].txBytes-ring[n-2].txBytes) / elapsed
			}
		}
	}
	return out
}

func seriesOf(ring []sample, pick func(sample) float64) []float64 {
	out := make([]float64, 0, len(ring))
	for _, s := range ring {
		out = append(out, pick(s))
	}
	return out
}

// cgroupRoot is a variable so tests can point the readers at a fixture tree.
var cgroupRoot = "/sys/fs/cgroup"

// readCgroupMemUsed and readCgroupMemLimit reuse headroom.go's reader, which
// already knows that "max" and the v1 sentinel both mean "no limit set" rather
// than a real number. They go through cgroupRoot so tests can redirect them.
func readCgroupMemUsed() (int64, bool) {
	return readCgroupBytes(
		filepath.Join(cgroupRoot, "memory.current"),
		filepath.Join(cgroupRoot, "memory", "memory.usage_in_bytes"),
	)
}

func readCgroupMemLimit() (int64, bool) {
	return readCgroupBytes(
		filepath.Join(cgroupRoot, "memory.max"),
		filepath.Join(cgroupRoot, "memory", "memory.limit_in_bytes"),
	)
}

// readCgroupCPUUsec returns cumulative CPU microseconds for the cgroup.
func readCgroupCPUUsec() (int64, bool) {
	if b, err := os.ReadFile(filepath.Join(cgroupRoot, "cpu.stat")); err == nil {
		return parseCPUStat(string(b))
	}
	// cgroup v1 reports nanoseconds in a different file.
	if b, err := os.ReadFile(filepath.Join(cgroupRoot, "cpuacct", "cpuacct.usage")); err == nil {
		n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		return n / 1000, err == nil
	}
	return 0, false
}

func parseCPUStat(s string) (int64, bool) {
	for _, line := range strings.Split(s, "\n") {
		if v, ok := strings.CutPrefix(line, "usage_usec "); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			return n, err == nil
		}
	}
	return 0, false
}

// readCgroupCPUMax returns the quota in cores, 0 for unlimited.
func readCgroupCPUMax() (float64, bool) {
	b, err := os.ReadFile(filepath.Join(cgroupRoot, "cpu.max"))
	if err != nil {
		return 0, false
	}
	return parseCPUMax(string(b))
}

// parseCPUMax reads "quota period" in microseconds and returns cores.
//
// "max period" means no quota, and that is reported as (0, true) -- present but
// unlimited. A caller must not read the zero as "no CPU allowed", which is the
// same distinction headroom.go draws for memory.
func parseCPUMax(s string) (float64, bool) {
	f := strings.Fields(strings.TrimSpace(s))
	if len(f) != 2 {
		return 0, false
	}
	if f[0] == "max" {
		return 0, true
	}
	quota, err1 := strconv.ParseFloat(f[0], 64)
	period, err2 := strconv.ParseFloat(f[1], 64)
	if err1 != nil || err2 != nil || period == 0 {
		return 0, false
	}
	return quota / period, true
}

// readNetDev totals interface counters, skipping loopback.
func readNetDev() (rx, tx int64, ok bool) {
	b, err := os.ReadFile(filepath.Join(procRoot, "net", "dev"))
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		name, rest, found := strings.Cut(line, ":")
		if !found {
			continue // the two header lines
		}
		if strings.TrimSpace(name) == "lo" {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			continue
		}
		r, _ := strconv.ParseInt(f[0], 10, 64)
		t, _ := strconv.ParseInt(f[8], 10, 64)
		rx += r
		tx += t
	}
	return rx, tx, true
}

// diskUsage reports the filesystem holding the workspace. This is the figure an
// operator wants -- "will the next npm install fail" -- rather than cgroup IO
// accounting, which measures throughput and not space.
func diskUsage(root string) DiskUsage {
	if root == "" {
		return DiskUsage{}
	}
	var st unix.Statfs_t
	if err := unix.Statfs(root, &st); err != nil {
		return DiskUsage{Path: root}
	}
	total := int64(st.Blocks) * int64(st.Bsize)
	free := int64(st.Bavail) * int64(st.Bsize)
	return DiskUsage{
		Available: true,
		UsedMB:    (total - free) >> 20,
		TotalMB:   total >> 20,
		Path:      root,
	}
}
