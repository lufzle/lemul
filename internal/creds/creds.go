// Package creds derives the short-lived credentials the control plane hands out,
// rather than storing them.
//
// The distinction is the whole point. Both credentials used to be random values
// in a map in one process (the workspace map and the attach map that this
// package replaces), and that had two consequences neither of which was a
// deliberate design choice:
//
//   - A control-plane restart emptied the maps, so every live workspace task
//     presented a credential nothing recognised, got 401, and exited -- taking
//     its sessions with it. Every deploy orphaned every running workspace.
//   - A credential minted by one process could not be redeemed by another, which
//     makes splitting the relay into its own service impossible: endpoint
//     negotiation mints, and the relay redeems.
//
// Deriving them fixes both at once. A restart re-derives the same value because
// the input is (durable state, durable key); a second process derives the same
// value because it holds the same key. There is no state to lose and none to
// share.
//
// The invariant for a workspace credential is deliberately NOT an expiry:
//
//	a workspace credential is valid exactly while its generation is the
//	workspace's current generation
//
// Generation is already persisted because it feeds the ECS RunTask client-token
// (section 2.8), so it is the natural thing to bind to. Placing a replacement
// task increments it, which invalidates the previous task's credential for free
// -- the same property minting a fresh random value used to buy.
//
// This is why the generation must only advance on a REAL placement. Advancing it
// because a tunnel happened to be missing invalidates the credential held by a
// task that is alive and about to reconnect, and the ECS driver will happily
// adopt that same task (it is still running) and hand back its ARN. The task
// then cannot authenticate, ever, and exits killing its sessions. See
// controlplane.ensureWorkspace.
package creds

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// MinKeyBytes is the shortest signing key accepted.
//
// HMAC-SHA256 tolerates any key length, so nothing would fail loudly with a
// short one -- it would simply be weak, silently, forever. Refusing at
// construction is the only moment anyone is looking.
const MinKeyBytes = 32

var (
	ErrMalformed = errors.New("creds: malformed credential")
	ErrSignature = errors.New("creds: signature does not verify")
	ErrExpired   = errors.New("creds: credential has expired")
)

// Domain separation tags. They are mixed into the MAC so a credential minted for
// one purpose cannot be presented as another: without this, a value that is a
// valid workspace credential could be replayed at the attach endpoint if the
// field layouts ever lined up. Cheap now, impossible to retrofit after something
// depends on the current bytes.
const (
	domainWorkspace = "lemul-workspace-v1"
	domainAttach    = "lemul-attach-v1"
	domainRunner    = "lemul-runner-v1"
)

// Signer derives and verifies credentials.
//
// It holds the key, so it must never be logged, formatted or serialised. There
// is deliberately no String or LogValue method: the absence means a stray
// %v prints a struct with an unexported field rather than the key, and adding
// one later should feel like the mistake it would be.
type Signer struct {
	key []byte
}

// NewSigner returns a signer over key.
//
// The key has to come from configuration -- Secrets Manager, a file, an
// environment variable -- and NOT be generated at startup. A key generated at
// boot reproduces the exact bug this package exists to remove, just one level
// down: every restart derives different credentials from the same inputs, and
// every live task is orphaned again.
func NewSigner(key []byte) (*Signer, error) {
	if len(key) < MinKeyBytes {
		return nil, fmt.Errorf("creds: signing key must be at least %d bytes, got %d",
			MinKeyBytes, len(key))
	}
	k := make([]byte, len(key))
	copy(k, key)
	return &Signer{key: k}, nil
}

// Workspace is the credential a workspace task presents when it dials in.
type Workspace string

// LogValue redacts the credential wherever it is logged.
//
// Structural rather than a redact() helper at each call site. Section 2.6
// records why: the supervisor's own argv carried -token and rendered in full in
// the console the first time the process panel met a real task, because the
// exclusion list covered only the doors somebody had thought of. A type that
// cannot print itself has no doors.
func (w Workspace) LogValue() slog.Value { return slog.StringValue("<redacted>") }

// String is defined for the same reason LogValue is: fmt is the other way a
// credential reaches a log line, and %s or %v on this type must not be the
// moment it escapes. Callers that genuinely need the wire value take Secret().
func (w Workspace) String() string { return "<redacted>" }

// Secret returns the value to put on the wire. Named so that reading a call site
// makes it obvious something sensitive is leaving.
func (w Workspace) Secret() string { return string(w) }

// WorkspaceToken derives the credential for one workspace generation.
//
// Deterministic: the same organization, workspace and generation always yield
// the same value, which is what lets a restarted control plane recognise a task
// placed by its predecessor. That also means the value is a bearer credential
// for the lifetime of the generation, exactly as the stored random value it
// replaces was.
//
// The organization is in the input, not decoration. A workspace NAME is unique
// only within an organization -- `UNIQUE (tenant_id, name)` -- so deriving from
// the name alone would give two organizations' `api` workspaces the same
// credential at the same generation, and each would authenticate as the other.
func (s *Signer) WorkspaceToken(tenantID, workspaceName string, generation uint64) Workspace {
	mac := s.mac(domainWorkspace, tenantID, workspaceName, strconv.FormatUint(generation, 10))
	return Workspace(base64.RawURLEncoding.EncodeToString(mac))
}

// VerifyWorkspace reports whether tok is the credential for this workspace
// generation. Constant-time, and false for anything malformed.
func (s *Signer) VerifyWorkspace(tenantID, workspaceName string, generation uint64, tok string) bool {
	if tok == "" {
		return false
	}
	want := s.WorkspaceToken(tenantID, workspaceName, generation)
	return hmac.Equal([]byte(want), []byte(tok))
}

// Runner is the credential a runner presents when it dials in.
type Runner string

func (r Runner) LogValue() slog.Value { return slog.StringValue("<redacted>") }
func (r Runner) String() string       { return "<redacted>" }
func (r Runner) Secret() string       { return string(r) }

// RunnerToken derives the credential for one organization's runner.
//
// It replaces a single shared secret every runner presented. That secret
// authenticated the fleet but said nothing about WHICH organization a runner
// belonged to -- the runner announced that itself, in a query parameter nobody
// checked -- so any holder could register as any organization and receive its
// workspace placements, complete with the credentials to reach them. One
// tenant made that invisible; two make it the first thing to fix.
//
// Derived rather than stored, for the same reason the workspace credential is:
// a restart must not invalidate the token a running runner is already using.
// The cost is that rotation means rolling the signing key, which rotates every
// derived credential at once -- documented rather than pretended away, and the
// thing to revisit when a customer asks for per-organization rotation.
func (s *Signer) RunnerToken(tenantID string) Runner {
	mac := s.mac(domainRunner, tenantID)
	return Runner(base64.RawURLEncoding.EncodeToString(mac))
}

// VerifyRunner reports whether tok is this organization's runner credential.
func (s *Signer) VerifyRunner(tenantID, tok string) bool {
	if tok == "" {
		return false
	}
	want := s.RunnerToken(tenantID)
	return hmac.Equal([]byte(want), []byte(tok))
}

// Attach is the single-use credential endpoint negotiation mints for one client
// connection.
type Attach string

func (a Attach) LogValue() slog.Value { return slog.StringValue("<redacted>") }
func (a Attach) String() string       { return "<redacted>" }
func (a Attach) Secret() string       { return string(a) }

// AttachClaims is what an attach credential asserts.
//
// Everything the relay needs to route and authorise a connection travels inside
// the signed token, which is what lets the relay hold no database and no shared
// memory: it validates locally and opens a stream. That is the property that
// makes the relay a separate service at all.
type AttachClaims struct {
	SessionID string
	// TenantID and WorkspaceName are what the tunnel registry is keyed on. The
	// pair, not the name alone: a workspace name is unique only within an
	// organization. They are also, deliberately, enough to route WITHOUT a
	// database -- the workspace uuid this used to carry would have had to be
	// resolved against the cell first.
	TenantID      string
	WorkspaceName string
	// OrgSlug is carried for logs and error messages. It is display, not
	// authority: every decision compares TenantID.
	OrgSlug   string
	Mode      string
	ExpiresAt time.Time
	// Nonce makes each token unique so a redeemer can enforce single use by
	// remembering the ones it has seen. Without it two tokens for the same
	// session within the same second would be indistinguishable.
	Nonce string
}

// AttachTTL is how long an attach credential stays valid.
//
// Short on purpose: the credential travels in a WebSocket URL's query string,
// because a browser cannot set an Authorization header on a WebSocket handshake
// (see auth.Middleware), and query strings are logged verbatim by load
// balancers. The window between minting and dialling is a fraction of a second,
// so a minute is already generous.
const AttachTTL = 60 * time.Second

// AttachToken mints a credential for one attach.
func (s *Signer) AttachToken(sessionID, tenantID, workspaceName, orgSlug, mode string, ttl time.Duration) (Attach, error) {
	if ttl <= 0 {
		ttl = AttachTTL
	}
	var n [12]byte
	if _, err := rand.Read(n[:]); err != nil {
		return "", fmt.Errorf("creds: nonce: %w", err)
	}
	c := AttachClaims{
		SessionID:     sessionID,
		TenantID:      tenantID,
		WorkspaceName: workspaceName,
		OrgSlug:       orgSlug,
		Mode:          mode,
		ExpiresAt:     time.Now().Add(ttl).UTC(),
		Nonce:         base64.RawURLEncoding.EncodeToString(n[:]),
	}
	payload := encodeAttach(c)
	mac := s.mac(domainAttach, payload)
	return Attach(base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(mac)), nil
}

// VerifyAttach checks the signature and expiry and returns what the token
// asserts.
//
// The signature is checked BEFORE the payload is parsed. Parsing first would
// mean acting on attacker-controlled structure -- field counts, lengths, a
// timestamp -- ahead of establishing that we wrote it, which is the ordering
// that turns a parser bug into an authentication bypass.
func (s *Signer) VerifyAttach(tok string) (AttachClaims, error) {
	rawPayload, rawMAC, ok := strings.Cut(tok, ".")
	if !ok {
		return AttachClaims{}, ErrMalformed
	}
	payload, err := base64.RawURLEncoding.DecodeString(rawPayload)
	if err != nil {
		return AttachClaims{}, ErrMalformed
	}
	sig, err := base64.RawURLEncoding.DecodeString(rawMAC)
	if err != nil {
		return AttachClaims{}, ErrMalformed
	}
	if !hmac.Equal(s.mac(domainAttach, string(payload)), sig) {
		return AttachClaims{}, ErrSignature
	}

	c, err := decodeAttach(string(payload))
	if err != nil {
		return AttachClaims{}, err
	}
	if time.Now().After(c.ExpiresAt) {
		return AttachClaims{}, ErrExpired
	}
	return c, nil
}

// The payload encoding is length-prefixed rather than delimiter-joined, for the
// same reason the MAC input is (see mac). "a|b" and "a" + "|b" must not produce
// the same bytes.
// attachFields is the number of length-prefixed fields in the payload. Named so
// that adding a claim means changing one constant and getting a decode failure
// everywhere the two halves disagree, rather than an off-by-one that silently
// shifts every field by one position.
const attachFields = 7

func encodeAttach(c AttachClaims) string {
	var b strings.Builder
	for _, f := range []string{
		c.SessionID, c.TenantID, c.WorkspaceName, c.OrgSlug, c.Mode,
		strconv.FormatInt(c.ExpiresAt.Unix(), 10), c.Nonce,
	} {
		b.WriteString(strconv.Itoa(len(f)))
		b.WriteByte(':')
		b.WriteString(f)
	}
	return b.String()
}

func decodeAttach(s string) (AttachClaims, error) {
	fields := make([]string, 0, attachFields)
	for s != "" {
		lenStr, rest, ok := strings.Cut(s, ":")
		if !ok {
			return AttachClaims{}, ErrMalformed
		}
		n, err := strconv.Atoi(lenStr)
		if err != nil || n < 0 || n > len(rest) {
			return AttachClaims{}, ErrMalformed
		}
		fields = append(fields, rest[:n])
		s = rest[n:]
		if len(fields) > attachFields {
			return AttachClaims{}, ErrMalformed
		}
	}
	if len(fields) != attachFields {
		return AttachClaims{}, ErrMalformed
	}
	exp, err := strconv.ParseInt(fields[5], 10, 64)
	if err != nil {
		return AttachClaims{}, ErrMalformed
	}
	return AttachClaims{
		SessionID:     fields[0],
		TenantID:      fields[1],
		WorkspaceName: fields[2],
		OrgSlug:       fields[3],
		Mode:          fields[4],
		ExpiresAt:     time.Unix(exp, 0).UTC(),
		Nonce:         fields[6],
	}, nil
}

// mac computes HMAC-SHA256 over a canonical encoding of domain and fields.
//
// Each field is length-prefixed. Joining with a separator instead would make the
// encoding ambiguous whenever a field can contain that separator: workspace
// (id="a|1", generation=2) and (id="a", generation="1|2") would MAC identically,
// so a credential for one workspace would verify for another. Workspace ids are
// user-supplied names today, so this is reachable rather than theoretical.
func (s *Signer) mac(domain string, fields ...string) []byte {
	h := hmac.New(sha256.New, s.key)
	h.Write([]byte(domain))
	h.Write([]byte{0})
	var n [8]byte
	for _, f := range fields {
		binary.BigEndian.PutUint64(n[:], uint64(len(f)))
		h.Write(n[:])
		h.Write([]byte(f))
	}
	return h.Sum(nil)
}
