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
		{"200000 100000\n", 2, true},   // 2 cores
		{"50000 100000", 0.5, true},    // half a core
		{"max 100000\n", 0, true},      // present, unlimited -- NOT "no CPU"
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

	rx, tx, ok := readNetDev()
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
	if _, _, ok := readNetDev(); ok {
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
	bps := float64((cur.rxBytes - s.prev.rxBytes) + (cur.txBytes - s.prev.txBytes)) / elapsed
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
	mustWrite(t, filepath.Join(dir, "memory.current"), "1073741824")  // 1 GiB
	mustWrite(t, filepath.Join(dir, "memory.max"), "8589934592")      // 8 GiB
	cgroupRoot = dir
	t.Cleanup(func() { cgroupRoot = "/sys/fs/cgroup" })

	m := newSampler("").usage().Memory
	if !m.Available || m.UsedMB != 1024 || m.LimitMB != 8192 {
		t.Errorf("got %+v, want used 1024 of 8192", m)
	}
}
