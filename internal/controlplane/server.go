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
//	POST /v1/sessions/{sid}/stop           Ctrl-C/Ctrl-D semantics
//	POST /v1/sessions/{sid}/resume         start the process again, resuming
//	DELETE /v1/sessions/{sid}              end it and drop the conversation
package controlplane

import (
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/lufzle/lemul-cc/internal/auth"
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

	// GatewayURL and GatewayKey configure the supported inference path
	// (decision #12): workspace tasks broker model traffic through a loopback
	// proxy the supervisor owns, so no credential reaches a session.
	GatewayURL string
	GatewayKey string

	// OTel* configure Claude Code's telemetry, rendered into managed settings by
	// the image entrypoint so a session cannot override or silence it (§5.2).
	// Empty endpoint means no telemetry: audit signals are optional
	// infrastructure, not a dependency (§5).
	OTelEndpoint string
	OTelHeaders  string
	OTelProtocol string
	// OTelTraces opts into Claude Code's BETA trace export. Off by default:
	// higher volume than metrics or events, and beta.
	OTelTraces bool

	// BedrockPreflight turns on the model check inside each workspace task.
	// Off means the workspace is not using Bedrock at all (local development
	// against a host login), and the task reports its check as skipped.
	BedrockPreflight bool
	// Region for the workspace task's Bedrock calls.
	Region string
	// Pins as "role=modelID" pairs; empty uses the defaults.
	Pins string

	// AuthIssuer and AuthAudience turn on bearer-token validation for the
	// management API. Empty issuer leaves it off, which is what keeps the e2e
	// suite and local runs working without an identity provider (internal/auth).
	AuthIssuer   string
	AuthAudience string
}

type Server struct {
	opt        Options
	reg        *registry.Registry
	st         store.Store
	creds      *credentials
	preflights *preflightStore
	auth       *auth.Verifier

	// ensureLocks serialises placement per workspace. Without it two concurrent
	// session creations would each take a new generation and place a task, and a
	// workspace with two tasks means two filesystems (section 2.8). The
	// client-token protects a retried dispatch; this protects a concurrent one.
	ensureLocks sync.Map // workspace id -> *sync.Mutex
}

// New builds the server. It returns an error only for configuration that cannot
// work at all -- an issuer without an audience -- rather than for anything the
// identity provider might be doing, which is checked lazily per request.
func New(o Options) (*Server, error) {
	if o.StartTimeout <= 0 {
		o.StartTimeout = 90 * time.Second
	}
	if len(o.SessionCmd) == 0 {
		o.SessionCmd = []string{"claude"}
	}
	if o.TenantID == "" {
		o.TenantID = "t1"
	}
	v, err := auth.New(auth.Options{Issuer: o.AuthIssuer, Audience: o.AuthAudience})
	if err != nil {
		return nil, err
	}
	return &Server{
		opt:        o,
		reg:        registry.New(),
		st:         o.Store,
		creds:      newCredentials(),
		preflights: newPreflightStore(),
		auth:       v,
	}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Unauthenticated by design, and each for its own reason.
	//
	// The tunnel endpoints are how the runner and each workspace task dial in.
	// They present the agent token and the workspace credential; they are
	// machines, with no user to be signed in as.
	//
	// The attach WebSocket already carries a single-use credential minted by
	// endpoint negotiation -- and a browser cannot set an Authorization header
	// on a WebSocket handshake, so requiring a bearer here would make the
	// console's viewer impossible to build rather than merely inconvenient.
	mux.HandleFunc("GET /v1/tunnel/runner", s.handleRunnerTunnel)
	mux.HandleFunc("GET /v1/tunnel/workspace", s.handleWorkspaceTunnel)
	mux.HandleFunc("GET /v1/sessions/{sid}/attach", s.handleAttach)

	// The management API. Wrapped individually rather than by path prefix so
	// that adding an endpoint without protecting it is a visible omission at
	// the call site instead of a silent gap in a matcher.
	protect := s.auth.Wrap
	mux.HandleFunc("GET /v1/status", protect(s.handleStatus))
	mux.HandleFunc("GET /v1/workspaces", protect(s.handleListWorkspaces))
	mux.HandleFunc("GET /v1/workspaces/{wid}", protect(s.handleGetWorkspace))
	mux.HandleFunc("POST /v1/workspaces/{wid}/sessions", protect(s.handleCreateSession))
	mux.HandleFunc("GET /v1/workspaces/{wid}/sessions", protect(s.handleListSessions))
	mux.HandleFunc("GET /v1/workspaces/{wid}/preflight", protect(s.handlePreflight))
	mux.HandleFunc("GET /v1/sessions/{sid}/endpoint", protect(s.handleEndpoint))
	mux.HandleFunc("POST /v1/sessions/{sid}/stop", protect(s.handleStopSession))
	mux.HandleFunc("POST /v1/sessions/{sid}/resume", protect(s.handleResumeSession))
	mux.HandleFunc("DELETE /v1/sessions/{sid}", protect(s.handleDeleteSession))
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
