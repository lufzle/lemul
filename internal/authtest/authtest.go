// Package authtest is a local OpenID Connect issuer for tests.
//
// It exists because there is no unauthenticated mode to test against. There
// used to be one -- a dev principal every request was attributed to when no
// issuer was configured -- and it was justified entirely by this problem: the
// e2e suite runs the whole stack in one process, and making it depend on a
// running Logto plus Mailpit would have meant the suite could not run at all
// without those containers.
//
// That justification was never worth what it cost. The bypass lived in the
// shipping binary, reachable by omitting a flag, so a control plane could serve
// every organization to anyone who could reach it. Signing tokens locally
// solves the same problem for the same price: an RSA key, a key set served from
// an httptest server, and real RS256 tokens that internal/auth validates
// through exactly the code path a real deployment uses. Nothing is skipped, so
// nothing is left untested.
//
// Two subjects against one issuer is how a test says "two different people",
// which is closer to a real deployment than the two-control-planes-with-two-dev-
// principals arrangement it replaces.
package authtest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// The deployment facts a test signs against. Values rather than constants a
// caller must remember to line up: New returns them, and the control plane is
// configured from the same struct.
const (
	// Audience is the API resource indicator an ACCESS token is minted for.
	Audience = "https://api.lemul.test"
	// ClientID is the OAuth client an ID TOKEN is audienced to. The two differ
	// on purpose -- that difference is the whole reason PUT /v1/identity can
	// tell an access token from an identity assertion.
	ClientID = "lemul-test-client"

	keyID = "authtest-key"
)

// Issuer serves a JWKS and mints tokens signed by the matching key.
type Issuer struct {
	// URL is the issuer, which is also where the key set is served from.
	URL string
	// JWKSURL is where signing keys are published. internal/auth derives this
	// as issuer + "/jwks" by default, which this honours, but it is passed
	// explicitly so the two cannot drift.
	JWKSURL string

	key *rsa.PrivateKey
	srv *httptest.Server
}

// New starts a local issuer and stops it when the test ends.
func New(t *testing.T) *Issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("authtest: generate key: %v", err)
	}
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: key.Public(), KeyID: keyID, Algorithm: string(jose.RS256), Use: "sig",
	}}}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(set)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &Issuer{URL: srv.URL, JWKSURL: srv.URL + "/jwks", key: key, srv: srv}
}

// AccessToken mints the bearer token the management API takes. It carries a
// subject and no identity claims, which is OAuth working as designed: an access
// token minted for an API resource says who you are to the identity provider
// and nothing else about you.
func (i *Issuer) AccessToken(t *testing.T, subject string) string {
	t.Helper()
	return i.mint(t, Audience, subject, "")
}

// IDToken mints the identity assertion PUT /v1/identity takes. Audienced to the
// CLIENT rather than to the API resource, which is what makes the two
// distinguishable at all.
func (i *Issuer) IDToken(t *testing.T, subject, email string) string {
	t.Helper()
	return i.mint(t, ClientID, subject, email)
}

// Mint is the general form, for tests that need a token this package would not
// otherwise produce -- a wrong audience, a foreign subject, an expired one.
func (i *Issuer) Mint(t *testing.T, audience, subject, email string) string {
	t.Helper()
	return i.mint(t, audience, subject, email)
}

func (i *Issuer) mint(t *testing.T, audience, subject, email string) string {
	t.Helper()
	sig, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: i.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", keyID),
	)
	if err != nil {
		t.Fatalf("authtest: signer: %v", err)
	}
	b := jwt.Signed(sig).Claims(jwt.Claims{
		Issuer:   i.URL,
		Audience: jwt.Audience{audience},
		Subject:  subject,
		Expiry:   jwt.NewNumericDate(time.Now().Add(time.Hour)),
		// Backdated a minute so a clock-skew allowance is never what makes a
		// test pass.
		IssuedAt: jwt.NewNumericDate(time.Now().Add(-time.Minute)),
	})
	if email != "" {
		b = b.Claims(map[string]any{"email": email})
	}
	raw, err := b.Serialize()
	if err != nil {
		t.Fatalf("authtest: serialize: %v", err)
	}
	return raw
}
