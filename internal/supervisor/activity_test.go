package supervisor

import (
	"testing"
	"time"
)

// A running tool is any live descendant. The settle window that used to sit
// here was retired once both things it excluded were measured away: MCP servers
// cannot run in this image at all, and a real session spawns no long-lived
// children of its own.
func TestAnyLiveDescendantIsARunningTool(t *testing.T) {
	start := time.Now().Add(-time.Hour)
	procs := []ProcInfo{
		{PID: 10, SessionID: "s1", Name: "claude", State: "S",
			StartedAt: start.Format(time.RFC3339Nano)},
		{PID: 12, SessionID: "s1", Name: "make", State: "R",
			StartedAt: start.Add(30 * time.Minute).Format(time.RFC3339Nano)},
	}
	if !hasRunningTool(procs, "s1", 10) {
		t.Error("a long-running build was not detected; this is the session that " +
			"gets reaped an hour in")
	}
}

// The case the window actively broke, and the reason it is gone. A user
// attaches, starts a build within seconds, and walks away -- under a settle
// window that tool was read as a startup child and never counted, so the
// session was reaped mid-build by the very condition meant to protect it.
// Found by the end-to-end run rather than by reading.
func TestAToolStartedImmediatelyIsStillATool(t *testing.T) {
	start := time.Now()
	procs := []ProcInfo{
		{PID: 10, SessionID: "s1", Name: "sh", State: "S",
			StartedAt: start.Format(time.RFC3339Nano)},
		// One second after the session began.
		{PID: 12, SessionID: "s1", Name: "make", State: "R",
			StartedAt: start.Add(time.Second).Format(time.RFC3339Nano)},
	}
	if !hasRunningTool(procs, "s1", 10) {
		t.Error("a tool started in the session's first seconds was not counted")
	}
}

func TestRunningToolIgnoresOtherSessionsAndZombies(t *testing.T) {
	start := time.Now().Add(-time.Hour)
	late := start.Add(30 * time.Minute).Format(time.RFC3339Nano)

	// Another member's tool, in the same task. A workspace is shared, so this
	// is the normal case rather than a corner one -- counting it would let one
	// member's build hold another member's session open indefinitely.
	if hasRunningTool([]ProcInfo{
		{PID: 20, SessionID: "s2", Name: "make", State: "R", StartedAt: late},
	}, "s1", 10) {
		t.Error("another session's tool was attributed to this one")
	}

	// A finished process nobody has reaped. Counting these would hold a
	// workspace open on the strength of the litter a completed build left.
	if hasRunningTool([]ProcInfo{
		{PID: 21, SessionID: "s1", Name: "cc", State: "Z", StartedAt: late},
	}, "s1", 10) {
		t.Error("a zombie was counted as a running tool")
	}

	// The session's own process is never its own tool.
	if hasRunningTool([]ProcInfo{
		{PID: 10, SessionID: "s1", Name: "sh", State: "S", StartedAt: late},
	}, "s1", 10) {
		t.Error("the session's own root process was counted as a tool")
	}
}

func TestActivityTrackerSeedsToTheSessionStart(t *testing.T) {
	a := newActivityTracker()
	start := time.Now().Add(-10 * time.Minute)
	now := time.Now()

	// Never seen a tool: "no tool since it began" is a duration, not a special
	// case, so the seed is the session's own start rather than the zero time.
	if got := a.observe("s1", false, start, now); !got.Equal(start) {
		t.Errorf("first observation seeded to %v, want the session start %v", got, start)
	}
	// A tool running now moves it forward.
	if got := a.observe("s1", true, start, now); !got.Equal(now) {
		t.Errorf("a running tool did not update the mark: %v", got)
	}
	// And a later quiet poll must not move it BACK, or a build that just
	// finished would read as having been idle since the session started.
	later := now.Add(time.Minute)
	if got := a.observe("s1", false, start, later); !got.Equal(now) {
		t.Errorf("a quiet poll rewound the mark to %v, want %v", got, now)
	}

	a.forget("s1")
	if got := a.observe("s1", false, start, later); !got.Equal(start) {
		t.Errorf("forget did not drop the session: %v", got)
	}
}

// A clock that moved must not produce a negative "idle for", which would read
// as fresh activity by accident rather than by rule.
func TestSecsSinceClampsAtZero(t *testing.T) {
	now := time.Now()
	if got := secsSince(now, now.Add(time.Minute)); got != 0 {
		t.Errorf("secsSince returned %d for a future timestamp, want 0", got)
	}
	if got := secsSince(now, now.Add(-90*time.Second)); got != 90 {
		t.Errorf("secsSince = %d, want 90", got)
	}
}
