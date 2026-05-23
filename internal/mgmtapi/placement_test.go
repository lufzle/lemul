package mgmtapi

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/lufzle/lemul/internal/agent"
	"github.com/lufzle/lemul/internal/registry"
	"github.com/lufzle/lemul/internal/store"
	"github.com/lufzle/lemul/internal/tunnel"
)

// Increment 4: placement on create, as a background operation.
//
// What changed is not where a task is started but WHO IS WAITING. While
// placement happened inside a request, a failure was the response body and the
// client read it. A background placement has nobody to answer, so every test
// here is ultimately about one question: when it goes wrong, is the reason
// somewhere a person can still find it?

// fakeRunner registers a runner tunnel that answers StartWorkspace with a task
// reference, and -- if dialIn is set -- registers the workspace tunnel the
// supervisor would hold `after` a delay, which is what completes a placement.
//
// All three knobs matter. A runner that dispatches without the task ever
// dialing in is the failure mode notePlacementFailure has a rule for, and the
// DELAY is what makes a placement outlast the request that started it -- an
// instant one finishes before the handler returns and proves nothing about
// whose context it is running on.
func fakeRunner(t *testing.T, s *Server, org, ref string, dialIn bool, after time.Duration) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })

	go func() {
		cs, err := yamux.Client(b, agent.YamuxConfig())
		if err != nil {
			return
		}
		for {
			stream, err := cs.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = stream.Close() }()
				env, err := tunnel.ReadMsg(stream)
				if err != nil {
					return
				}
				if env.Type != tunnel.MsgStartWorkspace {
					_ = tunnel.WriteMsg(stream, tunnel.MsgOK, nil)
					return
				}
				var start tunnel.StartWorkspace
				_ = env.Decode(&start)
				if dialIn {
					// The supervisor dialing back, which is the only thing that
					// turns a dispatch into a placement. In its own goroutine so
					// the dispatch can be answered first, exactly as a real task
					// coming up behind its RunTask response.
					go func() {
						time.Sleep(after)
						s.reg.AddWorkspace(fakeTunnel(t, start.TenantID, start.WorkspaceID))
					}()
				}
				_ = tunnel.WriteMsg(stream, tunnel.MsgOK, tunnel.Ref{Ref: ref})
			}()
		}
	}()
	sess, err := yamux.Server(a, agent.YamuxConfig())
	if err != nil {
		t.Fatalf("yamux: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	s.reg.AddRunner(registry.NewTunnel("runner-"+org, org, "", "pipe", sess))
}

func workspaceRow(t *testing.T, s *Server, sc scope, name string) Workspace {
	t.Helper()
	ws, err := s.getWorkspaceByName(context.Background(), sc, name)
	if err != nil {
		t.Fatalf("re-read workspace %s: %v", name, err)
	}
	return ws
}

// THE property increment 4 exists for: a placement that fails is recorded on
// the record, not lost.
//
// Invert it -- drop the notePlacementFailure call from ensureWorkspace -- and
// this is what the customer sees instead: a workspace that says `starting`
// forever, with the only explanation in a log inside a service they cannot
// read. Nothing else in the suite notices, because every other caller of
// ensureWorkspace still gets the error as a return value.
func TestAFailedPlacementLandsOnTheRecord(t *testing.T) {
	s, sc := testServer(t)
	ws, err := s.createWorkspace(context.Background(), sc, "w1", accessOwner)
	if err != nil {
		t.Fatalf("createWorkspace: %v", err)
	}

	s.startPlacement(context.Background(), sc, ws, "test")
	s.WaitForPlacements()

	got := workspaceRow(t, s.Server, sc, "w1")
	if derefOr(got.LastError) == "" {
		t.Fatal("a placement failed with no runner connected and left no reason on the record")
	}
	if !strings.Contains(derefOr(got.LastError), "runner") {
		t.Errorf("the recorded reason does not name what was missing: %q", derefOr(got.LastError))
	}
	// Nothing was placed, so nothing may be running. A row left at `starting`
	// would be read by the next placement as a task worth waiting for.
	if got.Status != WorkspaceStopped {
		t.Errorf("status is %q after a placement that never dispatched; want %q",
			got.Status, WorkspaceStopped)
	}
	if derefOr(got.TaskRef) != "" {
		t.Errorf("a task reference %q was recorded for a task that was never started",
			derefOr(got.TaskRef))
	}
}

// A dispatched task that never dials in is NOT the same failure, and the
// difference is the only handle we have on it.
//
// Something was started. It may be slow rather than dead. Forgetting its
// reference here would leave a running Fargate task that nothing can stop and
// nothing is accounting for, and would deny it the reconnect grace that stops
// the next attempt placing a SECOND task beside it (section 2.8).
func TestADispatchedTaskThatNeverDialsInKeepsItsReference(t *testing.T) {
	s, sc := testServer(t)
	s.opt.StartTimeout = 300 * time.Millisecond
	fakeRunner(t, s.Server, sc.TenantID, "arn:aws:ecs:task/never", false, 0)

	ws, err := s.createWorkspace(context.Background(), sc, "w1", accessOwner)
	if err != nil {
		t.Fatalf("createWorkspace: %v", err)
	}
	s.startPlacement(context.Background(), sc, ws, "test")
	s.WaitForPlacements()

	got := workspaceRow(t, s.Server, sc, "w1")
	if derefOr(got.TaskRef) != "arn:aws:ecs:task/never" {
		t.Errorf("task reference is %q; the only handle for stopping a dispatched task is gone",
			derefOr(got.TaskRef))
	}
	if got.Status != WorkspaceStarting {
		t.Errorf("status is %q for a task that was dispatched and may still be coming up; want %q",
			got.Status, WorkspaceStarting)
	}
	if derefOr(got.LastError) == "" {
		t.Error("a task that never dialed in left no reason on the record")
	}
}

// last_error describes ONE attempt, and the next attempt clears it.
//
// Without that rule the field turns into a history: a workspace that failed
// once and has been running happily ever since would go on reporting the
// failure, and a console showing it beside a healthy row is worse than showing
// nothing at all -- it is read as the reason for the CURRENT state.
func TestAPlacementClearsThePreviousFailure(t *testing.T) {
	s, sc := testServer(t)
	ws, err := s.createWorkspace(context.Background(), sc, "w1", accessOwner)
	if err != nil {
		t.Fatalf("createWorkspace: %v", err)
	}

	// First attempt: no runner.
	s.startPlacement(context.Background(), sc, ws, "test")
	s.WaitForPlacements()
	if derefOr(workspaceRow(t, s.Server, sc, "w1").LastError) == "" {
		t.Fatal("the first placement failed without recording why")
	}

	// Second attempt, with a runner that answers and a task that dials in.
	fakeRunner(t, s.Server, sc.TenantID, "arn:aws:ecs:task/live", true, 0)
	s.startPlacement(context.Background(), sc, workspaceRow(t, s.Server, sc, "w1"), "test")
	s.WaitForPlacements()

	got := workspaceRow(t, s.Server, sc, "w1")
	if derefOr(got.LastError) != "" {
		t.Errorf("a successful placement left the previous failure on the record: %q",
			derefOr(got.LastError))
	}
	if got.Status != WorkspaceActive {
		t.Errorf("status is %q after a task dialed in; want %q", got.Status, WorkspaceActive)
	}
}

// A background placement must not inherit the REQUEST's context.
//
// This is the whole difference between a background operation and a blocking
// one, and it fails silently. The 202 is written, the request context is
// cancelled a moment later, and a placement holding it is killed mid-wait --
// so the workspace never comes up, and the reason recorded is "context
// canceled", which describes our own bug rather than anything a customer can
// act on.
//
// The task here dials in AFTER the response is complete, which is the ordinary
// case rather than a contrived one: a Fargate task takes 20-60 s and no
// response waits that long. It needs a real server, because an
// httptest.NewRecorder never cancels a request context at all -- the bug is
// simply not reachable there.
func TestPlacementOutlivesTheRequestThatStartedIt(t *testing.T) {
	s, h, iss := authedServerFull(t)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	tok, org := signUp(t, h, iss, "alice")
	sc := scopeForOrg(t, s, org)
	fakeRunner(t, s, sc.TenantID, "arn:aws:ecs:task/slow", true, 300*time.Millisecond)

	body := strings.NewReader(`{"name":"w1"}`)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		srv.URL+"/v1/orgs/"+org+"/workspaces", body)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create workspace: %s: %s", resp.Status, raw)
	}

	// The response is complete, so the request context is cancelled. The task
	// has not dialed in yet; everything from here on runs on the server's own
	// lifetime or not at all.
	s.WaitForPlacements()

	got := workspaceRow(t, s, sc, "w1")
	if e := derefOr(got.LastError); e != "" {
		t.Fatalf("the placement failed after its request ended: %q "+
			"(it is running on the request's context and dying with it)", e)
	}
	if got.Status != WorkspaceActive {
		t.Errorf("status is %q after a task dialed in behind the response; want %q",
			got.Status, WorkspaceActive)
	}
}

// Create answers 202 with a record that already says `starting`.
//
// 201 would be a lie in the direction that costs: a client reading "created" as
// "ready" posts a session immediately and blocks for the cold start it was just
// told about. The status has to be written BEFORE the response, not inside the
// goroutine, or the row a console polls a moment later still says `stopped`.
func TestCreateAnswers202WithAStartingRecord(t *testing.T) {
	h, iss := authedServer(t)
	tok, org := signUp(t, h, iss, "alice")

	code, b := do(t, h, http.MethodPost, "/v1/orgs/"+org+"/workspaces", tok, `{"name":"w1"}`)
	if code != http.StatusAccepted {
		t.Fatalf("create got %d, want 202: %s", code, b)
	}
	var doc workspaceDoc
	if err := json.Unmarshal([]byte(b), &doc); err != nil {
		t.Fatalf("workspace: %v (%s)", err, b)
	}
	if doc.Status != WorkspaceStarting {
		t.Errorf("create reported status %q; want %q", doc.Status, WorkspaceStarting)
	}
}

// scopeForOrg resolves a slug to the scope the state.go methods take, for a
// test that drove the HTTP surface and now wants to read the row underneath it.
func scopeForOrg(t *testing.T, s *Server, slug string) scope {
	t.Helper()
	org, err := s.dir.OrgBySlug(context.Background(), slug)
	if err != nil {
		t.Fatalf("resolve org %s: %v", slug, err)
	}
	// No acting user: this is a read on behalf of the test, and inventing a
	// user id would put an empty string into a uuid comparison -- the shape
	// that broke the idle sweeper (state.go).
	return scope{Scope: store.Scope{TenantID: org.ID}, Org: org, Role: roleOwner}
}
