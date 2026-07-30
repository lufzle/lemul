// Package controlplane is the only component with a public listener.
//
// In v0.1 one process holds what section 2 splits conceptually: the
// orchestrator API, the session relay, and the tunnel registries. Splitting
// them now would buy nothing -- a single task has no tenant-to-process routing
// problem (section 2.2 stage 1), and terminal traffic is a few KB/s per session.
//
// Endpoints:
//
//	GET  /v1/tunnel/runner                 runner dials in (one per tenant)
//	GET  /v1/tunnel/workspace              supervisor dials in (one per task)
//	POST /v1/workspaces/{wid}/sessions     create a session, starting the task
//	GET  /v1/sessions/{sid}/endpoint       where should the client connect?
//	GET  /v1/sessions/{sid}/attach         the client's WebSocket
package controlplane

import (
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/lufzle/lemul-cc/internal/registry"
	"github.com/lufzle/lemul-cc/internal/store"
)

type Options struct {
	// Store holds workspaces and sessions. Required.
	Store store.Store
	// AgentToken is the shared secret runners present at the upgrade. Phase 3
	// replaces it with per-tenant, rotatable tokens.
	AgentToken string
	// TenantID is the single tenant of Phase 1.
	TenantID string
	// SessionCmd is the command a new session runs.
	SessionCmd []string
	// StartTimeout bounds how long we wait for a placed task to dial in.
	// A Fargate cold start is 20-60 s.
	StartTimeout time.Duration
	// PublicURL, when set, overrides the ws:// base URL handed to agents and
	// clients. Leave empty in development so a request to 127.0.0.1 is not
	// answered with a URL for something else.
	PublicURL string
	// Image is passed through to the driver; the local driver ignores it.
	Image string

	// BedrockPreflight turns on the model check inside each workspace task.
	// Off means the workspace is not using Bedrock at all (local development
	// against a host login), and the task reports its check as skipped.
	BedrockPreflight bool
	// Region for the workspace task's Bedrock calls.
	Region string
	// Pins as "role=modelID" pairs; empty uses the defaults.
	Pins string
}

type Server struct {
	opt        Options
	reg        *registry.Registry
	st         store.Store
	creds      *credentials
	preflights *preflightStore

	// ensureLocks serialises placement per workspace. Without it two concurrent
	// session creations would each take a new generation and place a task, and a
	// workspace with two tasks means two filesystems (section 2.8). The
	// client-token protects a retried dispatch; this protects a concurrent one.
	ensureLocks sync.Map // workspace id -> *sync.Mutex
}

func New(o Options) *Server {
	if o.StartTimeout <= 0 {
		o.StartTimeout = 90 * time.Second
	}
	if len(o.SessionCmd) == 0 {
		o.SessionCmd = []string{"claude"}
	}
	if o.TenantID == "" {
		o.TenantID = "t1"
	}
	return &Server{
		opt:        o,
		reg:        registry.New(),
		st:         o.Store,
		creds:      newCredentials(),
		preflights: newPreflightStore(),
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/tunnel/runner", s.handleRunnerTunnel)
	mux.HandleFunc("GET /v1/tunnel/workspace", s.handleWorkspaceTunnel)
	mux.HandleFunc("POST /v1/workspaces/{wid}/sessions", s.handleCreateSession)
	mux.HandleFunc("GET /v1/workspaces/{wid}/sessions", s.handleListSessions)
	mux.HandleFunc("GET /v1/workspaces/{wid}/preflight", s.handlePreflight)
	mux.HandleFunc("GET /v1/sessions/{sid}/endpoint", s.handleEndpoint)
	mux.HandleFunc("GET /v1/sessions/{sid}/attach", s.handleAttach)
	return mux
}

// SetPublicURL sets the base URL agents dial back to. It exists because a
// server bound to an ephemeral port does not know its own address until after
// it is listening.
func (s *Server) SetPublicURL(u string) { s.opt.PublicURL = u }

// RunnerCount is exported for tests and for a future health endpoint.
func (s *Server) RunnerCount(tenantID string) int { return s.reg.RunnerCount(tenantID) }

// No Origin check: Phase 1 has no browser client and no cookie auth, so there
// is no CSRF surface to protect. The web client (Phase 4) must revisit this.
var upgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true },
}
