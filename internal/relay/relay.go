// Package relay is the session data path, and nothing else.
//
// It holds no database, no directory, no runner tunnel and no store. That is
// the point of it being a separate package rather than a folder: if it grows
// one, the import is a change somebody has to write down, not a drift nobody
// notices. There is a test that asserts exactly this (relay_test.go).
//
// WHY IT IS SEPARATE AT ALL. Until Phase 5 one tunnel carried PTY bytes,
// directory listings, process lists and command lines together, and one process
// served both attach and the Management API. As long as that is true, section
// 1.3's assumption A3 -- "we route only ciphertext; we hold no key that can
// decrypt your session" -- cannot become true, because it would be false about
// the same connection it was claimed on. Splitting is what lets this service
// see only opaque bytes, and end-to-end encryption is then a change to what
// those bytes are rather than a change to who can read them.
//
// What it does:
//
//	GET /v1/tunnel/data          a workspace task's DATA tunnel
//	GET /v1/sessions/{sid}/attach a client's WebSocket, joined to one PTY
//
// Everything a decision here rests on travels inside the signed attach
// credential -- session id, organization, workspace name, mode -- so serving a
// request is a local computation plus one nonce lookup.
//
// RECORDED LIMIT, because "holds nothing" is doing a lot of work above. This
// service holds the SIGNING KEY, since verifying an attach credential and a
// workspace credential both need it, and internal/creds is HMAC throughout. So
// a compromised relay can MINT attach credentials, not merely check them.
// Asymmetric signing would leave it holding a public key and nothing else; that
// is its own piece of work and is recorded in section 12 rather than implied
// away here.
package relay

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
	"github.com/lufzle/lemul/internal/agent"
	"github.com/lufzle/lemul/internal/creds"
	"github.com/lufzle/lemul/internal/registry"
	"github.com/lufzle/lemul/internal/tunnel"
)

type Options struct {
	// SigningKey verifies attach and workspace credentials. Shared with the
	// Management API, which mints them -- see the package comment for what that
	// costs.
	SigningKey []byte
}

type Server struct {
	opt          Options
	reg          *registry.Registry
	signer       *creds.Signer
	attachNonces *usedAttachNonces
	log          *slog.Logger
}

func New(o Options) (*Server, error) {
	signer, err := creds.NewSigner(o.SigningKey)
	if err != nil {
		return nil, err
	}
	return &Server{
		opt:          o,
		reg:          registry.New(),
		signer:       signer,
		attachNonces: newUsedAttachNonces(),
		log:          slog.Default().With("service", "relay"),
	}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// Both endpoints are unauthenticated in the bearer-token sense, and each for
	// its own reason. The data tunnel presents a derived workspace credential
	// and is a machine with no user to be. The attach WebSocket carries a
	// single-use signed credential, and a browser cannot set an Authorization
	// header on a WebSocket handshake at all -- requiring one would make the
	// console's viewer impossible rather than merely inconvenient.
	mux.HandleFunc("GET /v1/tunnel/data", s.handleDataTunnel)
	mux.HandleFunc("GET /v1/sessions/{sid}/attach", s.handleAttach)
	// A liveness endpoint, because a load balancer needs one and this service
	// has no other GET that answers without a credential.
	mux.HandleFunc("GET /v1/relay/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	return mux
}

// Close drops every tunnel this relay holds.
//
// Call it when shutting down. The peers detect a vanished relay by yamux
// keepalive otherwise, which is 15 s of sessions that cannot be attached to
// while both ends believe the tunnel is fine -- and the supervisor reconnects
// within 2 s of actually noticing, so closing is most of the difference between
// a redeploy being a blip and being an outage.
func (s *Server) Close() { s.reg.CloseAll() }

// WorkspaceCount is exported for tests and for a future health endpoint.
func (s *Server) WorkspaceCount(tenantID, name string) int {
	if _, err := s.reg.PickWorkspace(tenantID, name); err != nil {
		return 0
	}
	return 1
}

// handleDataTunnel accepts a workspace task's data connection.
//
// THE GENERATION IS ANNOUNCED, not looked up, and that is the one place this
// service differs from the Management API's equivalent. That handler reads the
// workspace record to learn the current generation; this one has no database,
// so the task states its generation and the credential is verified against what
// it stated.
//
// On its own that would accept a REPLACED task forever: it still holds a
// self-consistent (credential, generation) pair, and nothing here can know a
// newer one exists. What removes it is the other tunnel. A replaced task's
// CONTROL tunnel is refused by the Management API, which does read the record,
// and a control 401 makes the supervisor exit -- taking this tunnel with it.
//
// So the asymmetry in agent.Config.FatalOnUnauthorized is not a convenience.
// This check is exactly as good as that one, and the two have to be read
// together.
func (s *Server) handleDataTunnel(w http.ResponseWriter, r *http.Request) {
	tenant := r.URL.Query().Get("tenant")
	wid := r.URL.Query().Get("workspace")
	if tenant == "" || wid == "" {
		http.Error(w, "missing tenant or workspace", http.StatusBadRequest)
		return
	}
	gen, err := strconv.ParseUint(r.URL.Query().Get("generation"), 10, 64)
	if err != nil {
		http.Error(w, "missing or malformed generation", http.StatusBadRequest)
		return
	}
	if !s.signer.VerifyWorkspace(tenant, wid, gen, bearer(r)) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !s.checkProtocol(w, r) {
		return
	}

	sess, ok := s.upgradeToYamux(w, r)
	if !ok {
		return
	}
	t := registry.NewTunnel(wid, tenant, wid, r.RemoteAddr, sess)
	s.reg.AddWorkspace(t)
	s.log.Info("data tunnel up",
		"workspace", wid, "tenant", tenant, "generation", gen, "from", r.RemoteAddr)
	defer func() {
		s.reg.RemoveWorkspace(t)
		s.log.Info("data tunnel down", "workspace", wid, "tenant", tenant)
	}()

	// Hold the handler for the connection's lifetime. The supervisor opens one
	// stream on this tunnel and never sends on it -- there are no events on the
	// data path, because headroom, preflight and session-exit are all
	// control-plane facts.
	stream, err := t.Accept()
	if err != nil {
		<-t.Closed()
		return
	}
	defer func() { _ = stream.Close() }()
	<-t.Closed()
}

func (s *Server) upgradeToYamux(w http.ResponseWriter, r *http.Request) (*yamux.Session, bool) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return nil, false
	}
	// The relay is the yamux server, as the Management API is on its own tunnel:
	// it opens attach streams and the agent accepts them.
	sess, err := yamux.Server(tunnel.NewWSConn(ws), agent.YamuxConfig())
	if err != nil {
		s.log.Error("yamux session setup failed", "error", err)
		_ = ws.Close()
		return nil, false
	}
	return sess, true
}

// No Origin check: the attach endpoint's authority is its single-use signed
// credential rather than a cookie, so there is no ambient authority for a
// cross-site request to borrow. Revisit if this ever grows cookie auth.
var upgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true },
}

func bearer(r *http.Request) string {
	if after, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return after
	}
	return ""
}

// checkProtocol refuses an agent whose wire format this relay cannot speak.
//
// Runs AFTER the credential check, deliberately: these messages are diagnostics
// for an operator with a mismatched deployment, and an unauthenticated caller is
// owed a flat 401 rather than a reading of which versions we speak.
func (s *Server) checkProtocol(w http.ResponseWriter, r *http.Request) bool {
	raw := r.URL.Query().Get(tunnel.ProtocolParam)
	if raw == "" {
		http.Error(w, "this agent announces no protocol version; roll the workspace image forward.",
			http.StatusBadRequest)
		return false
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		http.Error(w, "malformed protocol version", http.StatusBadRequest)
		return false
	}
	switch {
	case v < tunnel.MinProtocolVersion:
		http.Error(w, "agent protocol "+raw+" is older than this relay's minimum. Roll the workspace image forward.",
			http.StatusBadRequest)
		return false
	case v > tunnel.ProtocolVersion:
		http.Error(w, "agent protocol "+raw+" is newer than this relay's. Update the relay.",
			http.StatusBadRequest)
		return false
	}
	return true
}

// usedAttachNonces enforces single use for attach credentials.
//
// The credential is signed and self-describing, so verifying it needs no shared
// state -- which is what let this service exist at all. Single use is the one
// property a signature cannot express on its own, so it is tracked here: a
// nonce is recorded when redeemed and refused every time after.
//
// Entries are swept by the credential's own expiry rather than by a size cap.
// An entry is worthless once its token has expired, so the bound is exact
// rather than a guess, and the map cannot outgrow the number of attaches
// possible inside one TTL.
//
// KNOWN LIMIT, recorded rather than hidden: this set is per-process, and the
// split makes it MORE relevant rather than less, since running two relay
// replicas is now a deployment choice rather than an impossibility. Two
// instances would each accept the same token once, so replay inside the
// 60-second TTL survives a fan-out until this moves to shared state. The attack
// it leaves open is stealing the URL and using it twice within a minute; the
// alternative is a database read on the hot attach path, which is precisely the
// cost the signed credential exists to avoid -- and the read this service has
// no database to perform.
type usedAttachNonces struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func newUsedAttachNonces() *usedAttachNonces {
	return &usedAttachNonces{seen: make(map[string]time.Time)}
}

func (u *usedAttachNonces) use(nonce string, expires time.Time) bool {
	now := time.Now()
	u.mu.Lock()
	defer u.mu.Unlock()
	for k, exp := range u.seen {
		if now.After(exp) {
			delete(u.seen, k)
		}
	}
	if _, spent := u.seen[nonce]; spent {
		return false
	}
	u.seen[nonce] = expires
	return true
}

// redeemAttach verifies a client's attach credential and consumes it.
func (s *Server) redeemAttach(tok string) (creds.AttachClaims, bool) {
	c, err := s.signer.VerifyAttach(tok)
	if err != nil {
		// Logged, not returned: an operator debugging clock skew needs to tell
		// expiry from forgery, and a prober does not.
		s.log.Warn("attach credential rejected", "error", err)
		return creds.AttachClaims{}, false
	}
	if !s.attachNonces.use(c.Nonce, c.ExpiresAt) {
		s.log.Warn("attach credential replayed", "session", c.SessionID)
		return creds.AttachClaims{}, false
	}
	return c, true
}

func queryUint16(r *http.Request, key string, def uint16) uint16 {
	v, err := strconv.ParseUint(r.URL.Query().Get(key), 10, 16)
	if err != nil || v == 0 {
		return def
	}
	return uint16(v)
}

// jsonError keeps the few error bodies this service produces machine-readable,
// since its clients are a CLI and a browser rather than a person with curl.
func jsonError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
