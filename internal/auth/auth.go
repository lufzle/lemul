// Package auth validates the bearer tokens the operator console and ourcli
// present to the management API.
//
// Scope: signature, issuer, audience and expiry. Deliberately NO scope or
// permission checks -- there is no user model yet (Phase 3, section 2.5), so
// there is nothing to check against. A token proves someone signed in to the
// tenant's identity provider, and that is the whole claim being made. In
// particular this does NOT make attach owner-scoped, which section 2.5 still
// records as outstanding: any authenticated caller can still reach any session.
//
// Authentication is opt-in. With no issuer configured the middleware is a
// pass-through, which is what keeps the e2e suite and a bare `go run` working
// without an identity provider standing next to them.
package auth

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

type Options struct {
	// Issuer is the OIDC issuer, e.g. http://localhost:3001/oidc. Empty
	// disables authentication entirely.
	Issuer string
	// Audience is the API resource indicator the token must be minted for.
	// A token issued for a different resource -- the identity provider's own
	// management API, say -- must not open this one.
	Audience string
	// JWKSURL overrides where signing keys are fetched from. Empty derives it
	// as issuer + "/jwks", which is where Logto publishes it. It is a setting
	// rather than a constant because the default is a convention, not a rule,
	// and discovering it eagerly would reintroduce the boot-time dependency
	// that New() exists to avoid.
	JWKSURL string
}

type Verifier struct {
	verifier *oidc.IDTokenVerifier
	enabled  bool
}

// Subject is the context key carrying the authenticated principal.
type ctxKey struct{}

// New builds a verifier. It performs no network I/O: the key set is fetched on
// first use and cached thereafter.
//
// That matters operationally. Discovery at construction would mean the control
// plane refuses to boot whenever the identity provider is slow or restarting,
// turning an auth outage into a workspace outage -- and running sessions do not
// need the identity provider at all. Lazy fetching keeps the blast radius on
// new requests, where it belongs.
func New(o Options) (*Verifier, error) {
	if o.Issuer == "" {
		return &Verifier{enabled: false}, nil
	}
	if o.Audience == "" {
		return nil, errors.New("auth: audience is required when an issuer is set")
	}
	issuer := strings.TrimRight(o.Issuer, "/")
	jwks := o.JWKSURL
	if jwks == "" {
		jwks = issuer + "/jwks"
	}
	keys := oidc.NewRemoteKeySet(context.Background(), jwks)
	return &Verifier{
		enabled: true,
		verifier: oidc.NewVerifier(issuer, keys, &oidc.Config{
			// The audience check. go-oidc calls it ClientID because it is
			// usually an ID token; for an access token minted against an API
			// resource this is the resource indicator.
			ClientID: o.Audience,
			// go-oidc defaults to RS256 ONLY, and Logto signs with ES384. The
			// mismatch surfaces as `unexpected signature algorithm "ES384"`
			// against a token that is otherwise perfectly valid, which reads
			// like a broken token rather than a missing setting.
			//
			// Every algorithm listed here is asymmetric, and that is the
			// security-relevant part rather than a detail. The HMAC family
			// (HS256 and friends) is deliberately absent: those verify with a
			// SHARED secret, so accepting them against a key set whose keys are
			// public by definition would let anyone mint a token by signing it
			// with the published verification key.
			SupportedSigningAlgs: []string{
				oidc.RS256, oidc.RS384, oidc.RS512,
				oidc.ES256, oidc.ES384, oidc.ES512,
				oidc.PS256, oidc.PS384, oidc.PS512,
			},
		}),
	}, nil
}

func (v *Verifier) Enabled() bool { return v != nil && v.enabled }

// Middleware rejects a request that does not carry a valid token.
//
// Wrap individual handlers rather than the whole mux. Two endpoints must stay
// reachable without a token and getting that wrong breaks the product rather
// than merely annoying someone:
//
//   - /v1/tunnel/* is how the runner and each workspace task dial in. They
//     authenticate with the agent token and the workspace credential, and they
//     are machines with no user to sign in as.
//   - /v1/sessions/{sid}/attach is the terminal WebSocket. It already carries a
//     single-use attach credential minted by endpoint negotiation, and a browser
//     cannot set an Authorization header on a WebSocket handshake at all, so
//     requiring one here would make the console's viewer impossible.
func (v *Verifier) Middleware(next http.Handler) http.Handler {
	if !v.Enabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := bearer(r)
		if raw == "" {
			unauthorized(w, "missing bearer token")
			return
		}
		tok, err := v.verifier.Verify(r.Context(), raw)
		if err != nil {
			// Logged, not returned. An operator debugging a misconfigured
			// audience needs the reason; an unauthenticated caller probing the
			// difference between "expired" and "wrong audience" does not get
			// free reconnaissance.
			log.Printf("auth: rejected token for %s: %v", r.URL.Path, err)
			unauthorized(w, "invalid token")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, tok.Subject)))
	})
}

// Wrap is Middleware for a bare handler function, which is what the mux uses.
func (v *Verifier) Wrap(h http.HandlerFunc) http.HandlerFunc {
	if !v.Enabled() {
		return h
	}
	wrapped := v.Middleware(h)
	return func(w http.ResponseWriter, r *http.Request) { wrapped.ServeHTTP(w, r) }
}

// SubjectFrom returns the authenticated principal, if any.
func SubjectFrom(ctx context.Context) (string, bool) {
	s, ok := ctx.Value(ctxKey{}).(string)
	return s, ok
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func unauthorized(w http.ResponseWriter, msg string) {
	// WWW-Authenticate is what tells a client this is an auth problem it can
	// fix by signing in, rather than a permission problem it cannot.
	w.Header().Set("WWW-Authenticate", `Bearer realm="lemul"`)
	http.Error(w, msg, http.StatusUnauthorized)
}
