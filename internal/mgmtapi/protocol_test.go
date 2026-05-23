package mgmtapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lufzle/lemul/internal/tunnel"
)

// dialRunner makes the upgrade request a runner makes, with the credential
// derived for its organization so the request reaches the version check.
func dialRunner(t *testing.T, s *testEnv, org, version string) *httptest.ResponseRecorder {
	t.Helper()
	u := "/v1/tunnel/runner?tenant=" + org + "&runner_id=r1"
	if version != "" {
		u += "&" + tunnel.ProtocolParam + "=" + version
	}
	r := httptest.NewRequest(http.MethodGet, u, nil)
	r.Header.Set("Authorization", "Bearer "+s.signer.RunnerToken(org).Secret())
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

// The supervisor is baked into the workspace image, so a customer on a stale
// image dials a control plane that has moved on. Every symptom of that reads
// like an application bug, so it has to be caught at the handshake.
func TestTunnelRefusesAnUnversionedAgent(t *testing.T) {
	s, sc := testServer(t)
	w := dialRunner(t, s, sc.TenantID, "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("an agent announcing no version got %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "protocol") {
		t.Errorf("the refusal does not mention the protocol: %q", w.Body.String())
	}
}

func TestTunnelRefusesAMalformedVersion(t *testing.T) {
	s, sc := testServer(t)
	for _, v := range []string{"abc", "1.0", "-", "9999999999999999999999"} {
		if w := dialRunner(t, s, sc.TenantID, v); w.Code != http.StatusBadRequest {
			t.Errorf("version %q got %d, want 400", v, w.Code)
		}
	}
}

// The two directions of skew need different advice: rolling the workspace image
// forward fixes one and wastes a deploy on the other.
func TestTunnelNamesWhichSideIsStale(t *testing.T) {
	s, sc := testServer(t)

	if tunnel.MinProtocolVersion > 0 {
		w := dialRunner(t, s, sc.TenantID, fmt.Sprint(tunnel.MinProtocolVersion-1))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("an older agent got %d, want 400", w.Code)
		}
		if !strings.Contains(w.Body.String(), "workspace image") {
			t.Errorf("an older agent should be told to roll the image forward, got %q", w.Body.String())
		}
	}

	w := dialRunner(t, s, sc.TenantID, fmt.Sprint(tunnel.ProtocolVersion+1))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a newer agent got %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "control plane") {
		t.Errorf("a newer agent should be told to update the control plane, got %q", w.Body.String())
	}
}

// The workspace tunnel carries the same check, reached only once the workspace
// credential verifies.
func TestWorkspaceTunnelChecksTheProtocol(t *testing.T) {
	s, sc := testServer(t)
	ws, err := s.createWorkspace(context.Background(), sc, "w1", accessOwner)
	if err != nil {
		t.Fatal(err)
	}
	cred := s.signer.WorkspaceToken(sc.TenantID, "w1", uint64(ws.Generation)).Secret()

	dial := func(version string) *httptest.ResponseRecorder {
		u := "/v1/tunnel/workspace?tenant=" + sc.TenantID + "&workspace=w1"
		if version != "" {
			u += "&" + tunnel.ProtocolParam + "=" + version
		}
		r := httptest.NewRequest(http.MethodGet, u, nil)
		r.Header.Set("Authorization", "Bearer "+cred)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}

	if w := dial(""); w.Code != http.StatusBadRequest ||
		!strings.Contains(w.Body.String(), "protocol") {
		t.Errorf("an unversioned workspace agent got %d %q, want a 400 naming the protocol",
			w.Code, w.Body.String())
	}

	// A current version gets past the check and on to the WebSocket upgrade,
	// which cannot complete against a recorder -- gorilla answers its own 400
	// ("Bad Request", no hijacker). So the body, not the status, is what
	// distinguishes "refused by us" from "got as far as the upgrade".
	if w := dial(fmt.Sprint(tunnel.ProtocolVersion)); strings.Contains(w.Body.String(), "protocol") {
		t.Errorf("the current protocol version was refused: %q", w.Body.String())
	}
}

// Authentication comes first on both tunnels. A bad credential must read as 401
// whatever version it claims, and an unauthenticated caller is not owed a
// reading of which protocol versions this deployment speaks.
func TestCredentialIsCheckedBeforeTheProtocol(t *testing.T) {
	s, sc := testServer(t)

	for _, version := range []string{"", "abc", fmt.Sprint(tunnel.ProtocolVersion)} {
		u := "/v1/tunnel/runner?tenant=" + sc.TenantID
		if version != "" {
			u += "&" + tunnel.ProtocolParam + "=" + version
		}
		r := httptest.NewRequest(http.MethodGet, u, nil)
		r.Header.Set("Authorization", "Bearer wrong-token")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("bad token with version %q got %d, want 401", version, w.Code)
		}
		if strings.Contains(w.Body.String(), "protocol") {
			t.Errorf("an unauthenticated caller was told about protocol versions: %q", w.Body.String())
		}
	}
}
