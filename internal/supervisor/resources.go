package supervisor

import (
	"os"
	"path/filepath"
	"runtime"
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
// Usage is read from cgroups and /proc, and section 13's whole mitigation for
// losing data-plane observability is that our debugging environment shares a
// code path with the product -- a metrics path that only worked on ECS would be
// untested everywhere we can actually watch it. So the cgroup is the first
// source for everything, always.
//
// THE LIMITS ARE THE EXCEPTION, twice over, and this paragraph used to deny it:
// "no ECS task-metadata special case. Fargate is Linux with the same cgroup
// files." Fargate is Linux with cgroup **v1**, mounted ro, and it enforces the
// task's caps ABOVE the cgroup the container can read -- so that cgroup
// honestly reports no limit while a limit very much exists. Memory was measured
// and corrected on 2026-08-03 (tasklimit.go); CPU was found the same way
// afterwards, a 2 vCPU task reporting an unlimited quota. Both now read the
// cgroup first and fall back to the task metadata only where the cgroup says
// "unset", which keeps the shared code path for every environment that answers
// honestly.
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

// CPUUsage and the groups beside it each pair UTILISATION with the SATURATION
// signal that actually bites
// this workload -- the USE method, narrowed to a Claude Code sandbox. Utilisation
// alone answers "how busy", which is the less useful half: a task can sit at 40%
// CPU and still be crawling because it is throttled, and can look comfortable on
// disk space while npm has exhausted the inode table.
//
// Pressure-stall information would be the better saturation source and is
// deliberately absent: cpu.pressure, memory.pressure and io.pressure do not
// exist on the kernel this runs on, so everything here comes from counters that
// were confirmed readable inside a real container.
type CPUUsage struct {
	Available bool `json:"available"`
	// Cores is CPU-seconds consumed per wall second: 1.0 means one core
	// saturated. Not a percentage, because a task may have several cores.
	Cores float64 `json:"cores"`
	// Limit is the cgroup quota in cores, 0 when unlimited.
	Limit float64 `json:"limit"`
	// CPUs is what a percentage is a percentage OF: the quota where one is set,
	// otherwise the processors this container can actually see. Without it the
	// console would have to guess a denominator, and guessing the host's core
	// count for a quota'd task overstates headroom by whatever the quota was.
	CPUs float64 `json:"cpus"`
	// ThrottledPercent is the share of scheduling periods in which the cgroup
	// was held back after exhausting its quota. This is the number that explains
	// a build crawling at what looks like moderate CPU, and it is meaningless
	// without a quota -- so it is 0 on a container started without one, which is
	// honest rather than reassuring.
	ThrottledPercent float64   `json:"throttled_percent"`
	History          []float64 `json:"history,omitempty"`
}

type MemoryUsage struct {
	Available bool `json:"available"`
	// AnonMB is memory that cannot be reclaimed -- the figure that decides
	// whether this task is heading for an OOM. CacheMB is page cache, which the
	// kernel drops under pressure.
	//
	// They are reported apart because memory.current lumps them together, and a
	// build that has read a large tree looks alarming on the total while being
	// perfectly healthy. `free` has drawn this distinction for decades.
	AnonMB  int `json:"anon_mb"`
	CacheMB int `json:"cache_mb"`
	// UsedMB is memory.current, the figure actually charged against the limit.
	UsedMB  int `json:"used_mb"`
	LimitMB int `json:"limit_mb"`
	// TotalMB is what a percentage is a percentage OF: the cgroup limit where
	// one is set, otherwise the machine's RAM. Without it "% used" has no
	// denominator on a container started without --memory, which is every
	// container the docker driver starts.
	TotalMB int `json:"total_mb"`
	// OOMKills counts processes the kernel killed in this cgroup. Section 2.4
	// makes this the blast-radius number: an OOM takes the whole task and every
	// session in it, including a sibling's unattended overnight run.
	OOMKills int       `json:"oom_kills"`
	History  []float64 `json:"history,omitempty"`
}

type DiskUsage struct {
	Available bool   `json:"available"`
	UsedMB    int64  `json:"used_mb"`
	TotalMB   int64  `json:"total_mb"`
	Path      string `json:"path,omitempty"`
	// InodesUsedPercent is the failure mode a space gauge cannot see. A
	// node_modules tree is hundreds of thousands of tiny files, so a workspace
	// can exhaust its inode table with the disk apparently half empty, and every
	// write then fails with ENOSPC while df reports plenty free.
	InodesUsedPercent float64 `json:"inodes_used_percent"`
}

type NetworkUsage struct {
	Available bool `json:"available"`
	// Rates in bytes per second, totalled across interfaces except loopback --
	// loopback is excluded because in gateway mode every model call goes over it
	// to the supervisor's own broker, which would swamp the real figure.
	RxBytesPerSec float64 `json:"rx_bytes_per_sec"`
	TxBytesPerSec float64 `json:"tx_bytes_per_sec"`
	RxTotal       int64   `json:"rx_total"`
	TxTotal       int64   `json:"tx_total"`
	// Errors and drops across the same interfaces. Agents pull packages
	// constantly, so a climbing drop count is the difference between "the
	// network is slow" and "the network is broken".
	Errors int64 `json:"errors"`
	Drops  int64 `json:"drops"`
	// Separate series, because the console draws receive and transmit as two
	// lines. A single combined series cannot be unmixed afterwards, and the
	// interesting shape is usually one direction moving without the other.
	RxHistory []float64 `json:"rx_history,omitempty"`
	TxHistory []float64 `json:"tx_history,omitempty"`
}

type sample struct {
	at       time.Time
	cpuUsec  int64
	memUsed  int64
	rxBytes  int64
	txBytes  int64
	cpuCores float64
	// anonMB, not memory.current: the history has to plot the same quantity the
	// tile reports, or the sparkline is quietly about something else.
	anonMB float64
	rxBps  float64
	txBps  float64
	ok     bool
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
	anon, _, _ := memoryBreakdown()
	cur.anonMB = float64(anon >> 20)
	cur.rxBytes, cur.txBytes, _, _, _ = readNetDev()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.prev.ok {
		if elapsed := cur.at.Sub(s.prev.at).Seconds(); elapsed > 0 {
			if cur.cpuUsec > 0 && s.prev.cpuUsec > 0 {
				cur.cpuCores = float64(cur.cpuUsec-s.prev.cpuUsec) / 1e6 / elapsed
			}
			if d := float64(cur.rxBytes - s.prev.rxBytes); d >= 0 {
				cur.rxBps = d / elapsed
			}
			if d := float64(cur.txBytes - s.prev.txBytes); d >= 0 {
				cur.txBps = d / elapsed
			}
		}
	}
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

	// The platform's quota, not only the cgroup's -- the same correction
	// taskMemoryLimitBytes makes below, for the same measured reason: on Fargate
	// the container's cgroup carries no quota while the task is capped above it.
	quota := taskCPULimitCores()
	if _, ok := readCgroupCPUUsec(); ok {
		out.CPU.Available = true
		if n := len(ring); n > 0 {
			out.CPU.Cores = ring[n-1].cpuCores
		}
		out.CPU.Limit = quota
		out.CPU.History = seriesOf(ring, func(s sample) float64 { return s.cpuCores })
		out.CPU.ThrottledPercent, _ = throttling()
		out.CPU.CPUs = quota
		if out.CPU.CPUs == 0 {
			// runtime.NumCPU honours the container's CPU affinity, so this is
			// what the task can schedule on rather than what the host owns.
			out.CPU.CPUs = float64(runtime.NumCPU())
		}
	}

	// Availability keys off USAGE, not the limit. A container started without
	// --memory reports memory.max as "max", and headroom.go treats that as
	// (0, 0) because admission control genuinely has no headroom figure to work
	// with. The explorer is a different question: usage is right there in
	// memory.current, and answering "unknown" while holding the number would be
	// the console admitting to an ignorance it does not have. A zero limit means
	// "no quota", exactly as it does for CPU above.
	if used, ok := readCgroupMemUsed(); ok {
		// The platform's limit, not only the cgroup's. On Fargate the cgroup the
		// container can read reports "unlimited" while the task is capped above
		// it, and falling straight through to /proc/meminfo then reported the
		// HOST's 15,765 MB as a 8,192 MB task's memory (tasklimit.go).
		limit := taskMemoryLimitBytes()
		anon, file, _ := memoryBreakdown()
		total := int(limit >> 20)
		if total == 0 {
			// /proc/meminfo inside a container reports the HOST's memory, which
			// is exactly right here: with nothing imposing a limit anywhere, the
			// host's RAM is what this task can actually use. `free` behaves the
			// same way.
			total = machineMemoryMB()
		}
		out.Memory = MemoryUsage{
			Available: true,
			AnonMB:    int(anon >> 20),
			CacheMB:   int(file >> 20),
			UsedMB:    int(used >> 20),
			LimitMB:   int(limit >> 20),
			TotalMB:   total,
			OOMKills:  oomKills(),
			History:   seriesOf(ring, func(s sample) float64 { return s.anonMB }),
		}
	}

	out.Disk = diskUsage(s.root)

	if rx, tx, errs, drops, ok := readNetDev(); ok {
		out.Network = NetworkUsage{
			Available: true,
			RxTotal:   rx,
			TxTotal:   tx,
			Errors:    errs,
			Drops:     drops,
			RxHistory: seriesOf(ring, func(s sample) float64 { return s.rxBps }),
			TxHistory: seriesOf(ring, func(s sample) float64 { return s.txBps }),
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

// throttling reports the share of scheduling periods in which the cgroup was
// held back. Zero periods means no quota is set, and therefore no throttling is
// possible -- not that none occurred.
func throttling() (percent float64, ok bool) {
	b, err := os.ReadFile(filepath.Join(cgroupRoot, "cpu.stat"))
	if err != nil {
		return 0, false
	}
	var periods, throttled float64
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		v, err := strconv.ParseFloat(f[1], 64)
		if err != nil {
			continue
		}
		switch f[0] {
		case "nr_periods":
			periods = v
		case "nr_throttled":
			throttled = v
		}
	}
	if periods == 0 {
		return 0, true
	}
	return throttled / periods * 100, true
}

// memoryBreakdown splits the charge into what can be reclaimed and what cannot.
//
// BOTH cgroup layouts, because they disagree on the file AND on the key names,
// and reading only v2 fails silently: every key misses, so it returns (0, 0,
// true) -- "measured, and the answer is zero". Fargate is cgroup v1 (measured
// 2026-08-03), so anon and cache were flat zero on every real task, which is
// also what the console's memory history plots.
func memoryBreakdown() (anon, file int64, ok bool) {
	b, err := os.ReadFile(filepath.Join(cgroupRoot, "memory.stat"))
	if err == nil {
		return parseMemoryStat(string(b), "anon", "file")
	}
	// v1: a different path, and total_* rather than the v2 names. The total_
	// prefix is the one that includes child cgroups, which is what "this task"
	// means once the supervisor forks a session per member.
	b, err = os.ReadFile(filepath.Join(cgroupRoot, "memory", "memory.stat"))
	if err != nil {
		return 0, 0, false
	}
	return parseMemoryStat(string(b), "total_rss", "total_cache")
}

func parseMemoryStat(s, anonKey, fileKey string) (anon, file int64, ok bool) {
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		v, _ := strconv.ParseInt(f[1], 10, 64)
		switch f[0] {
		case anonKey:
			anon = v
		case fileKey:
			file = v
		}
	}
	return anon, file, true
}

// oomKills is a counter over the cgroup's lifetime, so a task that survived one
// still reports it -- which is the point. Section 2.4 wants the event visible
// after the fact, not only while it is happening.
// machineMemoryMB reads MemTotal, the fallback denominator when no cgroup limit
// is set.
func machineMemoryMB() int {
	b, err := os.ReadFile(filepath.Join(procRoot, "meminfo"))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "MemTotal:"); ok {
			f := strings.Fields(v) // "16316000 kB"
			if len(f) == 0 {
				return 0
			}
			kb, err := strconv.ParseInt(f[0], 10, 64)
			if err != nil {
				return 0
			}
			return int(kb / 1024)
		}
	}
	return 0
}

func oomKills() int {
	b, err := os.ReadFile(filepath.Join(cgroupRoot, "memory.events"))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "oom_kill "); ok {
			n, _ := strconv.Atoi(strings.TrimSpace(v))
			return n
		}
	}
	return 0
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
//
// Both hierarchies, for the same reason readCgroupCPUUsec reads both -- and
// this one did not, which is how a 2 vCPU Fargate task reported its CPU quota
// as "unlimited" for as long as the endpoint existed. cpu.max is v2 only, so
// on v1 the read simply failed and the zero fell through as no-quota. v1 keeps
// the quota and the period in two files and spells "no quota" as -1, where v2
// puts them on one line and spells it "max".
func readCgroupCPUMax() (float64, bool) {
	if b, err := os.ReadFile(filepath.Join(cgroupRoot, "cpu.max")); err == nil {
		return parseCPUMax(string(b))
	}
	quota, ok := readCgroupInt(filepath.Join(cgroupRoot, "cpu", "cpu.cfs_quota_us"))
	if !ok {
		return 0, false
	}
	// -1 is v1's "max": present, and no quota set.
	if quota < 0 {
		return 0, true
	}
	period, ok := readCgroupInt(filepath.Join(cgroupRoot, "cpu", "cpu.cfs_period_us"))
	if !ok || period <= 0 {
		return 0, false
	}
	return float64(quota) / float64(period), true
}

// readCgroupInt reads one integer out of a cgroup file.
func readCgroupInt(path string) (int64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return v, err == nil
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
func readNetDev() (rx, tx, errs, drops int64, ok bool) {
	b, err := os.ReadFile(filepath.Join(procRoot, "net", "dev"))
	if err != nil {
		return 0, 0, 0, 0, false
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
		num := func(i int) int64 {
			if i >= len(f) {
				return 0
			}
			v, _ := strconv.ParseInt(f[i], 10, 64)
			return v
		}
		// Receive: bytes packets errs drop ... Transmit begins at index 8 with
		// the same first four.
		rx += num(0)
		tx += num(8)
		errs += num(2) + num(10)
		drops += num(3) + num(11)
	}
	return rx, tx, errs, drops, true
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
	// The unit is platform-specific and getting it wrong is a factor-of-256
	// error that looks plausible -- see statfs_linux.go.
	unit := statfsUnit(&st)
	total := int64(st.Blocks) * unit
	free := int64(st.Bavail) * unit

	d := DiskUsage{
		Available: true,
		UsedMB:    (total - free) >> 20,
		TotalMB:   total >> 20,
		Path:      root,
	}
	if st.Files > 0 {
		d.InodesUsedPercent = float64(st.Files-st.Ffree) / float64(st.Files) * 100
	}
	return d
}
