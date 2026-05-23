// Package auth validates the bearer tokens the operator console and the CLI
// present to the management API, and carries the resulting principal.
//
// Two token kinds, for two different jobs:
//
//   - The ACCESS token authorises the request. It is minted for an API resource
//     and carries a subject and no identity claims at all -- no email, no name.
//     That is OAuth working as designed: an API is meant to know its caller as a
//     stable opaque subject.
//   - The ID token carries identity. It is audienced to a CLIENT ID rather than
//     to the API resource, which is why it needs a second verifier rather than
//     the same one. It is accepted at exactly one endpoint, as a signed
//     assertion of an email the caller could otherwise simply make up.
//
// Scope checks are still deliberately absent here. Whether a caller may act on
// an organization is a membership question answered against the database, and
// answering half of it here would put authorisation in two places.
package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

type Options struct {
	// Issuer is the OIDC issuer, e.g. http://localhost:3001/oidc. Required:
	// there is no mode in which tokens go unvalidated.
	Issuer string
	// Audience is the API resource indicator the access token must be minted
	// for. A token issued for a different resource -- the identity provider's
	// own management API, say -- must not open this one.
	Audience string
	// IDTokenAudiences are the client ids whose ID tokens are accepted. There
	// is more than one because the console and the CLI are different OAuth
	// clients, and an ID token is audienced to the client that asked for it.
	IDTokenAudiences []string
	// JWKSURL overrides where signing keys are fetched from. Empty derives it
	// as issuer + "/jwks", which is where Logto publishes it. It is a setting
	// rather than a constant because the default is a convention, not a rule,
	// and discovering it eagerly would reintroduce the boot-time dependency
	// that New() exists to avoid.
	JWKSURL string
}

type Verifier struct {
	access *oidc.IDTokenVerifier
	id     *oidc.IDTokenVerifier
	idAuds []string
	issuer string
}

// Principal is who is making a request.
//
// Issuer and Subject come from the validated token and are all authentication
// can say. UserID and HomeOrgID are resolved against the directory and the cell
// -- see controlplane's resolve step -- and are what everything downstream
// actually uses. The split matters: a token proves who you are to an identity
// provider, and says nothing about whether we have ever seen you.
type Principal struct {
	Issuer  string
	Subject string
	// UserID is app_user.id in the cell. Empty before resolution.
	UserID string
	// HomeOrgID is the organization created for this user at sign-up.
	HomeOrgID string
}

// Resolved reports whether the directory lookup has happened.
func (p Principal) Resolved() bool { return p.UserID != "" }

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
	// No issuer-less mode. There used to be one -- every request attributed to a
	// fixed dev principal -- and it existed only so the test suite could run
	// without an identity provider. That made an unauthenticated control plane
	// reachable by OMITTING a flag, which is the shape of mistake AssertNoBypassRLS
	// refuses elsewhere: a deployment that cannot enforce its central security
	// property must not start. Tests now mint real tokens against a local key set
	// instead (internal/authtest), which costs no identity provider.
	if o.Issuer == "" {
		return nil, errors.New("auth: an issuer is required. A control plane that does not " +
			"validate tokens serves every organization to anyone who can reach it")
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

	// go-oidc defaults to RS256 ONLY, and Logto signs with ES384. The mismatch
	// surfaces as `unexpected signature algorithm "ES384"` against a token that
	// is otherwise perfectly valid, which reads like a broken token rather than
	// a missing setting.
	//
	// Every algorithm listed is asymmetric, and that is the security-relevant
	// part rather than a detail. The HMAC family (HS256 and friends) is
	// deliberately absent: those verify with a SHARED secret, so accepting them
	// against a key set whose keys are public by definition would let anyone
	// mint a token by signing it with the published verification key.
	algs := []string{
		oidc.RS256, oidc.RS384, oidc.RS512,
		oidc.ES256, oidc.ES384, oidc.ES512,
		oidc.PS256, oidc.PS384, oidc.PS512,
	}

	return &Verifier{
		issuer: issuer,
		access: oidc.NewVerifier(issuer, keys, &oidc.Config{
			// go-oidc calls it ClientID because it is usually an ID token; for
			// an access token minted against an API resource this is the
			// resource indicator.
			ClientID:             o.Audience,
			SupportedSigningAlgs: algs,
		}),
		// The audience check is done by hand below rather than by go-oidc,
		// because go-oidc accepts exactly one and there are two clients. Skipping
		// it here is only safe BECAUSE idAuds is checked in VerifyIDToken --
		// leaving that out would accept an ID token minted for any application
		// on the same identity provider, including one an attacker registered.
		id: oidc.NewVerifier(issuer, keys, &oidc.Config{
			SkipClientIDCheck:    true,
			SupportedSigningAlgs: algs,
		}),
		idAuds: slices.Clone(o.IDTokenAudiences),
	}, nil
}

// Middleware resolves a principal from the request, rejecting one that carries
// no valid token.
//
// Wrap individual handlers rather than the whole mux. Three endpoints must stay
// reachable without a token and getting that wrong breaks the product rather
// than merely annoying someone:
//
//   - /v1/tunnel/* is how the runner and each workspace task dial in. They
//     authenticate with their own derived credentials, and they are machines
//     with no user to sign in as.
//   - /v1/sessions/{sid}/attach is the terminal WebSocket. It already carries a
//     single-use attach credential minted by endpoint negotiation, and a browser
//     cannot set an Authorization header on a WebSocket handshake at all, so
//     requiring one here would make the console's viewer impossible.
//   - /v1/auth/config is what a client reads BEFORE it has a token.
//
// Everything else needs a valid token, with no exception and no configuration
// that removes the requirement -- see New.
func (v *Verifier) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := bearer(r)
		if raw == "" {
			unauthorized(w, "missing bearer token")
			return
		}
		tok, err := v.access.Verify(r.Context(), raw)
		if err != nil {
			// Logged, not returned. An operator debugging a misconfigured
			// audience needs the reason; an unauthenticated caller probing the
			// difference between "expired" and "wrong audience" does not get
			// free reconnaissance.
			slog.Warn("rejected bearer token", "path", r.URL.Path, "error", err)
			unauthorized(w, "invalid token")
			return
		}
		p := Principal{Issuer: v.issuer, Subject: tok.Subject}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

// Wrap is Middleware for a bare handler function, which is what the mux uses.
func (v *Verifier) Wrap(h http.HandlerFunc) http.HandlerFunc {
	wrapped := v.Middleware(h)
	return func(w http.ResponseWriter, r *http.Request) { wrapped.ServeHTTP(w, r) }
}

// IDClaims is what an ID token is accepted for.
type IDClaims struct {
	Subject string `json:"sub"`
	Email   string `json:"email"`
}

// VerifyIDToken validates an ID token and returns its identity claims.
//
// Signature, issuer and expiry come from go-oidc; the audience is checked here
// against the configured client ids, because there is more than one client and
// go-oidc's own check takes a single value.
//
// The caller must still compare the subject against the ACCESS token's subject.
// Without that, a valid ID token for anyone at all would let a caller write
// somebody else's email onto their own record -- the token proves who the
// bearer is, not that the bearer is the one making this request.
func (v *Verifier) VerifyIDToken(ctx context.Context, raw string) (IDClaims, error) {
	if len(v.idAuds) == 0 {
		return IDClaims{}, errors.New("auth: no ID token audiences configured, so no client's " +
			"ID token can be accepted; set the console and CLI client ids")
	}
	tok, err := v.id.Verify(ctx, raw)
	if err != nil {
		return IDClaims{}, fmt.Errorf("auth: id token: %w", err)
	}
	if !slices.ContainsFunc(tok.Audience, func(a string) bool { return slices.Contains(v.idAuds, a) }) {
		return IDClaims{}, errors.New("auth: id token is not audienced to a known client")
	}
	var claims IDClaims
	if err := tok.Claims(&claims); err != nil {
		return IDClaims{}, fmt.Errorf("auth: id token claims: %w", err)
	}
	if claims.Subject == "" {
		claims.Subject = tok.Subject
	}
	return claims, nil
}

// WithPrincipal attaches a principal to a context.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// PrincipalFrom returns the authenticated principal, if any.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
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
