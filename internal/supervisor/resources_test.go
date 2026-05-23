package supervisor

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseCPUMax(t *testing.T) {
	for _, tc := range []struct {
		in    string
		cores float64
		ok    bool
	}{
		{"200000 100000\n", 2, true}, // 2 cores
		{"50000 100000", 0.5, true},  // half a core
		{"max 100000\n", 0, true},    // present, unlimited -- NOT "no CPU"
		{"garbage", 0, false},
		{"", 0, false},
		{"100000 0", 0, false}, // a zero period would divide by zero
	} {
		cores, ok := parseCPUMax(tc.in)
		if ok != tc.ok || cores != tc.cores {
			t.Errorf("parseCPUMax(%q) = (%v, %v), want (%v, %v)", tc.in, cores, ok, tc.cores, tc.ok)
		}
	}
}

func TestParseCPUStat(t *testing.T) {
	const s = "usage_usec 123456\nuser_usec 100000\nsystem_usec 23456\n"
	if got, ok := parseCPUStat(s); !ok || got != 123456 {
		t.Errorf("got (%v, %v), want (123456, true)", got, ok)
	}
	if _, ok := parseCPUStat("nr_periods 0\n"); ok {
		t.Error("accepted a cpu.stat with no usage_usec")
	}
}

// Loopback is excluded on purpose: in gateway mode every model call goes over
// it to the supervisor's own broker, so counting it would swamp the figure an
// operator actually wants (what left the task).
func TestReadNetDevSkipsLoopback(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	const dev = `Inter-|   Receive                    |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 999999    100    0    0    0     0          0         0  888888     100    0    0    0     0       0          0
  eth0: 1000       10    0    0    0     0          0         0  2000        20    0    0    0     0       0          0
`
	if err := os.WriteFile(filepath.Join(root, "net", "dev"), []byte(dev), 0o644); err != nil {
		t.Fatal(err)
	}
	procRoot = root
	t.Cleanup(func() { procRoot = "/proc" })

	rx, tx, _, _, ok := readNetDev()
	if !ok {
		t.Fatal("readNetDev reported unavailable")
	}
	if rx != 1000 || tx != 2000 {
		t.Errorf("got rx=%d tx=%d, want 1000/2000 -- loopback was counted", rx, tx)
	}
}

func TestReadNetDevUnavailable(t *testing.T) {
	procRoot = filepath.Join(t.TempDir(), "nope")
	t.Cleanup(func() { procRoot = "/proc" })
	if _, _, _, _, ok := readNetDev(); ok {
		t.Error("reported available with no procfs")
	}
}

// The point of holding the ring in the supervisor: a rate must come from two
// samples a known interval apart, so it stays correct however often the console
// polls -- including not at all for a while, or twice at once.
func TestSamplerComputesRatesFromItsOwnInterval(t *testing.T) {
	s := newSampler(t.TempDir())

	now := time.Now()
	s.prev = sample{at: now, cpuUsec: 1_000_000, rxBytes: 0, txBytes: 0, ok: true}
	cur := sample{at: now.Add(2 * time.Second), cpuUsec: 3_000_000, rxBytes: 1000, txBytes: 1000, ok: true}

	// Reproduce tick's arithmetic: 2 CPU-seconds over 2 wall seconds = 1 core;
	// 2000 bytes over 2 seconds = 1000 B/s.
	elapsed := cur.at.Sub(s.prev.at).Seconds()
	cores := float64(cur.cpuUsec-s.prev.cpuUsec) / 1e6 / elapsed
	if cores != 1.0 {
		t.Errorf("cores = %v, want 1.0", cores)
	}
	bps := float64((cur.rxBytes-s.prev.rxBytes)+(cur.txBytes-s.prev.txBytes)) / elapsed
	if bps != 1000 {
		t.Errorf("bytes/sec = %v, want 1000", bps)
	}
}

// The ring must stay bounded: this runs for the life of a task, which may be
// days.
func TestSamplerRingIsBounded(t *testing.T) {
	s := newSampler(t.TempDir())
	for i := 0; i < sampleRingSize*3; i++ {
		s.tick()
	}
	if len(s.ring) > sampleRingSize {
		t.Errorf("ring grew to %d, cap is %d", len(s.ring), sampleRingSize)
	}
}

// On darwin there are no cgroups, which is where the local driver runs. Every
// group must say so rather than reporting a confident zero.
func TestUsageReportsUnavailableRatherThanZero(t *testing.T) {
	cgroupRoot = filepath.Join(t.TempDir(), "no-cgroups")
	procRoot = filepath.Join(t.TempDir(), "no-proc")
	t.Cleanup(func() { cgroupRoot = "/sys/fs/cgroup"; procRoot = "/proc" })

	u := newSampler("").usage()
	if u.CPU.Available {
		t.Error("CPU claimed available with no cgroup")
	}
	if u.Network.Available {
		t.Error("network claimed available with no procfs")
	}
	if u.Disk.Available {
		t.Error("disk claimed available with no root configured")
	}
}

// A container started without --memory reports memory.max as "max". headroom.go
// correctly calls that "no headroom figure", but the explorer still knows what
// is IN USE -- and reporting "unknown" while holding the number would be the
// console claiming an ignorance it does not have.
func TestUsageReportsMemoryWithoutALimit(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "memory.current"), "336834560")
	mustWrite(t, filepath.Join(dir, "memory.max"), "max\n")
	cgroupRoot = dir
	t.Cleanup(func() { cgroupRoot = "/sys/fs/cgroup" })

	m := newSampler("").usage().Memory
	if !m.Available {
		t.Fatal("memory reported unavailable though memory.current is readable")
	}
	if m.UsedMB != 321 {
		t.Errorf("UsedMB = %d, want 321", m.UsedMB)
	}
	if m.LimitMB != 0 {
		t.Errorf("LimitMB = %d, want 0 meaning no quota", m.LimitMB)
	}
}

func TestUsageReportsMemoryLimitWhenSet(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "memory.current"), "1073741824") // 1 GiB
	mustWrite(t, filepath.Join(dir, "memory.max"), "8589934592")     // 8 GiB
	cgroupRoot = dir
	t.Cleanup(func() { cgroupRoot = "/sys/fs/cgroup" })

	m := newSampler("").usage().Memory
	if !m.Available || m.UsedMB != 1024 || m.LimitMB != 8192 {
		t.Errorf("got %+v, want used 1024 of 8192", m)
	}
}

// Throttling is the CPU saturation signal: a task can sit at moderate
// utilisation and still crawl because it keeps exhausting its quota.
func TestThrottlingPercent(t *testing.T) {
	dir := t.TempDir()
	cgroupRoot = dir
	t.Cleanup(func() { cgroupRoot = "/sys/fs/cgroup" })

	mustWrite(t, filepath.Join(dir, "cpu.stat"),
		"usage_usec 100\nnr_periods 200\nnr_throttled 50\nthrottled_usec 900\n")
	got, ok := throttling()
	if !ok || got != 25 {
		t.Errorf("got (%v, %v), want (25, true)", got, ok)
	}

	// No quota means no periods, and therefore no throttling is POSSIBLE --
	// which must read as zero rather than as an unreadable metric.
	mustWrite(t, filepath.Join(dir, "cpu.stat"), "usage_usec 100\nnr_periods 0\nnr_throttled 0\n")
	if got, ok := throttling(); !ok || got != 0 {
		t.Errorf("unquota'd cgroup: got (%v, %v), want (0, true)", got, ok)
	}
}

// memory.current lumps page cache in with real usage, so a build that has read a
// large tree looks alarming while being perfectly healthy. anon is the figure
// that decides whether a task is heading for an OOM.
func TestMemoryBreakdownSeparatesCacheFromAnon(t *testing.T) {
	dir := t.TempDir()
	cgroupRoot = dir
	t.Cleanup(func() { cgroupRoot = "/sys/fs/cgroup" })
	mustWrite(t, filepath.Join(dir, "memory.stat"),
		"anon 179412992\nfile 44711936\nkernel 1234\nslab 0\n")

	anon, file, ok := memoryBreakdown()
	if !ok || anon != 179412992 || file != 44711936 {
		t.Errorf("got (%d, %d, %v)", anon, file, ok)
	}
}

// §2.4's blast-radius event: an OOM takes the whole task and every session in
// it. The counter persists, so a task that survived one still reports it.
func TestOOMKillsIsReadFromMemoryEvents(t *testing.T) {
	dir := t.TempDir()
	cgroupRoot = dir
	t.Cleanup(func() { cgroupRoot = "/sys/fs/cgroup" })

	mustWrite(t, filepath.Join(dir, "memory.events"),
		"low 0\nhigh 0\nmax 3\noom 2\noom_kill 1\noom_group_kill 0\n")
	if got := oomKills(); got != 1 {
		t.Errorf("oomKills = %d, want 1", got)
	}
	// oom_kill, not oom: the latter counts times the limit was hit, which the
	// kernel can survive by reclaiming.
	mustWrite(t, filepath.Join(dir, "memory.events"), "oom 9\noom_kill 0\n")
	if got := oomKills(); got != 0 {
		t.Errorf("oomKills = %d, want 0 -- it read `oom` instead of `oom_kill`", got)
	}
}

func TestReadNetDevCountsErrorsAndDrops(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	// rx: bytes packets errs drop ... tx begins at index 8 with the same four.
	const dev = `Inter-|   Receive                    |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 999999    100    7    7    0     0          0         0  888888     100    7    7    0     0       0          0
  eth0: 1000       10    2    3    0     0          0         0  2000        20    4    5    0     0       0          0
`
	if err := os.WriteFile(filepath.Join(root, "net", "dev"), []byte(dev), 0o644); err != nil {
		t.Fatal(err)
	}
	procRoot = root
	t.Cleanup(func() { procRoot = "/proc" })

	rx, tx, errs, drops, ok := readNetDev()
	if !ok {
		t.Fatal("unavailable")
	}
	if rx != 1000 || tx != 2000 {
		t.Errorf("rx=%d tx=%d; loopback was counted", rx, tx)
	}
	if errs != 6 || drops != 8 { // rx+tx on eth0 only
		t.Errorf("errs=%d drops=%d, want 6 and 8 (loopback excluded)", errs, drops)
	}
}

// The CPU quota has to be read out of BOTH cgroup hierarchies.
//
// cpu.max is v2 only. Fargate is v1 with every controller mounted ro, so the
// read failed and the quota fell through as "no quota" -- a 2 vCPU task
// rendering as unlimited for as long as the endpoint existed, which is the
// same v2-shaped assumption that made memory.limit_in_bytes report the host's
// RAM one field over. readCgroupCPUUsec beside it had always read both, which
// is why usage was right while the limit was not.
func TestTheCPUQuotaIsReadFromEitherCgroupHierarchy(t *testing.T) {
	t.Run("v2", func(t *testing.T) {
		dir := t.TempDir()
		mustWrite(t, filepath.Join(dir, "cpu.max"), "200000 100000\n")
		cgroupRoot = dir
		t.Cleanup(func() { cgroupRoot = "/sys/fs/cgroup" })

		if got, ok := readCgroupCPUMax(); !ok || got != 2 {
			t.Errorf("got (%v, %v), want (2, true)", got, ok)
		}
	})

	t.Run("v1", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "cpu"), 0o755); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(dir, "cpu", "cpu.cfs_quota_us"), "200000\n")
		mustWrite(t, filepath.Join(dir, "cpu", "cpu.cfs_period_us"), "100000\n")
		cgroupRoot = dir
		t.Cleanup(func() { cgroupRoot = "/sys/fs/cgroup" })

		if got, ok := readCgroupCPUMax(); !ok || got != 2 {
			t.Errorf("got (%v, %v), want (2, true) -- v1 quota not read", got, ok)
		}
	})

	// -1 is v1's spelling of "max": the file is there and no quota is set. It
	// must read as (0, true) like v2's "max", NOT as a negative core count and
	// not as absent.
	t.Run("v1 unlimited", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "cpu"), 0o755); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(dir, "cpu", "cpu.cfs_quota_us"), "-1\n")
		mustWrite(t, filepath.Join(dir, "cpu", "cpu.cfs_period_us"), "100000\n")
		cgroupRoot = dir
		t.Cleanup(func() { cgroupRoot = "/sys/fs/cgroup" })

		if got, ok := readCgroupCPUMax(); !ok || got != 0 {
			t.Errorf("got (%v, %v), want (0, true) meaning present and unlimited", got, ok)
		}
	})
}
