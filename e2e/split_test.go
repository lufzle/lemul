package e2e_test

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// These cover what the runner/supervisor split added, as opposed to what S3
// already proved about the transport.

// The Phase 1 exit criterion for decision #4: a client can detach and reattach
// into the same live session. The prototype killed the child on disconnect;
// keeping it alive IS detach (section 2.4).
func TestDetachAndReattachFindsTheSameLiveSession(t *testing.T) {
	// The child counts, so a session that survived detach can be told apart
	// from a freshly forked one.
	s := newStack(t, "sh", "-c",
		`stty raw -echo; i=0; while :; do i=$((i+1)); echo "tick-$i"; sleep 0.2; done`)
	sid := s.newSession("w1")

	first := s.attach(sid, 24, 80, "")
	if got := first.await("tick-3", 15*time.Second); !strings.Contains(got, "tick-3") {
		t.Fatalf("session never started: %q", got)
	}
	first.detach()

	// Let it run unattended: output during this window must not stall the child.
	time.Sleep(1500 * time.Millisecond)

	second := s.attach(sid, 24, 80, "")
	got := second.await("tick-", 15*time.Second)
	if strings.Contains(got, "tick-1\r\n") {
		t.Errorf("reattach restarted the child instead of finding the live session:\n%s", got)
	}
	if !strings.Contains(got, "tick-1") {
		// tick-1 appears via the replay ring; its absence means the ring did not
		// survive the detach.
		t.Errorf("replay ring did not survive the detach:\n%s", got)
	}
	// The counter must have advanced past where the first client left off, which
	// only happens if the child kept running while nobody was attached.
	if !containsAnyTick(got, 8) {
		t.Errorf("child appears to have been paused or restarted while detached:\n%s", got)
	}
}

func containsAnyTick(s string, from int) bool {
	for i := from; i < from+40; i++ {
		if strings.Contains(s, "tick-"+itoa(i)) {
			return true
		}
	}
	return false
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// A reattaching client must get the terminal modes back, or it lands on a
// correct-looking screen with broken Shift+Enter and broken paste. Claude Code
// emits them once at startup and never re-emits them, not even on SIGWINCH
// (measured in winch-probe/).
func TestReattachRestoresTerminalModes(t *testing.T) {
	s := newStack(t, "sh", "-c",
		`stty raw -echo; printf '\033[?2004h\033[>4;2m\033[>1u'; echo READY; while :; do sleep 0.2; done`)
	sid := s.newSession("w1")

	first := s.attach(sid, 24, 80, "")
	first.await("READY", 15*time.Second)
	first.detach()

	second := s.attach(sid, 24, 80, "")
	got := second.await("\x1b[>1u", 15*time.Second)
	for _, want := range []string{"\x1b[?2004h", "\x1b[>4;2m", "\x1b[>1u"} {
		if !strings.Contains(got, want) {
			t.Errorf("reattach did not restore %q\n got: %q", want, got)
		}
	}
}

// The second session in a running workspace forks a PTY in the existing task
// rather than paying a cold start -- the reason the task belongs to the
// workspace and not the session (section 2.3).
func TestSecondSessionReusesTheWorkspaceTask(t *testing.T) {
	s := newStack(t, "sh", "-c", `stty raw -echo; echo READY; exec cat`)

	start := time.Now()
	first := s.attach(s.newSession("w1"), 24, 80, "")
	first.await("READY", 20*time.Second)
	firstElapsed := time.Since(start)

	start = time.Now()
	second := s.attach(s.newSession("w1"), 30, 120, "")
	second.await("READY", 20*time.Second)
	secondElapsed := time.Since(start)

	t.Logf("first session %v, second session %v", firstElapsed, secondElapsed)
	if secondElapsed > firstElapsed {
		t.Errorf("second session (%v) was not faster than the first (%v); "+
			"it may have placed a second task", secondElapsed, firstElapsed)
	}

	// Both must be independently alive: sessions share a filesystem, not a PTY.
	first.writeBytes([]byte("<from-one>"))
	second.writeBytes([]byte("<from-two>"))
	if got := first.await("<from-one>", 10*time.Second); strings.Contains(got, "<from-two>") {
		t.Error("session 1 received session 2's input; the PTYs are not separate")
	}
	if got := second.await("<from-two>", 10*time.Second); strings.Contains(got, "<from-one>") {
		t.Error("session 2 received session 1's input; the PTYs are not separate")
	}
}

// A viewer that can write is silently a co-driver, because both attachers write
// the same PTY stdin (section 2.5). The relay drops viewer input and so does
// the supervisor; this proves the pair.
func TestViewerCannotDriveTheSession(t *testing.T) {
	s := newStack(t, "sh", "-c", `stty raw -echo; echo READY; exec cat`)
	sid := s.newSession("w1")

	ctrl := s.attach(sid, 24, 80, "control")
	ctrl.await("READY", 15*time.Second)

	viewer := s.attach(sid, 24, 80, "viewer")
	viewer.writeBytes([]byte("<from-viewer>"))
	ctrl.writeBytes([]byte("<from-control>"))

	got := ctrl.await("<from-control>", 10*time.Second)
	if strings.Contains(got, "<from-viewer>") {
		t.Errorf("viewer input reached the PTY:\n%q", got)
	}
	if !strings.Contains(got, "<from-control>") {
		t.Errorf("control input did not reach the PTY:\n%q", got)
	}
	// The viewer must still SEE the session -- it is read-only, not blind.
	if v := viewer.await("<from-control>", 10*time.Second); !strings.Contains(v, "<from-control>") {
		t.Errorf("viewer did not receive output:\n%q", v)
	}
}

// Endpoint negotiation exists from day one so E2E, direct and tailnet become a
// per-tenant config change rather than a client rewrite (section 2.7).
func TestEndpointNegotiationContract(t *testing.T) {
	s := newStack(t, "sh", "-c", `exec cat`)
	sid := s.newSession("w1")

	ep := s.endpoint(sid)
	if ep.Transport != "relay" {
		t.Errorf("transport = %q, want relay (the only v0.1 answer)", ep.Transport)
	}
	if ep.Credential == "" {
		t.Error("endpoint returned no credential")
	}
	if !strings.Contains(ep.Address, "/v1/sessions/"+sid+"/attach") {
		t.Errorf("address does not point at this session's attach endpoint: %q", ep.Address)
	}
}

// A leaked attach URL must not be replayable.
func TestAttachCredentialIsSingleUse(t *testing.T) {
	s := newStack(t, "sh", "-c", `stty raw -echo; echo READY; exec cat`)
	sid := s.newSession("w1")

	c := s.attach(sid, 24, 80, "")
	c.await("READY", 15*time.Second)

	// Re-present the credential the first attach already redeemed.
	ep := s.endpoint(sid)
	resp, err := http.Get(s.baseURL + "/v1/sessions/" + sid + "/attach?credential=" + ep.Credential)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = resp.Body.Close()
	// A plain GET cannot upgrade, so anything other than 401 means the
	// credential check itself passed -- which is what we are testing.
	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatal("a fresh credential was rejected")
	}

	resp2, err := http.Get(s.baseURL + "/v1/sessions/" + sid + "/attach?credential=" + ep.Credential)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("replayed credential returned %s, want 401", resp2.Status)
	}
}

// The workspace tunnel credential is minted per placement, so an unauthenticated
// task cannot register as a workspace it was not started for (decision #6).
func TestWorkspaceTunnelRejectsBadCredential(t *testing.T) {
	s := newStack(t, "sh", "-c", `exec cat`)
	s.newSession("w1") // force a placement so the workspace exists

	req, _ := http.NewRequest(http.MethodGet,
		s.baseURL+"/v1/tunnel/workspace?tenant=t1&workspace=w1", nil)
	req.Header.Set("Authorization", "Bearer not-the-credential")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad workspace credential returned %s, want 401", resp.Status)
	}
}

// The runner tunnel is the tenant's control channel; an unauthenticated peer
// must never get a yamux session at all.
func TestRunnerTunnelRejectsBadToken(t *testing.T) {
	s := newStack(t, "sh", "-c", `exec cat`)

	req, _ := http.NewRequest(http.MethodGet, s.baseURL+"/v1/tunnel/runner?tenant=t1", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad runner token returned %s, want 401", resp.Status)
	}
}

// Listing must report what the SUPERVISOR knows, not what the control plane
// remembers. The supervisor is the only component that knows whether a PTY still
// exists; keeping our own copy would drift into a second source of truth.
func TestListSessionsReflectsLiveState(t *testing.T) {
	s := newStack(t, "sh", "-c", `stty raw -echo; echo READY; exec cat`)

	sid1 := s.newSession("w1")
	sid2 := s.newSession("w1")

	// Nothing attached yet: both records exist, both PTYs do not.
	before := s.sessionList("w1")
	if len(before) != 2 {
		t.Fatalf("got %d session records, want 2", len(before))
	}
	for _, d := range before {
		if d.Status != "stopped" {
			t.Errorf("session %s reported %q before anything attached; the PTY is "+
				"forked on first attach, so it cannot be running yet", d.ID, d.Status)
		}
	}

	c := s.attach(sid1, 24, 80, "")
	c.await("READY", 15*time.Second)

	after := byID(s.sessionList("w1"))
	if got := after[sid1].Status; got != "running" {
		t.Errorf("attached session %s reported %q, want running", sid1, got)
	}
	if got := after[sid1].Attachers; got != 1 {
		t.Errorf("attached session %s reported %d clients, want 1", sid1, got)
	}
	if got := after[sid2].Status; got != "stopped" {
		t.Errorf("never-attached session %s reported %q, want stopped", sid2, got)
	}

	// Detaching leaves the child running, so the session stays live with zero
	// clients -- exactly the state `connect` reattaches to.
	c.detach()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		d := byID(s.sessionList("w1"))[sid1]
		if d.Status == "running" && d.Attachers == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	d := byID(s.sessionList("w1"))[sid1]
	t.Fatalf("after detach, session %s reported status=%q clients=%d; "+
		"want running with 0 clients", sid1, d.Status, d.Attachers)
}

// Listing must never place a task: it is a read, and a user asking what exists
// should not be billed for a Fargate cold start.
func TestListSessionsDoesNotStartTheWorkspace(t *testing.T) {
	s := newStack(t, "sh", "-c", `exec cat`)

	got := s.sessionList("never-used")
	if len(got) != 0 {
		t.Errorf("got %d sessions for an unknown workspace, want 0", len(got))
	}
	if n := s.supervisorCount(); n != 0 {
		t.Errorf("listing placed %d workspace task(s)", n)
	}
}

func byID(docs []sessionDocT) map[string]sessionDocT {
	m := make(map[string]sessionDocT, len(docs))
	for _, d := range docs {
		m[d.ID] = d
	}
	return m
}

// Local development does not use Bedrock at all, so the supervisor reports
// "skipped" rather than staying silent. Absence means "not yet" and skipped
// means "not applicable"; conflating them would either block every local session
// or fail open in production.
func TestPreflightSkippedWhenNotUsingBedrock(t *testing.T) {
	s := newStack(t, "sh", "-c", `stty raw -echo; echo READY; exec cat`)
	sid := s.newSession("w1") // must not be refused
	c := s.attach(sid, 24, 80, "")
	if got := c.await("READY", 15*time.Second); !strings.Contains(got, "READY") {
		t.Fatalf("session did not start: %q", got)
	}

	rep := s.preflight("w1")
	if !rep.Available {
		t.Fatal("no preflight report reached the control plane")
	}
	if !rep.Report.Skipped {
		t.Errorf("report should be marked skipped when bedrock is off: %+v", rep.Report)
	}
	if rep.Report.Blocking {
		t.Error("a skipped preflight must never block")
	}
}

// The console needs the report even for a workspace that never reported, and it
// must be able to tell "nothing yet" from "checked and fine".
func TestPreflightEndpointReportsAbsence(t *testing.T) {
	s := newStack(t, "sh", "-c", `exec cat`)
	rep := s.preflight("never-used")
	if rep.Available {
		t.Errorf("reported a verdict for a workspace that never ran: %+v", rep.Report)
	}
}
