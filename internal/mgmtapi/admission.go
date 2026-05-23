package mgmtapi

import (
	"fmt"
	"time"

	"github.com/lufzle/lemul/internal/tunnel"
)

// Admission control (section 2.4).
//
// A Fargate task's size is fixed at launch and cannot grow, so the control
// plane admits or refuses a new session rather than letting an OOM be the
// discovery mechanism. Since Phase 6 that is not a billing question: a workspace
// is a shared machine, so an OOM kills the task and with it EVERY member's
// sessions -- one developer's runaway build ends a colleague's overnight run.
//
// The decision is a pure function over a report, deliberately. It is the part
// worth being able to test exhaustively without a container, and every rule
// below is a rule about what to do with something we are NOT sure of.

// Admission policies, mirroring the CHECK in schema.sql.
const (
	admitUnlimited   = "unlimited"
	admitMaxSessions = "max_sessions"
	admitMinFreePct  = "min_free_memory_pct"
)

// minFreeFloorMB is the absolute headroom below which no session is admitted,
// whatever the percentage works out to.
//
// A CONSTANT rather than a column, because it is a property of what Claude Code
// costs rather than of what a customer wants. The percentage adapts across task
// tiers, which is why it is the policy -- but a session's footprint does NOT
// scale with task size, so 20% of a 2 GiB task is ~410 MB, less than one
// session, while 20% of 16 GiB is generous. A percentage alone is wrong at the
// small end in exactly the direction that causes an OOM.
const minFreeFloorMB = 768

// admissionPolicy is what a workspace's record says about taking new sessions.
type admissionPolicy struct {
	Policy      string
	MaxSessions int
	MinFreePct  int
}

// verdict is the outcome, with the reason attached. The reason is not
// decoration: it is what the caller shows a user, in the style the Bedrock
// preflight set -- name the policy and the numbers, so "it refused" is
// diagnosable without reading a log.
type verdict struct {
	Admit  bool
	Reason string
}

// admits decides whether a workspace can take another session.
//
// `report` is the supervisor's latest, `have` says whether there is one at all,
// and `fresh` whether it is recent enough to act on. A workspace with no live
// tunnel has no report, which is the cold case: nothing is running, so nothing
// can be saturated.
func admits(p admissionPolicy, report tunnel.Headroom, have, fresh bool) verdict {
	if p.Policy == admitUnlimited {
		return verdict{Admit: true}
	}
	if !have {
		// No task, therefore no sessions, therefore room. Refusing here would
		// make a cold workspace impossible to start.
		return verdict{Admit: true}
	}

	byCount := func() verdict {
		if report.Sessions >= p.MaxSessions {
			return verdict{Reason: fmt.Sprintf(
				"this workspace already has %d of %d sessions; stop one, or raise max_sessions",
				report.Sessions, p.MaxSessions)}
		}
		return verdict{Admit: true}
	}

	if p.Policy == admitMaxSessions {
		return byCount()
	}

	// min_free_memory_pct from here down.
	//
	// RULE: unknown headroom ADMITS. memoryHeadroomMB() reports a zero limit on
	// darwin and on an unlimited cgroup, and refusing on that would make every
	// local development setup reject every session -- a policy that fails
	// closed against its own operators rather than against real saturation.
	if report.MemLimitMB == 0 {
		return verdict{Admit: true}
	}

	// RULE: a STALE report against a KNOWN limit is a different case, and must
	// not be treated as the unknown above. The numbers exist, they are simply
	// old, and admitting blind on them is how a task gets oversubscribed while
	// its supervisor is wedged. Fall back to the count, which needs no fresh
	// measurement to be true.
	if !fresh {
		return byCount()
	}

	required := report.MemLimitMB * p.MinFreePct / 100
	if required < minFreeFloorMB {
		required = minFreeFloorMB
	}
	if report.MemFreeMB < required {
		return verdict{Reason: fmt.Sprintf(
			"this workspace has %d MB free and needs %d MB to take another session "+
				"(%d%% of %d MB, floored at %d MB); stop a session or use a larger instance",
			report.MemFreeMB, required, p.MinFreePct, report.MemLimitMB, minFreeFloorMB)}
	}
	return verdict{Admit: true}
}

// admitSession is the server-side half: find this workspace's latest report and
// apply its policy.
func (s *Server) admitSession(sc scope, ws Workspace) verdict {
	rep, have := s.activity.get(wsRef{sc.TenantID, ws.Name})
	return admits(policyOf(ws), rep.Headroom, have, rep.fresh(time.Now(), s.headroomInterval))
}

// policyOf reads a workspace record into the shape the decision takes.
func policyOf(ws Workspace) admissionPolicy {
	return admissionPolicy{
		Policy:      ws.AdmissionPolicy,
		MaxSessions: int(ws.MaxSessions),
		MinFreePct:  int(ws.MinFreeMemoryPct),
	}
}

// idleTimeout resolves section 2.4's per-session override against the
// workspace's default. A nil session override inherits; 0 at either level is the
// explicit opt-out, and returns false rather than a zero duration so no caller
// can accidentally read "disabled" as "expires immediately".
func idleTimeout(ws Workspace, sessionOverride *int32) (time.Duration, bool) {
	secs := int(ws.IdleTimeoutSecs)
	if sessionOverride != nil {
		secs = int(*sessionOverride)
	}
	if secs <= 0 {
		return 0, false
	}
	return time.Duration(secs) * time.Second, true
}
