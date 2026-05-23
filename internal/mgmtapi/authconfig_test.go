package mgmtapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lufzle/lemul/internal/store/storetest"
)

func authServer(t *testing.T, issuer, audience, cliClientID string) http.Handler {
	t.Helper()
	s, err := New(Options{
		Store:           storetest.Open(t),
		Directory:       storetest.OpenDirectory(t),
		AuthIssuer:      issuer,
		AuthAudience:    audience,
		AuthCLIClientID: cliClientID,
		SigningKey:      testSigningKey,
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

// A server that cannot validate tokens cannot be built at all, which is what
// makes `required: true` an invariant rather than a setting.
//
// This replaces a test asserting the opposite -- that an issuer-less server
// answered `required: false`. That mode is gone: it put an unauthenticated
// control plane one omitted flag away, and every organization in it within reach
// of anyone who could open a socket.
func TestAServerThatCannotValidateTokensCannotBeBuilt(t *testing.T) {
	for _, tc := range []struct{ name, issuer, audience string }{
		{"neither", "", ""},
		{"no issuer", "", "https://api.lemul.local"},
		{"no audience", "https://idp.example/oidc", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(Options{
				Store:        storetest.Open(t),
				Directory:    storetest.OpenDirectory(t),
				AuthIssuer:   tc.issuer,
				AuthAudience: tc.audience,
				SigningKey:   testSigningKey,
			})
			if err == nil {
				t.Fatal("built a control plane that would not validate tokens")
			}
		})
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
