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
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/lufzle/lemul-cc/internal/controlplane"
	"github.com/lufzle/lemul-cc/internal/driver/local"
	"github.com/lufzle/lemul-cc/internal/proto"
	"github.com/lufzle/lemul-cc/internal/runner"
	"github.com/lufzle/lemul-cc/internal/store"
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
	build := exec.Command("go", "build", "-o", supervisorBin, "github.com/lufzle/lemul-cc/cmd/supervisor")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build supervisor:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

const testToken = "test-agent-token"

type stack struct {
	t       *testing.T
	http    *httptest.Server
	baseURL string // http://...
	drv     *local.Driver
	cp      *controlplane.Server
}

// newStack brings up a control plane and a runner. sessionCmd is what a new
// session runs, which is how each test substitutes a probe for Claude Code.
func newStack(t *testing.T, sessionCmd ...string) *stack {
	t.Helper()

	st, err := store.NewMemory(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}

	cp := controlplane.New(controlplane.Options{
		Store:        st,
		AgentToken:   testToken,
		TenantID:     "t1",
		SessionCmd:   sessionCmd,
		StartTimeout: 30 * time.Second,
	})
	srv := httptest.NewServer(cp.Handler())
	t.Cleanup(srv.Close)

	// The control plane hands this URL to the runner so a placed task knows
	// where to dial back.
	wsBase := "ws://" + strings.TrimPrefix(srv.URL, "http://")
	cp.SetPublicURL(wsBase)

	drv := local.New(supervisorBin)
	drv.Stdout, drv.Stderr = nil, nil // keep test output readable

	r := runner.New(runner.Options{
		ControlPlane: wsBase,
		TenantID:     "t1",
		RunnerID:     "r1",
		Token:        testToken,
		Driver:       drv,
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = r.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		drv.StopAll(context.Background())
	})

	s := &stack{t: t, http: srv, baseURL: srv.URL, drv: drv, cp: cp}
	s.awaitRunner()
	return s
}

// awaitRunner blocks until the runner's tunnel has registered. Placement fails
// with "no runner connected" until it has, and the dial-out is asynchronous.
func (s *stack) awaitRunner() {
	s.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s.cp.RunnerCount("t1") > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	s.t.Fatal("runner tunnel never came up")
}

// newSession creates a session in the given workspace and returns its id.
func (s *stack) newSession(workspace string) string {
	s.t.Helper()
	resp, err := http.Post(s.baseURL+"/v1/workspaces/"+workspace+"/sessions", "application/json", nil)
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
	PeerPubkey string `json:"peer_pubkey"`
}

func (s *stack) endpoint(sid string) endpointDoc {
	s.t.Helper()
	resp, err := http.Get(s.baseURL + "/v1/sessions/" + sid + "/endpoint")
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
	ep := s.endpoint(sid)
	if ep.Transport != "relay" {
		s.t.Fatalf("unexpected transport %q", ep.Transport)
	}
	u, err := neturl.Parse(ep.Address)
	if err != nil {
		s.t.Fatalf("parse address: %v", err)
	}
	q := u.Query()
	q.Set("credential", ep.Credential)
	q.Set("rows", strconv.Itoa(int(rows)))
	q.Set("cols", strconv.Itoa(int(cols)))
	if mode != "" {
		q.Set("mode", mode)
	}
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

// await waits for want to appear in the accumulated output.
func (c *conn) await(want string, d time.Duration) string {
	c.t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if got := c.snapshot(); bytes.Contains(got, []byte(want)) {
			return string(got)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return string(c.snapshot())
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
	resp, err := http.Get(s.baseURL + "/v1/workspaces/" + workspace + "/sessions")
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

// supervisorCount reports how many workspace tasks this stack's driver placed.
func (s *stack) supervisorCount() int {
	return s.drv.Count()
}
