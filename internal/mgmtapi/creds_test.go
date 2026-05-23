package mgmtapi

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/lufzle/lemul/internal/auth"
	"github.com/lufzle/lemul/internal/authtest"
	"github.com/lufzle/lemul/internal/store"
	"github.com/lufzle/lemul/internal/store/storetest"
)

// testSigningKey is fixed so that a test can build two servers and assert they
// agree -- which is the whole point of deriving credentials rather than storing
// them.
var testSigningKey = []byte("controlplane-test-signing-key-0123456789")

// Each server signs up its own subject, so every test gets its own organization
// in one shared database and cannot see another's rows -- using the isolation
// under test to isolate the tests.
var subjectSeq atomic.Uint32

// testEnv is a server together with the identity provider it validates against.
//
// The two travel together because they are useless apart: there is no
// unauthenticated mode, so a server that is not pointed at an issuer cannot be
// constructed, and a token that is not signed by that issuer cannot reach a
// handler. Embedding *Server keeps every `s.signer` / `s.dir` call site reading
// as it did.
type testEnv struct {
	*Server
	iss *authtest.Issuer
	// subject is this server's own user, unique per test.
	subject string
}

// newTestServer builds a server on the shared database, against a local issuer.
// Nothing is bootstrapped by hand: the subject signs up through the real path,
// which is what keeps that path exercised by every test in this package.
func newTestServer(t *testing.T) *testEnv {
	t.Helper()
	st := storetest.Open(t)
	dir := storetest.OpenDirectory(t)
	if err := dir.EnsureCell(context.Background(), DefaultCellID, "test", "inline:test"); err != nil {
		t.Fatalf("ensure cell: %v", err)
	}
	iss := authtest.New(t)
	s, err := New(Options{
		Store: st, Directory: dir, SigningKey: testSigningKey,
		AuthIssuer:          iss.URL,
		AuthAudience:        authtest.Audience,
		AuthJWKSURL:         iss.JWKSURL,
		AuthConsoleClientID: authtest.ClientID,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// See authedServerFull: a background placement must not outlive the pool it
	// queries.
	t.Cleanup(s.WaitForPlacements)
	return &testEnv{Server: s, iss: iss, subject: fmt.Sprintf("user-%d", subjectSeq.Add(1))}
}

// testServer returns a server and a scope for its subject's own organization,
// which is what the state.go methods take now.
func testServer(t *testing.T) (*testEnv, scope) {
	t.Helper()
	s := newTestServer(t)
	return s, s.scopeFor(t, s.subject)
}

// scopeFor resolves a subject, signing them up on first call, and returns a
// scope for their own organization.
func (e *testEnv) scopeFor(t *testing.T, subject string) scope {
	t.Helper()
	ctx := context.Background()
	p, err := e.resolvePrincipal(ctx, auth.Principal{Issuer: e.iss.URL, Subject: subject})
	if err != nil {
		t.Fatalf("resolve %s: %v", subject, err)
	}
	org, err := e.dir.OrgByID(ctx, p.HomeOrgID)
	if err != nil {
		t.Fatalf("resolve the home organization: %v", err)
	}
	return scope{
		Scope:     store.Scope{TenantID: org.ID, UserID: p.UserID},
		Org:       org,
		Role:      roleOwner,
		Principal: p,
	}
}

func TestNewRequiresASigningKey(t *testing.T) {
	if _, err := New(Options{
		Store:     storetest.Open(t),
		Directory: storetest.OpenDirectory(t),
	}); err == nil {
		t.Fatal("New accepted a missing signing key; credentials would be underived " +
			"and the failure would surface as workspace tasks exiting")
	}
}

// A directory is not optional. Without one nothing can resolve a caller to a
// cell, so every request would fail at the first lookup -- better to refuse at
// construction than to boot into a control plane that cannot serve anyone.
func TestNewRequiresADirectory(t *testing.T) {
	if _, err := New(Options{
		Store: storetest.Open(t), SigningKey: testSigningKey,
	}); err == nil {
		t.Fatal("New accepted a server with no directory")
	}
}

// The failure this replaces: a restart emptied the credential map, so every live
// workspace task presented something nothing recognised, got 401, and exited
// taking its sessions with it. A second server holding the same key must accept
// what the first one issued.
func TestWorkspaceCredentialSurvivesAControlPlaneRestart(t *testing.T) {
	first := newTestServer(t)
	cred := first.signer.WorkspaceToken("org-1", "myproj", 7)

	second := newTestServer(t) // a fresh process, same configured key
	if !second.signer.VerifyWorkspace("org-1", "myproj", 7, cred.Secret()) {
		t.Fatal("a restarted control plane rejected a live task's credential")
	}
}

// Placing a replacement task is the ONLY thing that may lock out the task it
// replaces. This is the property the generation bump buys, and the reason the
// bump must not happen merely because a tunnel was briefly missing.
func TestNewGenerationInvalidatesThePreviousTask(t *testing.T) {
	s := newTestServer(t)
	old := s.signer.WorkspaceToken("org-1", "myproj", 1)
	if s.signer.VerifyWorkspace("org-1", "myproj", 2, old.Secret()) {
		t.Fatal("a replaced task's credential still verified against the new generation")
	}
}

// Two concurrent first requests from the same principal must converge on ONE
// organization.
//
// The window is real: sign-up creates an organization and THEN claims the
// binding, so both callers create one and exactly one wins BindPrincipal's
// ON CONFLICT DO NOTHING. The loser has to adopt the winner's and discard its
// own -- because a person with two home organizations has no answer to "which
// one is mine", and the CLI's --org default would pick differently on
// alternating runs.
//
// Run through resolvePrincipal rather than signUp directly, so the test covers
// the path a request actually takes, including the cell-side user row that also
// races on app_user's global UNIQUE.
func TestConcurrentFirstRequestsConvergeOnOneOrganization(t *testing.T) {
	s := newTestServer(t)
	p := auth.Principal{Issuer: s.iss.URL, Subject: s.subject}

	const n = 8
	type result struct {
		p   auth.Principal
		err error
	}
	out := make(chan result, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := s.resolvePrincipal(context.Background(), p)
			out <- result{got, err}
		}()
	}
	wg.Wait()
	close(out)

	orgs := map[string]bool{}
	users := map[string]bool{}
	for r := range out {
		if r.err != nil {
			t.Fatalf("resolvePrincipal: %v", r.err)
		}
		orgs[r.p.HomeOrgID] = true
		users[r.p.UserID] = true
	}
	if len(orgs) != 1 {
		t.Errorf("%d concurrent sign-ups produced %d home organizations", n, len(orgs))
	}
	if len(users) != 1 {
		t.Errorf("%d concurrent sign-ups produced %d user rows", n, len(users))
	}
}

// Resolution is idempotent: signing in again finds the same rows rather than
// creating more. Cheap to state, and the thing that would break first if the
// lookup and the sign-up paths ever disagreed about what identifies a person.
func TestResolvingTwiceIsStable(t *testing.T) {
	s := newTestServer(t)
	p := auth.Principal{Issuer: s.iss.URL, Subject: s.subject}

	first, err := s.resolvePrincipal(context.Background(), p)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := s.resolvePrincipal(context.Background(), p)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first.UserID != second.UserID || first.HomeOrgID != second.HomeOrgID {
		t.Errorf("resolution is not stable: %+v then %+v", first, second)
	}
}

// A different subject is a different person, even on the same issuer. Obvious,
// and worth pinning because the directory key is a PAIR -- a lookup that
// accidentally matched on issuer alone would put an entire identity provider
// into one organization.
func TestDifferentSubjectsGetDifferentOrganizations(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	a, err := s.resolvePrincipal(ctx, auth.Principal{Issuer: s.iss.URL, Subject: "alice"})
	if err != nil {
		t.Fatalf("alice: %v", err)
	}
	b, err := s.resolvePrincipal(ctx, auth.Principal{Issuer: s.iss.URL, Subject: "bob"})
	if err != nil {
		t.Fatalf("bob: %v", err)
	}
	if a.HomeOrgID == b.HomeOrgID {
		t.Error("two subjects share a home organization")
	}
	if a.UserID == b.UserID {
		t.Error("two subjects share a user row")
	}
}
