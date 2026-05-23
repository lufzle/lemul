package supervisor

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// fargateSentinel is what a real Fargate task reports for
// memory.limit_in_bytes: LONG_MAX page-aligned, meaning "no limit set at this
// level". Measured 2026-08-03, and it is the whole reason this file exists.
const fargateSentinel = "9223372036854771712"

// cgroupV1 writes the layout Fargate actually has -- a controller per mount,
// v1 filenames -- with the given limit and usage.
func cgroupV1(t *testing.T, limit, usage string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, v string) {
		if err := os.WriteFile(filepath.Join(root, "memory", name), []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("memory.limit_in_bytes", limit)
	write("memory.usage_in_bytes", usage)
	old := cgroupRoot
	cgroupRoot = root
	t.Cleanup(func() { cgroupRoot = old })
}

// metadataServer stands in for 169.254.170.2, and reports how often it was
// asked -- which is how "the cgroup wins when it has a real limit" is asserted
// as *not consulting* it rather than as agreeing by coincidence.
func metadataServer(t *testing.T, body string) *int {
	t.Helper()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/task" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		hits++
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	t.Setenv(ecsMetadataURIEnv, srv.URL)
	resetTaskLimit(t)
	return &hits
}

// resetTaskLimit clears the cache. The limit is fixed at launch in production,
// so it is read once; a test that did not reset it would assert the previous
// test's answer.
func resetTaskLimit(t *testing.T) {
	t.Helper()
	clear := func() {
		taskLimit.Lock()
		taskLimit.done, taskLimit.bytes, taskLimit.cores = false, 0, 0
		taskLimit.Unlock()
	}
	clear()
	t.Cleanup(clear)
}

// THE BUG THIS FILE EXISTS FOR. On Fargate the container's own cgroup honestly
// reports "unlimited" while the platform enforces 8 GiB above it, so admission
// control saw "unknown headroom" and decision #9's unknown-admits rule let every
// session in -- on the one substrate admission control is for.
func TestHeadroomUsesTheTaskLimitWhenTheCgroupSaysUnlimited(t *testing.T) {
	cgroupV1(t, fargateSentinel, "1073741824") // 1 GiB in use
	metadataServer(t, `{"Limits":{"CPU":2,"Memory":8192}}`)

	free, limit := memoryHeadroomMB()
	if limit != 8192 {
		t.Fatalf("limit = %d MB, want 8192 -- a zero here is 'unknown', and "+
			"unknown ADMITS, so admission control does nothing on Fargate", limit)
	}
	if free != 7168 {
		t.Errorf("free = %d MB, want 7168 (8192 - 1024)", free)
	}
}

// The cgroup stays the first source wherever it is honest: docker with
// --memory, and any local run. Asserted by the metadata endpoint going
// untouched, not by the numbers happening to match.
func TestTheCgroupLimitWinsWhenItIsRealAndNothingElseIsAsked(t *testing.T) {
	cgroupV1(t, "2147483648", "536870912") // 2 GiB limit, 512 MiB used
	hits := metadataServer(t, `{"Limits":{"Memory":8192}}`)

	free, limit := memoryHeadroomMB()
	if limit != 2048 {
		t.Fatalf("limit = %d MB, want 2048 from the cgroup", limit)
	}
	if free != 1536 {
		t.Errorf("free = %d MB, want 1536", free)
	}
	if *hits != 0 {
		t.Errorf("asked the task metadata %d time(s) while the cgroup had a real "+
			"limit; the cgroup is the honest source wherever it is set", *hits)
	}
}

// Off ECS entirely -- the local driver on darwin, or docker without --memory --
// there is no limit anywhere and "unknown" is the correct, documented answer.
// Callers must keep reading it as unknown rather than as zero headroom.
func TestHeadroomIsUnknownWithNoLimitAndNoMetadata(t *testing.T) {
	cgroupV1(t, fargateSentinel, "1073741824")
	t.Setenv(ecsMetadataURIEnv, "")
	resetTaskLimit(t)

	if free, limit := memoryHeadroomMB(); free != 0 || limit != 0 {
		t.Fatalf("memoryHeadroomMB() = (%d, %d), want (0, 0) so the caller keeps "+
			"treating it as unknown", free, limit)
	}
}

// A metadata endpoint that answers rubbish must read as unknown, not as a limit
// of zero -- which would refuse every session instead of admitting them.
//
// The NEGATIVE case is why the guard is `<= 0` rather than `== 0`, and it is in
// this table because leaving it out let a mutant survive: at zero the guard is
// invisible, since 0<<20 is 0 whether it fires or not. A negative slips through
// as a negative limit, which reads as a real one -- non-zero, so not "unknown" --
// and every admission decision is then made against a nonsense number.
func TestUnusableTaskMetadataReadsAsUnknown(t *testing.T) {
	for _, body := range []string{
		`{"Limits":{"Memory":0}}`,
		`{"Limits":{"Memory":-1}}`,
		`{}`,
		`not json`,
	} {
		cgroupV1(t, fargateSentinel, "1073741824")
		metadataServer(t, body)
		if _, limit := memoryHeadroomMB(); limit != 0 {
			t.Errorf("body %q gave limit %d, want 0 (unknown)", body, limit)
		}
	}
}

// The same v2-shaped assumption one field over: memory.stat is a different file
// with different key names under v1, and reading only v2 fails SILENTLY --
// every key misses and it returns a confident zero. Fargate is v1, so anon and
// cache were flat zero on every real task, which is what the console plots.
func TestMemoryBreakdownReadsTheV1Layout(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	stat := "cache 4096\nrss 2048\ntotal_cache 209715200\ntotal_rss 419430400\n"
	if err := os.WriteFile(filepath.Join(root, "memory", "memory.stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
	old := cgroupRoot
	cgroupRoot = root
	t.Cleanup(func() { cgroupRoot = old })

	anon, file, ok := memoryBreakdown()
	if !ok {
		t.Fatal("memoryBreakdown reported unavailable on a v1 tree")
	}
	if anon != 419430400 {
		t.Errorf("anon = %d, want 419430400 (total_rss)", anon)
	}
	if file != 209715200 {
		t.Errorf("file = %d, want 209715200 (total_cache)", file)
	}
}

// CPU has the same two-source problem as memory, and had only one source.
//
// Fargate's container cgroup carries no CPU quota -- the cap sits above it,
// exactly as the memory cap does -- so a 2 vCPU task reported its quota as 0,
// which the type documents as "unlimited". The console then drew an unbounded
// CPU axis for a task that is very much bounded. Measured on the deployed
// stack 2026-08-03: cpu.limit 0 beside memory.limit_mb 8192, the memory half
// having been fixed the same day.
func TestTheCPUQuotaFallsBackToTheTaskMetadata(t *testing.T) {
	cgroupV1(t, fargateSentinel, "1073741824") // no cpu/ tree at all
	metadataServer(t, `{"Limits":{"CPU":2,"Memory":8192}}`)

	if got := taskCPULimitCores(); got != 2 {
		t.Errorf("taskCPULimitCores() = %v, want 2 from the platform; a zero here "+
			"reads as 'no quota' for a task that has one", got)
	}
}

// The cgroup stays the first source wherever it is honest, asserted by the
// metadata going unasked rather than by the numbers agreeing.
func TestTheCgroupCPUQuotaWinsWhenItIsReal(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "cpu"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]string{
		"cpu.cfs_quota_us":  "400000",
		"cpu.cfs_period_us": "100000",
	} {
		if err := os.WriteFile(filepath.Join(root, "cpu", name), []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := cgroupRoot
	cgroupRoot = root
	t.Cleanup(func() { cgroupRoot = old })

	hits := metadataServer(t, `{"Limits":{"CPU":2,"Memory":8192}}`)

	if got := taskCPULimitCores(); got != 4 {
		t.Errorf("taskCPULimitCores() = %v, want 4 from the cgroup", got)
	}
	if *hits != 0 {
		t.Errorf("asked the task metadata %d time(s) while the cgroup had a real quota", *hits)
	}
}

// Nothing anywhere is "no quota", and it must stay 0 rather than becoming a
// nonsense number -- the same reason the memory guard is `<= 0`.
func TestUnusableCPUMetadataReadsAsNoQuota(t *testing.T) {
	for _, body := range []string{
		`{"Limits":{"CPU":0,"Memory":8192}}`,
		`{"Limits":{"CPU":-1,"Memory":8192}}`,
		`{}`,
		`not json`,
	} {
		cgroupV1(t, fargateSentinel, "1073741824")
		metadataServer(t, body)
		if got := taskCPULimitCores(); got != 0 {
			t.Errorf("body %q gave %v cores, want 0", body, got)
		}
	}
}
