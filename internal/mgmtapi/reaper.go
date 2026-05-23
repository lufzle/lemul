package mgmtapi

import (
	"context"
	"time"

	"github.com/lufzle/lemul/internal/store"
	"github.com/lufzle/lemul/internal/tunnel"
)

// Section 2.4's auto-stop cascade: session idle-stop -> last session stops ->
// warm hold -> workspace stops.
//
// It runs HERE rather than in the supervisor because the supervisor cannot
// outlive the task it would be timing, and because policy lives with the
// service that owns it and acts on it. The supervisor states facts up the
// control tunnel; this decides what they mean.
//
// Two rules run through everything below, and they are the same principle
// pointing in opposite directions:
//
//   - SILENCE IS NOT IDLE. A missing or stale report means the sweeper does
//     nothing. A wedged tunnel must never look like an idle workspace, or the
//     failure mode is reaping live work -- including a colleague's unattended
//     run, since a workspace is shared.
//   - UNKNOWN HEADROOM ADMITS (admission.go). Refusing on an unknown would fail
//     closed against our own operators rather than against real saturation.
//
// Both are refusals to act on something we cannot see. They look different only
// because "do nothing" means "do not stop" in one and "do not refuse" in the
// other.
//
// REPLICA SAFETY IS FREE, and worth stating because it is not obvious: the
// sweeper acts only on workspaces whose report is in ITS OWN memory, and a
// workspace task holds its control tunnel with exactly one control-plane
// process. N replicas therefore partition rather than race, which is what makes
// desiredCount > 1 safe later without a lease or a leader election.

// sweepInterval is how often the cascade is evaluated. Idle is a minutes-scale
// decision, so this does not need to be fast -- and being slower than the
// supervisor's reporting interval would mean acting on numbers already
// superseded.
const sweepInterval = 30 * time.Second

// runReaper drives the cascade until the server shuts down.
func (s *Server) runReaper(ctx context.Context) {
	t := time.NewTicker(s.sweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.sweep(ctx, now)
		}
	}
}

// sweep evaluates every workspace with a live report exactly once.
func (s *Server) sweep(ctx context.Context, now time.Time) {
	for ref, rep := range s.activity.all() {
		if !rep.fresh(now, s.headroomInterval) {
			// Rule: silence is not idle. A supervisor that has stopped
			// reporting tells us nothing about its sessions, and "nothing" is
			// not evidence of inactivity.
			continue
		}
		s.sweepWorkspace(ctx, ref, rep, now)
	}
}

func (s *Server) sweepWorkspace(ctx context.Context, ref wsRef, rep activityReport, now time.Time) {
	// A scope with a tenant and no acting user: every query below is scoped by
	// tenant_id, and inventing a user would put a fiction into app.user_id.
	sc := scope{Scope: store.Scope{TenantID: ref.tenantID}}

	ws, err := s.getWorkspaceByName(ctx, sc, ref.name)
	if err != nil {
		// The record is gone but a report is still arriving: the workspace was
		// deleted and its task has not noticed yet. Deleting drops the record
		// and stops the task, so there is nothing left to decide.
		s.activity.forget(ref)
		return
	}

	pinned := s.stopIdleSessions(ctx, sc, ws, rep)
	s.notePinned(ctx, sc, ws, pinned, now)
	s.advanceWarmHold(ctx, sc, ws, rep, now)
}

// stopIdleSessions applies section 2.4's four conditions and reports whether the
// workspace is being held open ONLY by background processes -- see notePinned.
func (s *Server) stopIdleSessions(ctx context.Context, sc scope, ws Workspace, rep activityReport) (pinned bool) {
	if len(rep.Headroom.Activity) == 0 {
		return false
	}
	// The per-session override lives on the record, not in the report: it is
	// policy, and the task is deliberately not told any.
	overrides := make(map[string]*int32)
	sessions, err := s.listWorkspaceSessions(ctx, sc, ws.ID)
	if err != nil {
		// Without the records we cannot tell an opted-out session from a
		// default one, and stopping on that guess is exactly the mistake these
		// rules exist to prevent.
		s.log.Warn("reading sessions for idle detection", "workspace", ws.Name, "error", err)
		return false
	}
	for _, sess := range sessions {
		overrides[sess.ID] = sess.IdleTimeoutSecs
	}

	t, err := s.reg.PickWorkspace(sc.TenantID, ws.Name)
	if err != nil {
		return false // the task went away between the report and now
	}

	anyBusy, anyPinned := false, false
	for _, a := range rep.Headroom.Activity {
		override, known := overrides[a.SessionID]
		if !known {
			// A PTY the control plane has no record of. Not ours to reap.
			continue
		}
		timeout, on := idleTimeout(ws, override)
		if !on {
			// Opted out, per session or per workspace. Counted as busy rather
			// than pinned: somebody said in as many words that this session is
			// meant to sit there, so it is not a surprise worth warning about.
			anyBusy = true
			continue
		}
		switch classify(a, timeout) {
		case sessionBusy:
			anyBusy = true
			continue
		case sessionPinned:
			anyPinned = true
			continue
		case sessionIdle:
		}
		// Ctrl-C/Ctrl-D semantics, the same verb a user gets. The conversation
		// survives on the workspace volume, so this is an interruption rather
		// than data loss, and --resume picks the history back up.
		env, err := command(t, tunnel.MsgStopSession, tunnel.StopSession{
			SessionID: a.SessionID,
		}, 15*time.Second)
		if err != nil || env.Type == tunnel.MsgError {
			s.log.Warn("stopping an idle session", "workspace", ws.Name,
				"session", a.SessionID, "error", err)
			continue
		}
		s.log.Info("session stopped: idle", "workspace", ws.Name, "session", a.SessionID,
			"timeout", timeout, "output_idle_s", a.OutputIdleSecs,
			"model_idle_s", a.ModelIdleSecs, "tool_idle_s", a.ToolIdleSecs)
	}
	// Only when NOTHING else is going on. If a colleague is working in this
	// workspace it is legitimately up, and a warning about somebody's dev server
	// would be noise on a row that is behaving perfectly normally.
	return anyPinned && !anyBusy
}

// notePinned records that this workspace would be stopping, and the only thing
// holding it open is a background process inside a session.
//
// It changes NO behaviour, and that is the design. The member who started that
// process meant to -- a dev server left up for a colleague to hit is a use case
// section 2.3 explicitly expects -- so stopping the session would destroy work
// somebody chose to leave running. What is missing is not enforcement, it is
// VISIBILITY: nobody goes home on a Friday intending to bill all weekend, they
// just cannot see that they are. The console renders this on the workspace row.
//
// Recorded with a timestamp rather than a flag so the row can say HOW LONG,
// which is the part that makes it act-on-able -- and it is the same field a
// ceiling would key off, should this ever need one.
func (s *Server) notePinned(ctx context.Context, sc scope, ws Workspace, pinned bool, now time.Time) {
	switch {
	case pinned && ws.IdlePinnedSince == nil:
		if err := s.setIdlePinned(ctx, sc, ws.ID, &now); err != nil {
			s.log.Error("recording an idle-pinned workspace", "workspace", ws.Name, "error", err)
			return
		}
		s.log.Info("workspace held open by a background process", "workspace", ws.Name,
			"since", now)
	case !pinned && ws.IdlePinnedSince != nil:
		if err := s.setIdlePinned(ctx, sc, ws.ID, nil); err != nil {
			s.log.Error("clearing an idle-pinned workspace", "workspace", ws.Name, "error", err)
			return
		}
		s.log.Info("workspace no longer held open by a background process",
			"workspace", ws.Name)
	}
}

// sessionState is what one session is doing, on section 2.4's terms.
type sessionState int

const (
	// sessionBusy: somebody is attached, or the PTY is producing output, or
	// model requests are going out. The session is in use.
	sessionBusy sessionState = iota
	// sessionPinned: quiet on every axis a PERSON drives, but a tool is still
	// running. This is the overnight build, and it is also the dev server
	// somebody left up. The two are indistinguishable from here and are
	// treated the same way -- not stopped.
	sessionPinned
	// sessionIdle: all four conditions. Nothing is happening.
	sessionIdle
)

// classify applies section 2.4's four conditions, keeping the tool apart from
// the other three.
//
// The split matters because the two outcomes call for opposite actions. All four
// quiet means stop. Three quiet with a tool running means DO NOT stop -- a
// one-hour build, a long test run, or anything blocked on the network produces
// no PTY output and no model calls, because Claude Code is sitting blocked
// waiting on the tool. Without that distinction the other three go quiet exactly
// when the workspace is busiest, and the build is reaped an hour in.
func classify(a tunnel.SessionActivity, timeout time.Duration) sessionState {
	secs := int(timeout.Seconds())
	if a.Attachers > 0 || a.OutputIdleSecs < secs || a.ModelIdleSecs < secs {
		return sessionBusy
	}
	if a.ToolIdleSecs < secs {
		return sessionPinned
	}
	return sessionIdle
}

func isIdle(a tunnel.SessionActivity, timeout time.Duration) bool {
	return classify(a, timeout) == sessionIdle
}

// advanceWarmHold runs the compute axis: start a hold when the last session
// goes, clear it when one comes back, stop the task when it expires.
//
// The deadline is a COLUMN. A timer in this process would die with it, and a
// redeploy mid-hold would leave the task running with nobody timing it -- the
// exact failure warm hold exists to prevent, made permanent.
func (s *Server) advanceWarmHold(ctx context.Context, sc scope, ws Workspace, rep activityReport, now time.Time) {
	// The report's own count, not one recomputed after the stops above. Those
	// take effect on the next tick, and mixing two views of the same workspace
	// in one pass is how a task gets stopped out from under a session that was
	// still starting.
	if rep.Headroom.Sessions > 0 {
		if ws.WarmHoldUntil != nil {
			if err := s.setWarmHold(ctx, sc, ws.ID, nil); err != nil {
				s.log.Error("clearing a warm hold", "workspace", ws.Name, "error", err)
				return
			}
			s.log.Info("warm hold cleared: a session is running", "workspace", ws.Name)
		}
		return
	}

	if ws.WarmHoldUntil == nil {
		until := now.Add(time.Duration(ws.WarmHoldSecs) * time.Second)
		if err := s.setWarmHold(ctx, sc, ws.ID, &until); err != nil {
			s.log.Error("starting a warm hold", "workspace", ws.Name, "error", err)
			return
		}
		s.log.Info("warm hold started", "workspace", ws.Name, "until", until,
			"secs", ws.WarmHoldSecs)
		return
	}

	if now.Before(*ws.WarmHoldUntil) {
		return
	}

	// THE stop that has to preserve. This is the one that runs on its own, so a
	// disk lost here is lost to nobody's decision -- section 2.4's cascade exists
	// to stop paying for an idle workspace, and until the volume landed it also
	// destroyed every member's home one warm hold after the last session
	// ended (e2e.TestWithoutAVolumeTheCascadeDestroysEveryHome).
	s.stopTask(ctx, sc.TenantID, ws.Name, derefOr(ws.TaskRef), "warm hold expired",
		keepDisk(sc, ws.ID))
	if err := s.setWorkspaceStatus(ctx, sc, ws.ID, WorkspaceStopped, ""); err != nil {
		s.log.Error("recording a stopped workspace", "workspace", ws.Name, "error", err)
	}
	if err := s.setWarmHold(ctx, sc, ws.ID, nil); err != nil {
		s.log.Error("clearing an expired warm hold", "workspace", ws.Name, "error", err)
	}
	// Its numbers describe a task that no longer exists, and the next placement
	// must not be judged by them.
	s.activity.forget(wsRef{sc.TenantID, ws.Name})
	s.log.Info("workspace stopped: warm hold expired", "workspace", ws.Name)
}
