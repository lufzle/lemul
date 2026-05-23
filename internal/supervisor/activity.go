package supervisor

import (
	"sync"
	"time"

	"github.com/lufzle/lemul/internal/tunnel"
)

// What each session has been doing, for section 2.4's idle detection.
//
// The supervisor states facts; it does not decide. Idle timeouts, the
// per-session opt-out and what to do about a verdict all live in the Management
// API, which owns the policy, acts on it, and outlives the task it would be
// timing. What is computed HERE is only what cannot be computed anywhere else.
//
// Section 2.4's four conditions, and where each comes from:
//
//	no client attached  -- ptysession.Session.Attachers()
//	no PTY output       -- ptysession.Session.LastOutput()
//	no model requests   -- gateway.Broker.LastRequest()
//	no tool executing   -- /proc, below
//
// None of it needs OTel, which is what unblocks idle detection from the
// telemetry pipeline entirely. Where telemetry IS deployed it refines the
// verdict rather than enabling it.

// A running tool is ANY live descendant of a session, with no settle window.
//
// This used to exclude anything started within 15 s of the session, on the
// theory recorded in procfs.go's header: MCP servers come up with Claude Code
// and tool invocations start later, so start time tells them apart. Two
// measurements retired that idea from both ends.
//
//   - MCP servers in 2.1.220 are started LAZILY, not with the session, so a
//     window never separated them anyway -- and one is now barred from running
//     at all by the managed allowlist the image ships
//     (supervisor.TestMCPServersWouldBreakToolDetection).
//   - A real Claude Code session spawns ZERO long-lived children of its own,
//     measured in the real image
//     (e2e.TestARealSessionSpawnsNoLongLivedChildren).
//
// So the window had nothing left to exclude, while it still cost something
// real: a tool started in a session's first seconds was read as a startup child
// and never counted. A user who attaches, kicks off a long build and walks away
// would have had that session reaped mid-build -- which is the exact failure
// section 2.4's fourth condition exists to prevent, reachable through the
// mechanism meant to implement it. Found by the end-to-end run, not by reading.
//
// What would break this is precisely what those two tests guard: an MCP server
// in the image, or a future Claude Code that keeps a helper process alive. Both
// fail a test before they reach a workspace.

// activityTracker remembers when each session last had a tool running.
//
// It exists because "a tool is executing" is observed by polling while the four
// conditions are expressed as durations, and a poll can only ever say what is
// true right now. Sampling on the reporting tick is sufficient for what this
// condition is FOR: it exists to protect a long build, which spans many ticks by
// definition. A tool that starts and finishes between two ticks is invisible
// here, and that is fine -- a session running short tools is also producing PTY
// output and making model calls, so conditions two and three already hold it
// alive.
type activityTracker struct {
	mu       sync.Mutex
	lastTool map[string]time.Time
}

func newActivityTracker() *activityTracker {
	return &activityTracker{lastTool: make(map[string]time.Time)}
}

// observe records that a session had a tool running at t, and returns when it
// last did. A session seen for the first time is seeded with its own start,
// because "no tool since it began" and "no tool ever observed" are the same
// fact and only one of them is a duration.
func (a *activityTracker) observe(id string, running bool, startedAt, now time.Time) time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.lastTool[id]; !ok {
		a.lastTool[id] = startedAt
	}
	if running {
		a.lastTool[id] = now
	}
	return a.lastTool[id]
}

// forget drops a session's bookkeeping when its process ends.
func (a *activityTracker) forget(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.lastTool, id)
}

// sessionActivity builds section 2.4's per-session report.
func (s *Supervisor) sessionActivity() []tunnel.SessionActivity {
	sessions := s.mgr.List()
	if len(sessions) == 0 {
		return nil
	}

	pids := make(map[string]int, len(sessions))
	for _, sess := range sessions {
		if pid := sess.Pid(); pid > 0 {
			pids[sess.ID] = pid
		}
	}
	procs, procsOK := readProcesses(pids)

	now := time.Now()
	out := make([]tunnel.SessionActivity, 0, len(sessions))
	for _, sess := range sessions {
		// A signal that has never fired reports time since the session started,
		// so every field below is a real duration and the control plane never
		// has to special-case a zero.
		lastModel := sess.StartedAt
		if s.broker != nil {
			if t, ok := s.broker.LastRequest(sess.ID); ok {
				lastModel = t
			}
			// No listener for this session means the gateway is configured but
			// this session predates it or has already been closed. Falling back
			// to the session start is right: there is no evidence of a model
			// call, which is what the condition asks about.
		}

		// procsOK is false off Linux -- local development on darwin, where
		// procRoot does not exist. Unknown reads as BUSY here, unlike unknown
		// headroom, which reads as admit. The asymmetry is deliberate and is
		// not an inconsistency: for an activity signal the safe direction is
		// "do not reap", while for admission the safe direction is "do not
		// refuse". Both are refusals to act on something we cannot see.
		toolRunning := !procsOK
		if procsOK {
			toolRunning = hasRunningTool(procs, sess.ID, sess.Pid())
		}
		lastTool := s.activity.observe(sess.ID, toolRunning, sess.StartedAt, now)

		out = append(out, tunnel.SessionActivity{
			SessionID:      sess.ID,
			Attachers:      sess.Attachers(),
			OutputIdleSecs: secsSince(now, sess.LastOutput()),
			ModelIdleSecs:  secsSince(now, lastModel),
			ToolIdleSecs:   secsSince(now, lastTool),
		})
	}
	return out
}

// hasRunningTool reports whether a session has any live descendant.
func hasRunningTool(procs []ProcInfo, sessionID string, rootPID int) bool {
	for _, p := range procs {
		if p.SessionID != sessionID || p.PID == rootPID {
			continue
		}
		// A zombie is a finished process nobody has reaped, not a running tool.
		// Counting them would hold a workspace open on the strength of the
		// litter a completed build left behind.
		if p.State == "Z" {
			continue
		}
		return true
	}
	return false
}

// secsSince clamps at zero, because a duration that went negative would mean the
// clock moved and a negative "idle for" reads as fresh activity -- which is the
// safe direction, but only by accident. Making it explicit keeps it that way.
func secsSince(now, then time.Time) int {
	d := now.Sub(then)
	if d < 0 {
		return 0
	}
	return int(d.Seconds())
}
