package relay

import (
	"go/build"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lufzle/lemul/internal/creds"
)

var testSigningKey = []byte("relay-test-signing-key-0123456789abcd")

func newTestRelay(t *testing.T) *Server {
	t.Helper()
	s, err := New(Options{SigningKey: testSigningKey})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// THE property the whole split exists to create, asserted the only way it can
// be: over the package's own import graph.
//
// Nothing else would notice this decaying. A store import added for one
// "quick" lookup compiles, passes every behavioural test, and quietly makes
// this service one that reads customer rows -- at which point section 1.3's A3,
// "we hold no key that can decrypt your session", is false again and nobody
// finds out from a test failure.
//
// The list is what the data path legitimately needs: framing, the tunnel
// registry, credential verification, the client protocol, and the shared
// dial-out config. Adding to it should be an argument somebody has, which is
// what this test forces.
func TestTheRelayImportsNothingThatReadsCustomerData(t *testing.T) {
	forbidden := []string{
		"github.com/lufzle/lemul/internal/store",
		"github.com/lufzle/lemul/internal/store/celldb",
		"github.com/lufzle/lemul/internal/directory",
		"github.com/lufzle/lemul/internal/directory/directorydb",
		"github.com/lufzle/lemul/internal/mgmtapi",
		"github.com/lufzle/lemul/internal/runner",
		"github.com/lufzle/lemul/internal/driver",
		"github.com/lufzle/lemul/internal/auth",
		"github.com/lufzle/lemul/internal/gateway",
		"github.com/lufzle/lemul/internal/bedrock",
	}
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("reading this package: %v", err)
	}
	for _, imp := range pkg.Imports {
		if slices.Contains(forbidden, imp) {
			t.Errorf("the relay imports %s.\n"+
				"This service is meant to see opaque bytes and nothing else. If it now "+
				"needs customer data, that is a change to what we can claim about the "+
				"data path (2.7), not a refactor -- say so in section 12 first.", imp)
		}
	}
	// And the other direction: importing the Management API would make "neither
	// service calls the other" a comment rather than a fact.
	for _, imp := range pkg.Imports {
		if strings.HasSuffix(imp, "/mgmtapi") {
			t.Errorf("the relay imports the Management API (%s); they must not call each other", imp)
		}
	}
}

// A credential is single-use. The signature proves who minted it and cannot
// express that it has been spent, so the nonce set is the only thing that can.
func TestAttachCredentialIsSingleUse(t *testing.T) {
	s := newTestRelay(t)
	cred, err := s.signer.AttachToken("sess-1", "org-1", "ws-1", "slug-1", "control", creds.AttachTTL)
	if err != nil {
		t.Fatalf("AttachToken: %v", err)
	}

	claims, ok := s.redeemAttach(cred.Secret())
	if !ok {
		t.Fatal("a freshly minted attach credential was refused")
	}
	if claims.SessionID != "sess-1" || claims.Mode != "control" {
		t.Fatalf("claims came back wrong: %+v", claims)
	}
	if _, ok := s.redeemAttach(cred.Secret()); ok {
		t.Fatal("the same credential was redeemed twice; a leaked attach URL could be replayed")
	}
}

func TestRedeemAttachRejectsForgeryAndExpiry(t *testing.T) {
	s := newTestRelay(t)

	other, err := creds.NewSigner([]byte("a-completely-different-signing-key-0123"))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	forged, err := other.AttachToken("sess-1", "org-1", "ws-1", "slug-1", "control", creds.AttachTTL)
	if err != nil {
		t.Fatalf("AttachToken: %v", err)
	}
	if _, ok := s.redeemAttach(forged.Secret()); ok {
		t.Error("a credential signed with another key was accepted")
	}

	expired, err := s.signer.AttachToken("sess-1", "org-1", "ws-1", "slug-1", "control", time.Nanosecond)
	if err != nil {
		t.Fatalf("AttachToken: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, ok := s.redeemAttach(expired.Secret()); ok {
		t.Error("an expired credential was accepted")
	}

	if _, ok := s.redeemAttach(""); ok {
		t.Error("an empty credential was accepted")
	}
}

// A viewer that can promote itself to controller is the same violation as a
// viewer that can type, so the mode has to survive round-tripping through the
// credential rather than being re-read from the request.
func TestAttachModeTravelsInTheCredential(t *testing.T) {
	s := newTestRelay(t)
	cred, err := s.signer.AttachToken("sess-1", "org-1", "ws-1", "slug-1", "viewer", creds.AttachTTL)
	if err != nil {
		t.Fatalf("AttachToken: %v", err)
	}
	claims, ok := s.redeemAttach(cred.Secret())
	if !ok {
		t.Fatal("credential refused")
	}
	if claims.Mode != "viewer" {
		t.Fatalf("mode came back %q, want viewer", claims.Mode)
	}
}

// The nonce set is swept by expiry rather than capped, so an expired entry must
// not keep occupying it.
func TestUsedNoncesAreSweptOnceExpired(t *testing.T) {
	u := newUsedAttachNonces()
	if !u.use("a", time.Now().Add(-time.Second)) {
		t.Fatal("first use was refused")
	}
	// "b" forces a sweep, which should evict the already-expired "a".
	u.use("b", time.Now().Add(time.Minute))
	u.mu.Lock()
	_, stillThere := u.seen["a"]
	u.mu.Unlock()
	if stillThere {
		t.Error("an expired nonce was not swept; the set grows without bound")
	}
}

// Attach with no live data tunnel must answer, not hang.
//
// This window is NEW and is the honest cost of the split: endpoint negotiation
// placed the task and waited for it, and it can die before the client connects.
// The relay cannot place anything, so the only useful thing it can do is say so
// and name the remedy -- which the client can act on and it cannot.
func TestAttachWithNoLiveTaskSaysSoRatherThanHanging(t *testing.T) {
	s := newTestRelay(t)
	cred, err := s.signer.AttachToken("sess-1", "org-1", "ghost", "slug-1", "control", creds.AttachTTL)
	if err != nil {
		t.Fatalf("AttachToken: %v", err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/sessions/sess-1/attach?credential=" + cred.Secret())
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("attach with no task got %d, want 503", resp.StatusCode)
	}
}

// The credential names the session it is for. Presenting one for a DIFFERENT
// session id in the path would otherwise let a single valid credential open any
// session on the relay.
func TestACredentialForAnotherSessionIsRefused(t *testing.T) {
	s := newTestRelay(t)
	cred, err := s.signer.AttachToken("sess-mine", "org-1", "ws-1", "slug-1", "control", creds.AttachTTL)
	if err != nil {
		t.Fatalf("AttachToken: %v", err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/sessions/sess-theirs/attach?credential=" + cred.Secret())
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a credential for another session got %d, want 401", resp.StatusCode)
	}
}

// The data tunnel refuses a credential that does not match the generation the
// task announces, which is what stops an arbitrary caller registering as a
// workspace and receiving its attach streams.
func TestTheDataTunnelRefusesAMismatchedGeneration(t *testing.T) {
	s := newTestRelay(t)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	// A credential genuinely minted for generation 1, announced as generation 2.
	cred := s.signer.WorkspaceToken("org-1", "ws-1", 1)
	u := srv.URL + "/v1/tunnel/data?tenant=org-1&workspace=ws-1&generation=2&v=1"
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, u, nil)
	req.Header.Set("Authorization", "Bearer "+cred.Secret())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a mismatched generation got %d, want 401", resp.StatusCode)
	}
}
