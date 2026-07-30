package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
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

// The tests mint their own tokens against a local key set rather than talking to
// a real identity provider. That keeps them hermetic -- `go test ./...` must not
// need Logto standing next to it, which is the same reason authentication is
// opt-in in the first place.

const (
	testIssuer   = "https://idp.test/oidc"
	testAudience = "https://api.lemul.local"
)

type signer struct {
	key   *rsa.PrivateKey
	keyID string
	jwks  *httptest.Server
}

func newSigner(t *testing.T) *signer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s := &signer{key: key, keyID: "test-key"}
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key:       key.Public(),
		KeyID:     s.keyID,
		Algorithm: string(jose.RS256),
		Use:       "sig",
	}}}
	s.jwks = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(set)
	}))
	t.Cleanup(s.jwks.Close)
	return s
}

func (s *signer) token(t *testing.T, issuer, audience string, expiry time.Time) string {
	t.Helper()
	sig, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: s.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", s.keyID),
	)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jwt.Signed(sig).Claims(jwt.Claims{
		Issuer:   issuer,
		Audience: jwt.Audience{audience},
		Subject:  "user-1",
		Expiry:   jwt.NewNumericDate(expiry),
		IssuedAt: jwt.NewNumericDate(time.Now().Add(-time.Minute)),
	}).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (s *signer) verifier(t *testing.T) *Verifier {
	t.Helper()
	v, err := New(Options{Issuer: testIssuer, Audience: testAudience, JWKSURL: s.jwks.URL})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// probe returns a handler that records whether it was reached.
func probe(reached *bool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusOK)
	}
}

func do(t *testing.T, h http.Handler, token string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/v1/workspaces", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestValidTokenPasses(t *testing.T) {
	s := newSigner(t)
	var reached bool
	h := s.verifier(t).Wrap(probe(&reached))

	w := do(t, h, s.token(t, testIssuer, testAudience, time.Now().Add(time.Hour)))
	if w.Code != http.StatusOK || !reached {
		t.Fatalf("valid token: code=%d reached=%v", w.Code, reached)
	}
}

func TestMissingTokenIsRejected(t *testing.T) {
	s := newSigner(t)
	var reached bool
	h := s.verifier(t).Wrap(probe(&reached))

	w := do(t, h, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d, want 401", w.Code)
	}
	if reached {
		t.Error("handler ran without a token")
	}
	// A client that cannot tell "sign in" from "you may not" retries forever.
	if got := w.Header().Get("WWW-Authenticate"); got == "" {
		t.Error("401 carried no WWW-Authenticate header")
	}
}

// The audience check is what stops a token minted for a DIFFERENT API from
// opening this one. Logto hands out tokens for its own management API to the
// same tenant; without this check one of those would be accepted here.
func TestWrongAudienceIsRejected(t *testing.T) {
	s := newSigner(t)
	var reached bool
	h := s.verifier(t).Wrap(probe(&reached))

	w := do(t, h, s.token(t, testIssuer, "https://default.logto.app/api", time.Now().Add(time.Hour)))
	if w.Code != http.StatusUnauthorized || reached {
		t.Fatalf("token for another audience: code=%d reached=%v", w.Code, reached)
	}
}

// A token from another issuer is signed by a key we do not trust, but the issuer
// claim is checked independently so a compromised-but-valid signature from the
// wrong tenant still fails.
func TestWrongIssuerIsRejected(t *testing.T) {
	s := newSigner(t)
	var reached bool
	h := s.verifier(t).Wrap(probe(&reached))

	w := do(t, h, s.token(t, "https://someone-else/oidc", testAudience, time.Now().Add(time.Hour)))
	if w.Code != http.StatusUnauthorized || reached {
		t.Fatalf("token from another issuer: code=%d reached=%v", w.Code, reached)
	}
}

func TestExpiredTokenIsRejected(t *testing.T) {
	s := newSigner(t)
	var reached bool
	h := s.verifier(t).Wrap(probe(&reached))

	w := do(t, h, s.token(t, testIssuer, testAudience, time.Now().Add(-time.Minute)))
	if w.Code != http.StatusUnauthorized || reached {
		t.Fatalf("expired token: code=%d reached=%v", w.Code, reached)
	}
}

// A garbage bearer must be rejected as cleanly as a missing one -- no panic, no
// 500, which is what an unauthenticated caller would probe for.
func TestGarbageTokenIsRejected(t *testing.T) {
	s := newSigner(t)
	var reached bool
	h := s.verifier(t).Wrap(probe(&reached))

	for _, bad := range []string{"not-a-jwt", "a.b.c", "..", "Bearer"} {
		w := do(t, h, bad)
		if w.Code != http.StatusUnauthorized || reached {
			t.Errorf("token %q: code=%d reached=%v", bad, w.Code, reached)
		}
	}
}

// Authentication is opt-in, and "off" has to mean genuinely transparent: the
// e2e suite and a bare local run depend on it.
func TestDisabledVerifierIsAPassThrough(t *testing.T) {
	v, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if v.Enabled() {
		t.Fatal("verifier with no issuer reports enabled")
	}
	var reached bool
	if w := do(t, v.Wrap(probe(&reached)), ""); w.Code != http.StatusOK || !reached {
		t.Fatalf("disabled: code=%d reached=%v", w.Code, reached)
	}
}

// An issuer with no audience is a verifier that would accept tokens minted for
// anything. Fail at construction rather than silently downgrading.
func TestIssuerWithoutAudienceIsAnError(t *testing.T) {
	if _, err := New(Options{Issuer: testIssuer}); err == nil {
		t.Fatal("issuer without audience was accepted")
	}
}

func TestSubjectReachesTheHandler(t *testing.T) {
	s := newSigner(t)
	var got string
	h := s.verifier(t).Wrap(func(w http.ResponseWriter, r *http.Request) {
		got, _ = SubjectFrom(r.Context())
	})
	do(t, h, s.token(t, testIssuer, testAudience, time.Now().Add(time.Hour)))
	if got != "user-1" {
		t.Fatalf("subject %q, want user-1", got)
	}
}

// Logto signs with ES384, and go-oidc accepts RS256 only unless told otherwise.
// The mismatch presents as `unexpected signature algorithm` against a token that
// is otherwise entirely valid, so it reads as a broken token rather than a
// missing setting. This pins the behaviour.
func TestEllipticCurveTokenIsAccepted(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: key.Public(), KeyID: "ec", Algorithm: string(jose.ES384), Use: "sig",
	}}}
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(set)
	}))
	defer jwks.Close()

	sig, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.ES384, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "ec"),
	)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jwt.Signed(sig).Claims(jwt.Claims{
		Issuer:   testIssuer,
		Audience: jwt.Audience{testAudience},
		Subject:  "ec-user",
		Expiry:   jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}).Serialize()
	if err != nil {
		t.Fatal(err)
	}

	v, err := New(Options{Issuer: testIssuer, Audience: testAudience, JWKSURL: jwks.URL})
	if err != nil {
		t.Fatal(err)
	}
	var reached bool
	if w := do(t, v.Wrap(probe(&reached)), raw); w.Code != http.StatusOK || !reached {
		t.Fatalf("ES384 token: code=%d reached=%v", w.Code, reached)
	}
}

// The HMAC family must stay rejected. Those algorithms verify with a SHARED
// secret, so accepting one against a key set whose keys are public by
// definition would let anyone mint a valid token by signing it with the
// published verification key. Widening SupportedSigningAlgs to fix a real
// algorithm mismatch is exactly when this gets broken by accident.
func TestHmacSignedTokenIsRejected(t *testing.T) {
	s := newSigner(t)
	secret := []byte("0123456789abcdef0123456789abcdef")
	sig, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.HS256, Key: secret},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jwt.Signed(sig).Claims(jwt.Claims{
		Issuer:   testIssuer,
		Audience: jwt.Audience{testAudience},
		Subject:  "forged",
		Expiry:   jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}).Serialize()
	if err != nil {
		t.Fatal(err)
	}

	var reached bool
	if w := do(t, s.verifier(t).Wrap(probe(&reached)), raw); w.Code != http.StatusUnauthorized || reached {
		t.Fatalf("HMAC-signed token was accepted: code=%d reached=%v", w.Code, reached)
	}
}
