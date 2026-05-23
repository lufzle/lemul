package mgmtapi

import (
	"strings"
	"testing"

	"github.com/lufzle/lemul/internal/tunnel"
)

// Admission is a pure function over a report, which is what makes the rules
// below testable exhaustively and without a container. Every one of them is a
// rule about what to do with something we are not sure of.

func pct(maxSessions, minFree int) admissionPolicy {
	return admissionPolicy{Policy: admitMinFreePct, MaxSessions: maxSessions, MinFreePct: minFree}
}

// THE RULE, and the one worth a test of its own: unknown headroom ADMITS.
//
// memoryHeadroomMB() reports a zero limit on darwin and on an unlimited cgroup.
// Inverting this makes every local development setup refuse every session --
// a policy failing closed against its own operators rather than against real
// saturation. Nothing else in the suite would catch that.
func TestUnknownHeadroomAdmits(t *testing.T) {
	// A zero limit with a zero free figure is exactly what darwin produces.
	v := admits(pct(4, 20), tunnel.Headroom{MemLimitMB: 0, MemFreeMB: 0, Sessions: 99},
		true, true)
	if !v.Admit {
		t.Fatalf("a session was refused on an unknown memory limit: %s", v.Reason)
	}
}

// The OTHER half of the same rule, and the reason it needs stating separately: a
// STALE report against a KNOWN limit is not the unknown above. The numbers exist
// and are merely old, so admitting blind on them is how a task gets
// oversubscribed while its supervisor is wedged. It falls back to the count,
// which needs no fresh measurement to be true.
func TestAStaleReportFallsBackToTheSessionCount(t *testing.T) {
	// Plenty of memory free according to a report we no longer trust, and more
	// sessions than max_sessions allows. The count must win.
	stale := tunnel.Headroom{MemLimitMB: 8192, MemFreeMB: 8000, Sessions: 4}
	if v := admits(pct(4, 20), stale, true, false); v.Admit {
		t.Error("a stale report was trusted for its memory figures")
	}
	// And with room by the count, a stale report still admits -- falling back is
	// not the same as refusing.
	stale.Sessions = 1
	if v := admits(pct(4, 20), stale, true, false); !v.Admit {
		t.Errorf("the count fallback refused a workspace with room: %s", v.Reason)
	}
}

func TestAdmissionPolicies(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy admissionPolicy
		rep    tunnel.Headroom
		have   bool
		fresh  bool
		admit  bool
	}{
		{
			name:   "no report means a cold workspace, which has room by definition",
			policy: pct(4, 20),
			have:   false,
			admit:  true,
		},
		{
			name:   "unlimited admits whatever the numbers say",
			policy: admissionPolicy{Policy: admitUnlimited, MaxSessions: 1},
			rep:    tunnel.Headroom{MemLimitMB: 8192, MemFreeMB: 1, Sessions: 99},
			have:   true, fresh: true,
			admit: true,
		},
		{
			name:   "max_sessions refuses at the limit",
			policy: admissionPolicy{Policy: admitMaxSessions, MaxSessions: 2},
			rep:    tunnel.Headroom{Sessions: 2},
			have:   true, fresh: true,
			admit: false,
		},
		{
			name:   "max_sessions admits below it",
			policy: admissionPolicy{Policy: admitMaxSessions, MaxSessions: 2},
			rep:    tunnel.Headroom{Sessions: 1},
			have:   true, fresh: true,
			admit: true,
		},
		{
			name:   "a large task with the percentage satisfied",
			policy: pct(9, 20),
			rep:    tunnel.Headroom{MemLimitMB: 16384, MemFreeMB: 4000, Sessions: 2},
			have:   true, fresh: true,
			admit: true,
		},
		{
			name:   "a large task below the percentage",
			policy: pct(9, 20),
			rep:    tunnel.Headroom{MemLimitMB: 16384, MemFreeMB: 3000, Sessions: 2},
			have:   true, fresh: true,
			admit: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := admits(tc.policy, tc.rep, tc.have, tc.fresh)
			if v.Admit != tc.admit {
				t.Fatalf("admit = %v, want %v (reason: %s)", v.Admit, tc.admit, v.Reason)
			}
			if !v.Admit && v.Reason == "" {
				t.Error("a refusal with no reason; the message is what the user sees")
			}
		})
	}
}

// The floor is what makes a percentage safe on a small task, and it is the one
// place this deviates from what section 2.4 first recorded.
//
// 20% of a 2 GiB task is ~410 MB, which is less than one Claude Code session --
// a session's footprint does not scale with the task, so a percentage alone is
// wrong at exactly the small end where an OOM is easiest to reach.
func TestTheAbsoluteFloorBeatsThePercentageOnASmallTask(t *testing.T) {
	small := tunnel.Headroom{MemLimitMB: 2048, MemFreeMB: 500, Sessions: 1}
	// 20% of 2048 is 409, which 500 clears. The floor is what refuses it.
	if v := admits(pct(4, 20), small, true, true); v.Admit {
		t.Fatal("500 MB free was admitted on a 2 GiB task: the percentage was " +
			"satisfied and the absolute floor did not apply")
	}
	// Above the floor, the same task admits.
	small.MemFreeMB = minFreeFloorMB + 1
	if v := admits(pct(4, 20), small, true, true); !v.Admit {
		t.Errorf("refused above the floor: %s", v.Reason)
	}
}

// A refusal has to name the policy and the numbers, in the style the Bedrock
// preflight set. "It refused" that needs a log read is the message this exists
// to replace.
func TestARefusalNamesTheNumbers(t *testing.T) {
	v := admits(pct(4, 20), tunnel.Headroom{MemLimitMB: 8192, MemFreeMB: 100, Sessions: 1},
		true, true)
	if v.Admit {
		t.Fatal("expected a refusal")
	}
	for _, want := range []string{"100 MB free", "8192"} {
		if !strings.Contains(v.Reason, want) {
			t.Errorf("the refusal does not mention %q: %s", want, v.Reason)
		}
	}
}

func TestIdleTimeoutResolution(t *testing.T) {
	ws := Workspace{IdleTimeoutSecs: 7200}

	if d, on := idleTimeout(ws, nil); !on || d.Hours() != 2 {
		t.Errorf("a session with no override should inherit 2 h, got %v (on=%v)", d, on)
	}

	// The per-session override section 2.4 requires.
	override := int32(60)
	if d, on := idleTimeout(ws, &override); !on || d.Seconds() != 60 {
		t.Errorf("the override was not applied: %v (on=%v)", d, on)
	}

	// 0 is the explicit opt-out, and must report "off" rather than a zero
	// duration -- a zero would read as "expires immediately", which is the
	// exact opposite of what the user asked for.
	zero := int32(0)
	if _, on := idleTimeout(ws, &zero); on {
		t.Error("a session opted out of the idle stop was still eligible for it")
	}
	// Same at the workspace level.
	if _, on := idleTimeout(Workspace{IdleTimeoutSecs: 0}, nil); on {
		t.Error("a workspace with the idle stop disabled was still eligible")
	}
}
