package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lufzle/lemul-cc/internal/store"
)

func authServer(t *testing.T, issuer, audience, cliClientID string) http.Handler {
	t.Helper()
	st, err := store.NewMemory("")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	s, err := New(Options{
		Store:           st,
		AuthIssuer:      issuer,
		AuthAudience:    audience,
		AuthCLIClientID: cliClientID,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return s.Handler()
}

func getAuthConfig(t *testing.T, h http.Handler, hdr http.Header) (int, authConfigDoc) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/auth/config", nil)
	for k, v := range hdr {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var doc authConfigDoc
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("decode: %v (body %q)", err, rec.Body.String())
		}
	}
	return rec.Code, doc
}

// The load-bearing one. This endpoint is what a client reads BEFORE it has a
// token, so protecting it would be circular -- the user could never learn where
// to get the token the endpoint demands.
func TestAuthConfigIsReachableWithoutAToken(t *testing.T) {
	h := authServer(t, "https://idp.example/oidc", "https://api.lemul.local", "cli-abc")

	code, doc := getAuthConfig(t, h, nil)
	if code != http.StatusOK {
		t.Fatalf("no bearer token: got %d, want 200", code)
	}
	if !doc.Required {
		t.Error("required is false on a control plane that enforces authentication")
	}

	// And the contrast that proves authentication really is on: a protected
	// endpoint on the same server refuses the same tokenless request.
	req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/v1/status without a token: got %d, want 401", rec.Code)
	}
}

func TestAuthConfigAdvertisesWhatTheClientNeeds(t *testing.T) {
	h := authServer(t, "https://idp.example/oidc", "https://api.lemul.local", "cli-abc")

	_, doc := getAuthConfig(t, h, nil)
	if doc.Issuer != "https://idp.example/oidc" {
		t.Errorf("issuer = %q", doc.Issuer)
	}
	if doc.Audience != "https://api.lemul.local" {
		t.Errorf("audience = %q", doc.Audience)
	}
	if doc.ClientID != "cli-abc" {
		t.Errorf("client_id = %q", doc.ClientID)
	}
}

// "Authentication is off" and "the question went unanswered" need opposite
// behaviour from a client, so the answer is explicit rather than inferred from
// an empty issuer.
func TestAuthConfigSaysNotRequiredWhenAuthIsOff(t *testing.T) {
	h := authServer(t, "", "", "")

	code, doc := getAuthConfig(t, h, nil)
	if code != http.StatusOK {
		t.Fatalf("got %d, want 200", code)
	}
	if doc.Required {
		t.Error("required is true on a control plane with no issuer")
	}
	if doc.Issuer != "" || doc.ClientID != "" || doc.Audience != "" {
		t.Errorf("advertised configuration while authentication is off: %+v", doc)
	}
}

// A deployment that only uses the console has no CLI client to advertise. That
// must not become a broken document: the client turns the gap into a sentence
// naming the flag, which it can only do if `required` still says true.
func TestAuthConfigWithoutACLIClientStillReportsRequired(t *testing.T) {
	h := authServer(t, "https://idp.example/oidc", "https://api.lemul.local", "")

	_, doc := getAuthConfig(t, h, nil)
	if !doc.Required {
		t.Error("required is false, so a client would conclude no sign-in is needed")
	}
	if doc.ClientID != "" {
		t.Errorf("client_id = %q, want empty", doc.ClientID)
	}
}
