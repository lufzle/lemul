// Package e2e_test drives the whole Phase 1 path in one process:
//
//	test client --WS--> control plane --yamux--> supervisor --> PTY
//	                          |
//	                          +--yamux--> runner --local driver--> supervisor
//
// Only the client is a test harness. The control plane, runner and supervisor
// are the production types, and the supervisor is the real binary, placed by
// the real local driver -- so a failure here indicts the product code rather
// than the scaffolding. That is the point of building the driver interface on
// day one (sections 8 and 13).
package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/lufzle/lemul/internal/authtest"
	"github.com/lufzle/lemul/internal/driver"
	"github.com/lufzle/lemul/internal/driver/local"
	"github.com/lufzle/lemul/internal/mgmtapi"
	"github.com/lufzle/lemul/internal/proto"
	"github.com/lufzle/lemul/internal/relay"
	"github.com/lufzle/lemul/internal/runner"
	"github.com/lufzle/lemul/internal/store/storetest"
)

// supervisorBin is built once for the whole package: the local driver execs a
// real binary, not an in-process stub.
var supervisorBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "lemul-e2e")
	if err != nil {
		fmt.Fprintln(os.Stderr, "tempdir:", err)
		os.Exit(1)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	supervisorBin = filepath.Join(dir, "supervisor")
	build := exec.Command("go", "build", "-o", supervisorBin, "github.com/lufzle/lemul/cmd/supervisor")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build supervisor:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// Each stack gets its OWN subject, and therefore its own organization, in the
// one shared database.
//
// The container is shared for speed, so without this a workspace left `active`
// with a task reference by one test is still there for the next -- which then
// waits out the full reconnect grace looking for a task that belongs to a
// stack that no longer exists. Isolating by organization uses the mechanism
// under test to isolate the tests, which is a pleasing way round.
var subjectSeq atomic.Uint32

func newSubject() string { return fmt.Sprintf("e2e-%d", subjectSeq.Add(1)) }

// testSigningKey derives every credential the suite's control plane hands out.
// Fixed on purpose: a random key per run would be indistinguishable from a
// working one until something restarted the control plane mid-test.
var testSigningKey = []byte("e2e-signing-key-not-a-secret-0123456789")

type stack struct {
	t       *testing.T
	http    *httptest.Server
	baseURL string // http://...
	// org is the slug of this stack's own organization, and orgURL is
	// the root every organization-scoped request hangs off. The suite signs up
	// through the real path rather than seeding rows, so this is discovered at
	// GET /v1/orgs like any client would.
	org    string
	orgID  string
	orgURL string
	drv    driver.Driver
	cp     *mgmtapi.Server
	// relay is the session data path, running as a SECOND service exactly as it
	// does in production. The suite could have mounted both on one mux and been
	// simpler; it does not, because then the split would be the one arrangement
	// nothing routinely exercises -- and the split is what section 2.7 rests on.
	relay       *relay.Server
	relayServer *httptest.Server
	relayURL    string
	// stopRunner ends the runner this stack started. Held because a workspace
	// with NO runner is a state worth testing since increment 4: creating one
	// places its task, so "a workspace record whose task never ran" is no
	// longer reachable by simply not asking for a session.
	runnerCancel context.CancelFunc

	// iss is the identity provider this control plane validates against, and
	// token is the stack's own access token.
	//
	// The suite used to run with authentication switched off entirely, against
	// a dev principal. That mode is gone -- it made an unauthenticated control
	// plane one omitted flag away in the shipping binary -- and signing tokens
	// locally turns out to cost nothing: internal/authtest is an RSA key and an
	// httptest key set, and every request below now goes through exactly the
	// verification path a real deployment uses.
	iss     *authtest.Issuer
	subject string
	token   string
}

// hGet and hPost are http.Get and http.Post carrying the stack's token.
//
// Every organization-scoped request goes through them, so a call that forgets
// the token is a compile error rather than a 401 discovered in a test that was
// asserting something else. The deliberately unauthenticated endpoints --
// /v1/tunnel/* and /v1/sessions/{sid}/attach -- still use net/http directly,
// which is what makes those call sites stand out.
func (s *stack) hGet(url string) (*http.Response, error) {
	return s.hDo(http.MethodGet, url, "", nil)
}

func (s *stack) hPost(url, contentType string, body io.Reader) (*http.Response, error) {
	return s.hDo(http.MethodPost, url, contentType, body)
}

func (s *stack) hDo(method, url, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), method, url, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	return http.DefaultClient.Do(req)
}

// stackOptions is what a test varies about the deployment it drives.
//
// Almost every test wants the same thing -- the local driver, no gateway -- so
// newStack keeps the short form and this exists for the one that does not: the
// gateway suite needs the real sandbox image, placed by the docker driver, with
// model traffic brokered through a real LiteLLM. That path shares every line
// below with the others, which is the point of having a driver interface at all
// (section 13).
type stackOptions struct {
	SessionCmd []string
	// Driver defaults to the local one, which execs the real supervisor binary.
	Driver driver.Driver
	// Gateway* configure the supported inference path (decision #12). Empty URL
	// means no gateway, and the workspace runs against whatever the session
	// command finds for itself.
	GatewayURL string
	GatewayKey string
	Pins       string
	// Image reaches the driver; the local one ignores it.
	Image string
	// HeadroomInterval and SweepInterval drive section 2.4's cascade. Left zero
	// they take production cadences (30 s each), which no test can wait out;
	// the lifecycle suite sets them to seconds. The supervisor learns the first
	// one from the control plane at placement, so setting it here moves BOTH
	// ends -- which is the property staleness depends on.
	HeadroomInterval time.Duration
	SweepInterval    time.Duration
	// Start runs the control plane's background work, which is the auto-stop
	// cascade. Off by default so tests that are not about it do not have a
	// sweeper running underneath them.
	Start bool
}

// newStack brings up a control plane and a runner. sessionCmd is what a new
// session runs, which is how each test substitutes a probe for Claude Code.
func newStack(t *testing.T, sessionCmd ...string) *stack {
	t.Helper()
	return newStackWith(t, stackOptions{SessionCmd: sessionCmd})
}

func newStackWith(t *testing.T, opt stackOptions) *stack {
	t.Helper()

	// A real Postgres, because the control plane's state IS one now: there is
	// no in-process fallback to substitute, and a fake would skip the part most
	// worth exercising. Skips when Docker is unavailable.
	st := storetest.Open(t)
	dir := storetest.OpenDirectory(t)
	if err := dir.EnsureCell(context.Background(), mgmtapi.DefaultCellID,
		"e2e", "inline:e2e"); err != nil {
		t.Fatalf("ensure cell: %v", err)
	}

	// A real issuer, because there is no other kind. Tokens are signed locally
	// rather than by Logto, so the suite still needs no compose stack -- but
	// they are real RS256 tokens validated through internal/auth's ordinary
	// path, with nothing skipped.
	//
	// Each stack gets its own subject, so tests do not share an organization.
	//
	// The signing key is FIXED rather than random, so that a test restarting the
	// control plane derives the same credentials its predecessor did -- which is
	// the property internal/creds exists to provide and the thing worth testing.
	iss := authtest.New(t)
	startTimeout := 30 * time.Second
	if opt.GatewayURL != "" {
		// A real image has to be pulled and started, and Claude Code's own boot
		// is not instant either. The local driver's 30 s is generous for a
		// process and tight for a container.
		startTimeout = 120 * time.Second
	}
	cp, err := mgmtapi.New(mgmtapi.Options{
		Store:               st,
		Directory:           dir,
		SessionCmd:          opt.SessionCmd,
		StartTimeout:        startTimeout,
		SigningKey:          testSigningKey,
		AuthIssuer:          iss.URL,
		AuthAudience:        authtest.Audience,
		AuthJWKSURL:         iss.JWKSURL,
		AuthConsoleClientID: authtest.ClientID,
		Image:               opt.Image,
		GatewayURL:          opt.GatewayURL,
		GatewayKey:          opt.GatewayKey,
		Pins:                opt.Pins,
		HeadroomInterval:    opt.HeadroomInterval,
		SweepInterval:       opt.SweepInterval,
	})
	if err != nil {
		t.Fatalf("control plane: %v", err)
	}
	srv := httptest.NewServer(cp.Handler())
	t.Cleanup(srv.Close)
	if opt.Start {
		ctx, stop := context.WithCancel(context.Background())
		t.Cleanup(stop)
		cp.Start(ctx)
	}

	// The relay, on its own listener and sharing only the signing key -- which
	// is the whole of what the two services have in common.
	// Configured with the signing key and NOTHING else. The session command is
	// deliberately not among the relay's options any more (section 2.5): the
	// control plane above states it once at placement, and the suite passing it
	// to only one service is what keeps that honest.
	rly, err := relay.New(relay.Options{SigningKey: testSigningKey})
	if err != nil {
		t.Fatalf("relay: %v", err)
	}
	rsrv := httptest.NewServer(rly.Handler())
	t.Cleanup(rsrv.Close)
	relayBase := "ws://" + strings.TrimPrefix(rsrv.URL, "http://")
	cp.SetRelayURL(relayBase)

	// The control plane hands this URL to the runner so a placed task knows
	// where to dial back.
	wsBase := "ws://" + strings.TrimPrefix(srv.URL, "http://")
	cp.SetPublicURL(wsBase)

	var drv driver.Driver
	if opt.Driver != nil {
		drv = opt.Driver
	} else {
		l := local.New(supervisorBin)
		l.Stdout, l.Stderr = nil, nil // keep test output readable
		drv = l
	}

	subject := newSubject()
	s := &stack{
		t: t, http: srv, baseURL: srv.URL, drv: drv, cp: cp,
		relay: rly, relayServer: rsrv, relayURL: rsrv.URL,
		iss: iss, subject: subject, token: iss.AccessToken(t, subject),
	}
	s.discoverOrg()

	// The runner serves ONE organization and proves which with a credential
	// derived for it, so the suite has to know its organization before it can
	// start one -- which is why sign-up happens first.
	r := runner.New(runner.Options{
		ControlPlane: wsBase,
		TenantID:     s.orgID,
		RunnerID:     "r1",
		Token:        cp.RunnerToken(s.orgID),
		Driver:       drv,
	})
	ctx, cancel := context.WithCancel(context.Background())
	s.runnerCancel = cancel
	go func() { _ = r.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if st, ok := drv.(stopper); ok {
			st.StopAll(context.Background())
		}
	})

	s.awaitRunner()
	return s
}

// discoverOrg signs this stack's subject up and learns where it landed, exactly as
// a client does: one call to GET /v1/orgs, which is the endpoint that exists
// because no API assumes an organization.
func (s *stack) discoverOrg() {
	s.t.Helper()
	resp, err := s.hGet(s.baseURL + "/v1/orgs")
	if err != nil {
		s.t.Fatalf("list organizations: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		s.t.Fatalf("list organizations: %s: %s", resp.Status, body)
	}
	var out struct {
		Orgs []struct {
			Slug     string `json:"slug"`
			Role     string `json:"role"`
			Personal bool   `json:"personal"`
		} `json:"orgs"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		s.t.Fatalf("list organizations: %v", err)
	}
	if len(out.Orgs) != 1 {
		s.t.Fatalf("a fresh principal should own exactly one organization, got %d", len(out.Orgs))
	}
	if !out.Orgs[0].Personal || out.Orgs[0].Role != "owner" {
		s.t.Fatalf("sign-up should give a personal organization owned by the signer, got %+v",
			out.Orgs[0])
	}
	s.org = out.Orgs[0].Slug
	s.orgURL = s.baseURL + "/v1/orgs/" + s.org
	s.orgID = s.cp.OrgIDForSlug(s.org)
	if s.orgID == "" {
		s.t.Fatalf("could not resolve the uuid of organization %q", s.org)
	}
}

// awaitRunner blocks until the runner's tunnel has registered. Placement fails
// with "no runner connected" until it has, and the dial-out is asynchronous.
func (s *stack) awaitRunner() {
	s.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s.cp.RunnerCount(s.orgID) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.t.Fatal("runner tunnel never came up")
}

// newWorkspace creates one by name, and is idempotent so a test can call it
// beside newSession without tracking which ran first.
//
// It exists because Phase 4 removed implicit creation: a session in a workspace
// nobody created is a 404 now, where it used to conjure the workspace. That is
// the point of the change -- a typo in a URL manufactured a record and there
// was no verb that did it deliberately -- and it means the suite has to say
// what it wants, exactly as a client does.
func (s *stack) newWorkspace(name string) {
	s.t.Helper()
	code, body := s.postJSON("/workspaces", fmt.Sprintf(`{"name":%q}`, name))
	// 202, because creating a workspace now places its task in the background
	// (increment 4). 409 is "already there", which is success for a helper
	// called this way.
	if code != http.StatusAccepted && code != http.StatusConflict {
		s.t.Fatalf("create workspace %s: %d: %s", name, code, body)
	}
}

// newSession creates a session in the given workspace, creating the workspace
// first, and returns the session id.
func (s *stack) newSession(workspace string) string {
	s.t.Helper()
	s.newWorkspace(workspace)
	resp, err := s.hPost(s.orgURL+"/workspaces/"+workspace+"/sessions", "application/json", nil)
	if err != nil {
		s.t.Fatalf("create session: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusAccepted {
		s.t.Fatalf("create session: %s: %s", resp.Status, body)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		s.t.Fatalf("create session: %v", err)
	}
	return out.ID
}

type endpointDoc struct {
	Transport  string `json:"transport"`
	Address    string `json:"address"`
	Credential string `json:"credential"`
	Mode       string `json:"mode"`
	PeerPubkey string `json:"peer_pubkey"`
}

// endpoint negotiates where to connect, declaring the mode being asked for.
//
// Mode belongs here rather than on the attach URL: it travels inside the signed
// credential, so the relay enforces what was authorised instead of trusting the
// connecting client to describe itself (see mgmtapi.handleEndpoint).
func (s *stack) endpoint(sid, mode string) endpointDoc {
	s.t.Helper()
	u := s.orgURL + "/sessions/" + sid + "/endpoint"
	if mode != "" {
		u += "?mode=" + neturl.QueryEscape(mode)
	}
	resp, err := s.hGet(u)
	if err != nil {
		s.t.Fatalf("endpoint: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		s.t.Fatalf("endpoint: %s: %s", resp.Status, body)
	}
	var ep endpointDoc
	if err := json.Unmarshal(body, &ep); err != nil {
		s.t.Fatalf("endpoint: %v", err)
	}
	return ep
}

// conn is a test stand-in for ourcli: it speaks the same client<->relay
// contract (binary frames for PTY bytes, text frames for control).
type conn struct {
	t        *testing.T
	c        *proto.Conn
	mu       sync.Mutex
	buf      bytes.Buffer
	closed   bool
	closeErr error
}

func (s *stack) attach(sid string, rows, cols uint16, mode string) *conn {
	s.t.Helper()
	ep := s.endpoint(sid, mode)
	if ep.Transport != "relay" {
		s.t.Fatalf("unexpected transport %q", ep.Transport)
	}
	if mode != "" && ep.Mode != mode {
		s.t.Fatalf("asked for mode %q, credential authorises %q", mode, ep.Mode)
	}
	u, err := neturl.Parse(ep.Address)
	if err != nil {
		s.t.Fatalf("parse address: %v", err)
	}
	q := u.Query()
	q.Set("credential", ep.Credential)
	q.Set("rows", strconv.Itoa(int(rows)))
	q.Set("cols", strconv.Itoa(int(cols)))
	u.RawQuery = q.Encode()

	ws, resp, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		if resp != nil {
			b, _ := io.ReadAll(resp.Body)
			s.t.Fatalf("attach: %s: %s", resp.Status, strings.TrimSpace(string(b)))
		}
		s.t.Fatalf("attach: %v", err)
	}

	c := &conn{t: s.t, c: proto.NewConn(ws)}
	go c.readLoop()
	s.t.Cleanup(func() { _ = c.c.Close() })
	return c
}

func (c *conn) readLoop() {
	for {
		mt, data, err := c.c.Read()
		if err != nil {
			c.mu.Lock()
			c.closed, c.closeErr = true, err
			c.mu.Unlock()
			return
		}
		if mt == websocket.BinaryMessage {
			c.mu.Lock()
			c.buf.Write(data)
			c.mu.Unlock()
		}
	}
}

func (c *conn) writeBytes(p []byte) {
	c.t.Helper()
	if err := c.c.WriteBytes(p); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

func (c *conn) resize(rows, cols uint16) {
	c.t.Helper()
	if err := c.c.WriteControl(proto.Control{Type: proto.TypeResize, Rows: rows, Cols: cols}); err != nil {
		c.t.Fatalf("resize: %v", err)
	}
}

func (c *conn) snapshot() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.buf.Bytes()...)
}

func (c *conn) isClosed() (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed, c.closeErr
}

// await waits for want to appear in the accumulated output, and FAILS if it
// never does.
//
// It used to return the snapshot either way. Every caller that did not compare
// the result -- most of them, because "wait for READY" reads like a wait rather
// than an assertion -- was therefore asserting nothing at all, and a session
// running the wrong program, or no program, passed. Found by mutating the
// supervisor to ignore its configured command and watching three tests written
// to catch exactly that go green.
//
// This is the section 2.5 lesson in the harness itself: a helper that cannot
// fail proves a property of the waiting, not of the thing waited on.
func (c *conn) await(want string, d time.Duration) string {
	c.t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if got := c.snapshot(); bytes.Contains(got, []byte(want)) {
			return string(got)
		}
		time.Sleep(10 * time.Millisecond)
	}
	got := c.snapshot()
	c.t.Fatalf("waited %s for %q; the session produced %q", d, want, got)
	return string(got)
}

// awaitEither waits for whichever of two strings appears first and returns it.
//
// For the cases where BOTH outcomes are meaningful and the test's job is to say
// which happened -- "the file is there" against "the file is gone" -- rather
// than to wait for one and time out on the other. A timeout here is a third
// outcome and a failure, because it means neither branch ran.
func (c *conn) awaitEither(a, b string, d time.Duration) string {
	c.t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		got := c.snapshot()
		switch {
		case bytes.Contains(got, []byte(a)):
			return a
		case bytes.Contains(got, []byte(b)):
			return b
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.t.Fatalf("waited %s for either %q or %q; the session produced %q",
		d, a, b, c.snapshot())
	return ""
}

func (c *conn) awaitRaw(want []byte, d time.Duration) []byte {
	c.t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if got := c.snapshot(); bytes.Contains(got, want) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	return c.snapshot()
}

func (c *conn) detach() {
	_ = c.c.WriteCloseFrame(websocket.CloseGoingAway, "client detached")
	_ = c.c.Close()
}

type sessionDocT struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
	Attachers int    `json:"attachers"`
	Rows      uint16 `json:"rows"`
	Cols      uint16 `json:"cols"`
}

func (s *stack) sessionList(workspace string) []sessionDocT {
	s.t.Helper()
	resp, err := s.hGet(s.orgURL + "/workspaces/" + workspace + "/sessions")
	if err != nil {
		s.t.Fatalf("list sessions: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		s.t.Fatalf("list sessions: %s: %s", resp.Status, body)
	}
	var out struct {
		Sessions []sessionDocT `json:"sessions"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		s.t.Fatalf("list sessions: %v", err)
	}
	return out.Sessions
}

// restartRelay tears the relay down and brings a fresh one up ON THE SAME
// ADDRESS, which is what a redeploy looks like.
//
// The address has to be the same, and finding out why was the point: a task is
// told where the relay is when it is PLACED, in its argv, so a relay that moves
// is a relay the task can never find again. Production puts it behind a stable
// address for exactly that reason. A test that moved it would be testing a
// deployment mistake rather than a redeploy.
//
// The new instance shares only the signing key, which is all the two services
// have in common -- it has no memory of the old one's tunnels or nonces.
func (s *stack) restartRelay() {
	s.t.Helper()
	addr := strings.TrimPrefix(s.relayURL, "http://")
	// Drop the established connections, not merely the listener.
	//
	// Closing an httptest server closes its listener and leaves hijacked
	// WebSockets alive, so the task stays happily connected to a "stopped"
	// relay and never reconnects -- which is not what a process exit looks
	// like, and cost a confusing half hour to notice.
	// Close the TUNNELS, then the server.
	//
	// Neither httptest.Close nor CloseClientConnections touches a hijacked
	// WebSocket, so the old handler goroutine keeps running and keeps answering
	// yamux keepalives -- the task stays connected to a relay that has stopped
	// routing, and looks healthy from both ends. That is exactly the state
	// Server.Close exists to avoid, and it is what a process exit does for free.
	s.relay.Close()
	s.relayServer.Close()

	rly, err := relay.New(relay.Options{SigningKey: testSigningKey})
	if err != nil {
		s.t.Fatalf("relay: %v", err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		s.t.Fatalf("rebinding the relay to %s: %v", addr, err)
	}
	srv := &httptest.Server{Listener: ln, Config: &http.Server{
		Handler: rly.Handler(), ReadHeaderTimeout: 10 * time.Second,
	}}
	srv.Start()
	s.t.Cleanup(srv.Close)
	s.relay, s.relayServer = rly, srv

	// Wait for the task to find it. The supervisor retries every 2 s, and
	// attaching before it has reconnected would fail for a reason the test is
	// not about.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if rly.WorkspaceCount(s.orgID, "w1") > 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	s.t.Fatal("the workspace never dialled the replacement relay")
}

// stopper is StopAll, which both real drivers offer and the driver INTERFACE
// deliberately does not: stopping everything is a test affordance for
// simulating a crashed task, not something the runner has any business doing.
//
// Asserted rather than assumed for the docker driver in particular. Its
// containers outlive the process that placed them, so a test that leaks one
// leaves a NAME behind -- and the container runtime enforces one task per
// workspace generation by refusing duplicate names, so the next placement of
// the same workspace silently never dials in. That is exactly how this was
// found.
type stopper interface{ StopAll(ctx context.Context) }

// stopAll drops every task this stack placed.
func (s *stack) stopAll(ctx context.Context) {
	if st, ok := s.drv.(stopper); ok {
		st.StopAll(ctx)
	}
}

// supervisorCount reports how many workspace tasks this stack's driver placed.
// Local driver only, for the same reason as stopAll.
func (s *stack) supervisorCount() int {
	if l, ok := s.drv.(*local.Driver); ok {
		return l.Count()
	}
	return 0
}

// awaitSupervisors waits for the driver to hold exactly n tasks.
//
// Needed because stopping one is asynchronous on both sides -- the signal, and
// then the control plane noticing the tunnel go -- so a test that asserted
// straight afterwards would be asserting on whichever it observed first.
func (s *stack) awaitSupervisors(n int, d time.Duration) {
	s.t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if s.supervisorCount() == n {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.t.Fatalf("driver holds %d tasks after %v; want %d", s.supervisorCount(), d, n)
}

// stopRunner ends this stack's runner and waits for the control plane to notice.
//
// It exists for the one state increment 4 made hard to reach: a workspace
// record whose task NEVER ran. Creating a workspace now places it, so the only
// way there is a placement that could not happen -- which is also a state a
// customer genuinely reaches, and the reason last_error exists.
func (s *stack) stopRunner() {
	s.t.Helper()
	s.runnerCancel()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s.cp.RunnerCount(s.orgID) == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.t.Fatal("the runner tunnel never went down")
}

// awaitWorkspaceStatus waits for a workspace record to settle on a status and
// returns it, so a test can then assert on the rest of the document.
//
// Placement is a background operation now (increment 4), so "create returned"
// and "the machine is up" are different moments and a test has to say which one
// it means.
func (s *stack) awaitWorkspaceStatus(name, want string, d time.Duration) workspaceDocT {
	s.t.Helper()
	deadline := time.Now().Add(d)
	var got workspaceDocT
	for time.Now().Before(deadline) {
		got = s.workspace(name)
		if got.Status == want {
			return got
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.t.Fatalf("workspace %s is %q after %v; want %q (last_error=%q)",
		name, got.Status, d, want, got.LastError)
	return got
}

// post drives a session lifecycle verb and returns the status and body, so a
// test can assert on the code rather than only on the happy path.
func (s *stack) post(path string) (int, string) {
	s.t.Helper()
	resp, err := s.hPost(s.orgURL+path, "application/json", nil)
	if err != nil {
		s.t.Fatalf("post %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(body))
}

func (s *stack) del(path string) (int, string) {
	s.t.Helper()
	resp, err := s.hDo(http.MethodDelete, s.orgURL+path, "", nil)
	if err != nil {
		s.t.Fatalf("delete %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(body))
}

// sessionStatus reports one session's process state, or "" if the record is gone.
func (s *stack) sessionStatus(workspace, sid string) string {
	s.t.Helper()
	for _, d := range s.sessionList(workspace) {
		if d.ID == sid {
			return d.Status
		}
	}
	return ""
}

// awaitStatus polls until a session reaches want. Stopping is asynchronous --
// the signal goes down the tunnel and the child exits in its own time -- so
// asserting immediately after the call would be a flake.
func (s *stack) awaitStatus(workspace, sid, want string, d time.Duration) string {
	s.t.Helper()
	deadline := time.Now().Add(d)
	for {
		got := s.sessionStatus(workspace, sid)
		if got == want || time.Now().After(deadline) {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// fakeClaude writes a script named `claude` that reports the argv it was given
// and then blocks on stdin the way a TUI does.
//
// Named `claude` deliberately: the supervisor only rewrites argv when the command
// basename says it is Claude Code, so a probe called anything else would test the
// wrong branch.
func fakeClaude(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	script := "#!/bin/sh\necho \"ARGV:$*:\"\ncat\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// transcriptPresent reports whether a conversation is on the volume.
func transcriptPresent(configDir, sessionID string) bool {
	m, err := filepath.Glob(filepath.Join(configDir, "projects", "*", sessionID+".jsonl"))
	return err == nil && len(m) > 0
}

// writeTranscript fakes a conversation already on the workspace volume.
func writeTranscript(t *testing.T, configDir, sessionID string) {
	t.Helper()
	dir := filepath.Join(configDir, "projects", "-workspace-project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionID+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

type preflightResp struct {
	WorkspaceID string `json:"workspace_id"`
	Available   bool   `json:"available"`
	Report      *struct {
		Region   string `json:"region"`
		Skipped  bool   `json:"skipped"`
		Blocking bool   `json:"blocking"`
		Models   []struct {
			Role      string `json:"role"`
			ModelID   string `json:"model_id"`
			Required  bool   `json:"required"`
			Invocable bool   `json:"invocable"`
			Advice    string `json:"advice"`
		} `json:"models"`
	} `json:"report"`
}

func (s *stack) preflight(workspace string) preflightResp {
	s.t.Helper()
	resp, err := s.hGet(s.orgURL + "/workspaces/" + workspace + "/preflight")
	if err != nil {
		s.t.Fatalf("preflight: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		s.t.Fatalf("preflight: %s: %s", resp.Status, body)
	}
	var out preflightResp
	if err := json.Unmarshal(body, &out); err != nil {
		s.t.Fatalf("preflight: %v", err)
	}
	return out
}

// postJSON is post with a body, for the handful of endpoints that take one.
func (s *stack) postJSON(path, body string) (int, string) {
	s.t.Helper()
	resp, err := s.hPost(s.orgURL+path, "application/json", strings.NewReader(body))
	if err != nil {
		s.t.Fatalf("post %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(out))
}
