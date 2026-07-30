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
