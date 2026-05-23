// Package mgmtapi is the Management API: the half of the control plane that
// knows things.
//
// Organizations, workspaces, sessions, membership, the explorer verbs, the
// runner tunnel and each task's CONTROL tunnel. It holds the database, the
// directory and the signing key, and it is the only thing that places a task.
//
// The other half is internal/relay, which carries session bytes and holds none
// of that. NEITHER CALLS THE OTHER, which is what makes the claim in section
// 2.7 possible: as long as one service saw both a PTY stream and a directory
// listing, "we route only ciphertext; we hold no key that can decrypt your
// session" would have been false about the same connection it was made on.
//
// They share exactly one thing, the signing key -- this service mints the
// credentials that one verifies.
//
// Everything an organization owns is addressed under /v1/orgs/{org}/, where
// {org} is its slug. That is not decoration. The organization has to be named
// before any row is read, because it is what the transaction is scoped to and
// what the row-level security policies compare against -- so putting it in the
// path makes the scoping structural instead of an argument a handler could
// forget to pass. A session id belonging to another organization then simply
// does not resolve, rather than resolving and being rejected afterwards.
//
// The corollary, and it is a rule rather than a default: NO endpoint assumes an
// organization. Picking a sensible one is a client's job -- see the CLI's --org
// handling -- because a server that guesses is a server that eventually guesses
// wrong on a destructive verb.
//
// Endpoints:
//
//	GET  /v1/tunnel/runner                        runner dials in (one per org)
//	GET  /v1/tunnel/workspace                     supervisor's CONTROL tunnel
//	GET  /v1/orgs                                 organizations I belong to
//	POST /v1/orgs/{org}/workspaces                create one, 202, placed in the background
//	GET  /v1/orgs/{org}/workspaces/{wid}/events   SSE — status transitions
//	POST /v1/orgs/{org}/workspaces/{wid}/sessions create a session, starting the task
//	GET  /v1/orgs/{org}/sessions/{sid}/endpoint   where should the client connect?
//	POST /v1/orgs/{org}/sessions/{sid}/stop       Ctrl-C/Ctrl-D semantics
//	POST /v1/orgs/{org}/sessions/{sid}/resume     start the process again, resuming
//	DELETE /v1/orgs/{org}/sessions/{sid}          end it and drop the conversation
//
// Attach is NOT here. The client's WebSocket and the workspace DATA tunnel both
// belong to cmd/relay, which holds no database and sees only PTY bytes -- see
// internal/relay for why that separation is what section 2.7 needs.
package mgmtapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/lufzle/lemul/internal/auth"
	"github.com/lufzle/lemul/internal/creds"
	"github.com/lufzle/lemul/internal/directory"
	"github.com/lufzle/lemul/internal/registry"
	"github.com/lufzle/lemul/internal/store"
)

type Options struct {
	// Store is this cell's database. Required.
	Store *store.Store
	// Directory is the global tier: which cell holds which principal, and which
	// organization is their own. Required.
	Directory *directory.Directory
	// CellID names the cell Store is, as the directory knows it. Every
	// organization this control plane creates is placed in it.
	CellID string
	// SessionCmd is the command a new session runs.
	SessionCmd []string
	// StartTimeout bounds how long we wait for a placed task to dial in.
	// A Fargate cold start is 20-60 s.
	StartTimeout time.Duration
	// PublicURL, when set, overrides the ws:// base URL handed to agents and
	// clients. Leave empty in development so a request to 127.0.0.1 is not
	// answered with a URL for something else.
	PublicURL string
	// RelayURL is where the session relay listens, and is the one address this
	// service hands out that is not its own.
	//
	// Two things read it: a placed task, which dials it for the DATA tunnel,
	// and a client, which is told it by endpoint negotiation. Neither service
	// ever calls the other -- this is a string we pass on, not a dependency we
	// hold. Empty means "the same place as this process", which keeps a
	// development setup that has not split them yet working.
	RelayURL string
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

	// AuthIssuer and AuthAudience configure bearer-token validation for the
	// management API. Both are REQUIRED: there is no mode in which a request
	// goes unauthenticated, because a control plane that cannot tell two
	// customers apart has nothing left to enforce (internal/auth).
	AuthIssuer   string
	AuthAudience string
	// AuthJWKSURL overrides where signing keys are fetched from. Empty derives
	// it as issuer + "/jwks", which is where Logto publishes it -- a convention
	// rather than a rule, so an identity provider that publishes elsewhere needs
	// this rather than a fork.
	AuthJWKSURL string
	// AuthCLIClientID is the public device-flow client the CLI signs in as. It is
	// advertised at GET /v1/auth/config so the CLI never has to be told it.
	AuthCLIClientID string
	// AuthConsoleClientID is the console's OAuth client. It is here only so that
	// ID tokens minted for it are accepted at PUT /v1/identity; unlike the CLI's
	// it is never advertised, because the console is configured with its own.
	AuthConsoleClientID string

	// ReconnectGrace is how long a workspace with a placed task is given to dial
	// back before we conclude the task is gone and place a replacement.
	//
	// It exists because placing into a reconnect window is destructive, not
	// merely wasteful: the new generation rotates the credential out from under
	// the task that is on its way back (see ensureWorkspace). Zero uses a default
	// comfortably above the supervisor's 2 s retry.
	ReconnectGrace time.Duration

	// HeadroomInterval is how often a workspace task is expected to report
	// (supervisor.Options.HeadroomInterval). It is configuration here rather
	// than a constant because it is what "stale" is measured against: both
	// fail-safe rules key off it, so a deployment that changes one and not the
	// other would silently move the line at which we stop trusting a report.
	// Zero uses the same 30 s default the supervisor does.
	HeadroomInterval time.Duration

	// SweepInterval is how often section 2.4's auto-stop cascade is evaluated.
	// Zero uses sweepInterval. Configurable for the same reason HeadroomInterval
	// is -- an end-to-end test of the cascade has to drive it in seconds rather
	// than wait out production cadences.
	SweepInterval time.Duration

	// SigningKey derives the workspace and attach credentials (internal/creds).
	// Required, and it must come from configuration that OUTLIVES the process:
	// a key generated at startup makes every restart mint credentials no live
	// workspace task recognises, which is the failure deriving them was meant to
	// remove.
	SigningKey []byte
}

type Server struct {
	opt        Options
	reg        *registry.Registry
	st         *store.Store
	dir        *directory.Directory
	signer     *creds.Signer
	preflights *preflightStore
	// activity holds the latest report each workspace task pushed up its
	// control tunnel, and is what section 2.4's cascade and admission control
	// both read (activity.go, reaper.go).
	activity         *activityStore
	headroomInterval time.Duration
	sweepInterval    time.Duration
	auth             *auth.Verifier
	log              *slog.Logger

	// ensureLocks serialises placement per workspace. Without it two concurrent
	// session creations would each take a new generation and place a task, and a
	// workspace with two tasks means two filesystems (section 2.8). The
	// client-token protects a retried dispatch; this protects a concurrent one.
	ensureLocks sync.Map // workspace id -> *sync.Mutex

	// bg is the lifetime of the server's own work, as opposed to a request's.
	//
	// Held rather than passed because the work that needs it is started FROM a
	// request and must outlive it: placement on create answers 202 immediately,
	// so a placement inheriting the request's context would be cancelled by its
	// own response (placement.go). It is cancelled when Start's context is, so
	// a shutdown still reaches it.
	bg       context.Context
	bgCancel context.CancelFunc
	// placing counts background placements in flight, so a shutdown -- and a
	// test -- can wait for them rather than exiting between dispatching a task
	// and recording its reference.
	placing sync.WaitGroup
}

// DefaultCellID is the cell a single-cell deployment is. Named rather than
// empty so the directory row is legible to whoever reads it while debugging
// routing, which is the one thing that table exists for.
const DefaultCellID = "cell-1"

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
	if o.CellID == "" {
		o.CellID = DefaultCellID
	}
	if o.Directory == nil {
		return nil, errors.New("controlplane: a directory is required: resolving a caller " +
			"means asking which cell holds them before anything tenant-scoped can be queried")
	}
	v, err := auth.New(auth.Options{
		Issuer:   o.AuthIssuer,
		Audience: o.AuthAudience,
		JWKSURL:  o.AuthJWKSURL,
		// Both clients, because an ID token is audienced to whichever one asked
		// for it and both relay theirs to PUT /v1/identity.
		IDTokenAudiences: nonEmpty(o.AuthConsoleClientID, o.AuthCLIClientID),
	})
	if err != nil {
		return nil, err
	}
	signer, err := creds.NewSigner(o.SigningKey)
	if err != nil {
		return nil, err
	}
	interval := o.HeadroomInterval
	if interval <= 0 {
		interval = defaultHeadroomInterval
	}
	sweep := o.SweepInterval
	if sweep <= 0 {
		sweep = sweepInterval
	}
	// Established here rather than in Start, because background placement is
	// reachable from the first request and a server is perfectly usable without
	// Start ever being called -- most of this package's tests are exactly that.
	// A server that is never started simply never has its background work
	// cancelled, which is the same lifetime the process has.
	bg, cancel := context.WithCancel(context.Background())
	return &Server{
		opt:              o,
		reg:              registry.New(),
		st:               o.Store,
		dir:              o.Directory,
		signer:           signer,
		preflights:       newPreflightStore(),
		activity:         newActivityStore(),
		headroomInterval: interval,
		sweepInterval:    sweep,
		auth:             v,
		bg:               bg,
		bgCancel:         cancel,
		log:              slog.Default().With("cell", o.CellID),
	}, nil
}

// defaultHeadroomInterval mirrors supervisor.Options.HeadroomInterval. The two
// have to agree about how often a report is expected, because that is what
// staleness is measured in.
const defaultHeadroomInterval = 30 * time.Second

// Start begins the background work the server owns: section 2.4's auto-stop
// cascade. It returns immediately; the sweeper stops when ctx is cancelled.
//
// A method rather than a goroutine started in New, so a test can build a server
// without one running underneath it and driving the clock sideways.
//
// It also binds ctx to the server's own background lifetime, which is what
// stops a placement in flight from surviving a shutdown as an orphan goroutine
// holding a database connection.
func (s *Server) Start(ctx context.Context) {
	go s.runReaper(ctx)
	go func() {
		<-ctx.Done()
		s.bgCancel()
	}()
}

func nonEmpty(ss ...string) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Unauthenticated by design, and each for its own reason.
	//
	// The tunnel endpoints are how the runner and each workspace task dial in.
	// They present the agent token and the workspace credential; they are
	// machines, with no user to be signed in as.
	//
	// The auth configuration is the bootstrap a client reads before it has a
	// token, so protecting it would be circular. It carries no secret.
	//
	// The attach WebSocket is NOT here any more: it is the relay's, along with
	// the data tunnel a task holds for it (internal/relay).
	mux.HandleFunc("GET /v1/tunnel/runner", s.handleRunnerTunnel)
	mux.HandleFunc("GET /v1/tunnel/workspace", s.handleWorkspaceTunnel)
	mux.HandleFunc("GET /v1/auth/config", s.handleAuthConfig)

	// The management API. Wrapped individually rather than by path prefix so
	// that adding an endpoint without protecting it is a visible omission at
	// the call site instead of a silent gap in a matcher.
	//
	// protect authenticates AND resolves the caller to a user and a home
	// organization; the {org} segment is then checked for membership inside each
	// handler, by requireOrg. Two steps because they answer different questions:
	// who are you, and are you allowed in here.
	protect := s.protect
	mux.HandleFunc("GET /v1/status", protect(s.handleStatus))
	mux.HandleFunc("PUT /v1/identity", protect(s.handleIdentity))
	mux.HandleFunc("GET /v1/orgs", protect(s.handleListOrgs))
	mux.HandleFunc("POST /v1/invites/{code}/redeem", protect(s.handleRedeemInvite))

	mux.HandleFunc("POST /v1/orgs/{org}/invites", protect(s.handleCreateInvite))
	mux.HandleFunc("GET /v1/orgs/{org}/members", protect(s.handleListOrgMembers))
	mux.HandleFunc("GET /v1/orgs/{org}/settings", protect(s.handleGetOrgSettings))
	mux.HandleFunc("PATCH /v1/orgs/{org}/settings", protect(s.handleUpdateOrgSettings))
	mux.HandleFunc("GET /v1/orgs/{org}/workspaces", protect(s.handleListWorkspaces))
	mux.HandleFunc("POST /v1/orgs/{org}/workspaces", protect(s.handleCreateWorkspace))
	mux.HandleFunc("GET /v1/orgs/{org}/workspaces/{wid}", protect(s.handleGetWorkspace))
	mux.HandleFunc("PATCH /v1/orgs/{org}/workspaces/{wid}", protect(s.handleRenameWorkspace))
	mux.HandleFunc("DELETE /v1/orgs/{org}/workspaces/{wid}", protect(s.handleDeleteWorkspace))
	mux.HandleFunc("GET /v1/orgs/{org}/workspaces/{wid}/members", protect(s.handleListWorkspaceMembers))
	mux.HandleFunc("PUT /v1/orgs/{org}/workspaces/{wid}/members/{user}", protect(s.handlePutWorkspaceMember))
	mux.HandleFunc("DELETE /v1/orgs/{org}/workspaces/{wid}/members/{user}", protect(s.handleDeleteWorkspaceMember))
	mux.HandleFunc("POST /v1/orgs/{org}/workspaces/{wid}/sessions", protect(s.handleCreateSession))
	mux.HandleFunc("GET /v1/orgs/{org}/workspaces/{wid}/sessions", protect(s.handleListSessions))
	mux.HandleFunc("GET /v1/orgs/{org}/workspaces/{wid}/events", protect(s.handleWorkspaceEvents))
	mux.HandleFunc("GET /v1/orgs/{org}/workspaces/{wid}/preflight", protect(s.handlePreflight))
	mux.HandleFunc("GET /v1/orgs/{org}/workspaces/{wid}/fs", protect(s.handleListDir))
	mux.HandleFunc("GET /v1/orgs/{org}/workspaces/{wid}/processes", protect(s.handleListProcesses))
	mux.HandleFunc("GET /v1/orgs/{org}/workspaces/{wid}/resources", protect(s.handleResources))
	mux.HandleFunc("GET /v1/orgs/{org}/sessions", protect(s.handleListOrgSessions))
	mux.HandleFunc("GET /v1/orgs/{org}/sessions/{sid}/endpoint", protect(s.handleEndpoint))
	mux.HandleFunc("POST /v1/orgs/{org}/sessions/{sid}/stop", protect(s.handleStopSession))
	mux.HandleFunc("POST /v1/orgs/{org}/sessions/{sid}/resume", protect(s.handleResumeSession))
	mux.HandleFunc("PATCH /v1/orgs/{org}/sessions/{sid}", protect(s.handlePatchSession))
	mux.HandleFunc("DELETE /v1/orgs/{org}/sessions/{sid}", protect(s.handleDeleteSession))
	return mux
}

// SetPublicURL sets the base URL agents dial back to. It exists because a
// server bound to an ephemeral port does not know its own address until after
// it is listening.
func (s *Server) SetPublicURL(u string) { s.opt.PublicURL = u }

// SetRelayURL sets where the session relay listens, for the same reason: in a
// test both services get ephemeral ports, and neither knows the other's until
// both are up.
func (s *Server) SetRelayURL(u string) { s.opt.RelayURL = u }

// reconnectGrace is the window a placed task gets to dial back before it is
// replaced. The default is several times the supervisor's 2 s retry, so an
// ordinary blip never reaches a placement decision.
func (s *Server) reconnectGrace() time.Duration {
	if s.opt.ReconnectGrace > 0 {
		return s.opt.ReconnectGrace
	}
	return 10 * time.Second
}

// RunnerCount is exported for tests and for a future health endpoint.
func (s *Server) RunnerCount(tenantID string) int { return s.reg.RunnerCount(tenantID) }

// RunnerToken derives an organization's runner credential.
//
// Exported for the same reason `controlplane -print-runner-token` exists: the
// value is a function of the signing key, so whatever holds the key is the only
// thing that can produce it, and an operator (or a test) needs it before a
// runner can dial in.
func (s *Server) RunnerToken(tenantID string) string {
	return s.signer.RunnerToken(tenantID).Secret()
}

// OrgIDForSlug resolves a slug to the uuid the tunnel endpoints are keyed on.
//
// Exported for tests and for operators: a slug is what a person has, and a
// runner is configured with the uuid. Empty for a slug that does not exist,
// because the only caller can do nothing more useful with the distinction.
func (s *Server) OrgIDForSlug(slug string) string {
	org, err := s.dir.OrgBySlug(context.Background(), slug)
	if err != nil {
		return ""
	}
	return org.ID
}

// No Origin check: the tunnel endpoints authenticate with derived credentials
// rather than a cookie, so there is no ambient authority for a cross-site
// request to borrow. Revisit if this service ever grows cookie auth.
var upgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true },
}
