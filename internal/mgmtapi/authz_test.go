package mgmtapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lufzle/lemul/internal/auth"
	"github.com/lufzle/lemul/internal/authtest"
	"github.com/lufzle/lemul/internal/store"
	"github.com/lufzle/lemul/internal/store/storetest"
	"github.com/lufzle/lemul/internal/tunnel"
)

// Authorisation, with two real principals.
//
// Every test in this package now runs against a real issuer -- there is no
// unauthenticated mode left to fall back to -- so what distinguishes these is
// that they drive the HTTP surface with two DIFFERENT subjects. The properties
// below are all about telling two people apart, which a single principal cannot
// express however it is authenticated.

// authedServer builds a control plane and the issuer it trusts.
//
// The *Server comes back beside its handler so a test can check what a
// credential actually SAYS, not only what the response claimed about it.
func authedServer(t *testing.T) (http.Handler, *authtest.Issuer) {
	s, h, iss := authedServerFull(t)
	_ = s
	return h, iss
}

func authedServerFull(t *testing.T) (*Server, http.Handler, *authtest.Issuer) {
	t.Helper()
	iss := authtest.New(t)
	dir := storetest.OpenDirectory(t)
	if err := dir.EnsureCell(t.Context(), DefaultCellID, "test", "inline:test"); err != nil {
		t.Fatalf("ensure cell: %v", err)
	}
	s, err := New(Options{
		Store:               storetest.Open(t),
		Directory:           dir,
		SigningKey:          testSigningKey,
		AuthIssuer:          iss.URL,
		AuthAudience:        authtest.Audience,
		AuthConsoleClientID: authtest.ClientID,
		// The default derives the key set as issuer + "/jwks", which the local
		// issuer does serve -- passed explicitly so the two cannot drift.
		AuthJWKSURL: iss.JWKSURL,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Creating a workspace now places its task in the background (placement.go),
	// so every test that makes one leaves a goroutine behind. Registered AFTER
	// storetest.Open's cleanup and therefore run before it: cleanups are LIFO,
	// and the alternative is a placement reaching a closed pool and reporting it
	// as a database fault after the test that caused it has passed.
	t.Cleanup(s.WaitForPlacements)
	return s, s.Handler(), iss
}

func do(t *testing.T, h http.Handler, method, path, token, body string) (int, string) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code, strings.TrimSpace(w.Body.String())
}

// THE property PUT /v1/identity rests on, and the one that had no test.
//
// An ID token is handed TO clients rather than kept from them, so possessing
// one proves nothing about who is making this request. Without comparing its
// subject against the access token's, any valid ID token would let a caller
// write somebody else's email onto their own user row -- and that row is the
// identity every owner column in the console displays, which is what makes it
// worth forging.
func TestAnotherPersonsIDTokenIsRefused(t *testing.T) {
	h, iss := authedServer(t)

	access := iss.AccessToken(t, "alice")
	victim := iss.IDToken(t, "bob", "bob@example.com")

	code, body := do(t, h, http.MethodPut, "/v1/identity", access,
		fmt.Sprintf(`{"id_token":%q}`, victim))
	if code != http.StatusForbidden {
		t.Fatalf("alice recorded bob's identity: %d %s", code, body)
	}
}

// The same endpoint, working. Without this the test above would pass against a
// handler that refuses everything.
func TestMyOwnIDTokenIsAccepted(t *testing.T) {
	h, iss := authedServer(t)

	access := iss.AccessToken(t, "alice")
	mine := iss.IDToken(t, "alice", "alice@example.com")

	code, body := do(t, h, http.MethodPut, "/v1/identity", access,
		fmt.Sprintf(`{"id_token":%q}`, mine))
	if code != http.StatusOK {
		t.Fatalf("alice could not record her own identity: %d %s", code, body)
	}
	var out identityResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if out.Email != "alice@example.com" {
		t.Errorf("email came back %q", out.Email)
	}
	// And it named her organization, which is the other half of what this
	// endpoint is for.
	if out.OrgName != "Alice's Org" {
		t.Errorf("organization named %q, want Alice's Org", out.OrgName)
	}
}

// An ACCESS token is not an ID token. It is audienced to the API resource, so
// accepting one here would mean a caller could assert an email using the very
// token they already hold -- which is the self-asserted label this endpoint
// stopped taking.
func TestAnAccessTokenIsNotAcceptedAsAnIdentity(t *testing.T) {
	h, iss := authedServer(t)

	access := iss.Mint(t, authtest.Audience, "alice", "alice@example.com")
	code, body := do(t, h, http.MethodPut, "/v1/identity", access,
		fmt.Sprintf(`{"id_token":%q}`, access))
	if code != http.StatusBadRequest {
		t.Fatalf("an access token was accepted as an identity assertion: %d %s", code, body)
	}
}

// Every organization-scoped route needs a token, and there is nothing left for
// one to fall back to.
func TestOrganizationRoutesRequireAToken(t *testing.T) {
	h, iss := authedServer(t)

	// Sign alice in so there is a real organization to name.
	access := iss.AccessToken(t, "alice")
	code, body := do(t, h, http.MethodGet, "/v1/orgs", access, "")
	if code != http.StatusOK {
		t.Fatalf("listing organizations: %d %s", code, body)
	}
	var out orgListResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil || len(out.Orgs) != 1 {
		t.Fatalf("expected one organization, got %s", body)
	}
	org := out.Orgs[0].Slug

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/orgs"},
		{http.MethodGet, "/v1/orgs/" + org + "/workspaces"},
		{http.MethodPost, "/v1/orgs/" + org + "/workspaces/w1/sessions"},
		{http.MethodPost, "/v1/orgs/" + org + "/invites"},
		{http.MethodDelete, "/v1/orgs/" + org + "/sessions/" + org},
		{http.MethodPut, "/v1/identity"},
		{http.MethodGet, "/v1/status"},
	} {
		if code, _ := do(t, h, tc.method, tc.path, "", ""); code != http.StatusUnauthorized {
			t.Errorf("%s %s without a token got %d, want 401", tc.method, tc.path, code)
		}
	}
}

// Somebody else's organization is a 404, not a 403, and not a leak.
//
// The e2e suite proves this across two control planes; this proves it inside
// one, against two real tokens, which is the shape a deployment actually has.
func TestOneUserCannotNameAnothersOrganization(t *testing.T) {
	h, iss := authedServer(t)

	alice := iss.AccessToken(t, "alice")
	bob := iss.AccessToken(t, "bob")

	_, body := do(t, h, http.MethodGet, "/v1/orgs", alice, "")
	var out orgListResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil || len(out.Orgs) != 1 {
		t.Fatalf("alice's organizations: %s", body)
	}
	aliceOrg := out.Orgs[0].Slug

	// Bob signs up into his own, then names hers.
	if code, b := do(t, h, http.MethodGet, "/v1/orgs", bob, ""); code != http.StatusOK {
		t.Fatalf("bob's organizations: %d %s", code, b)
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/orgs/" + aliceOrg + "/workspaces"},
		{http.MethodGet, "/v1/orgs/" + aliceOrg + "/workspaces/w1/sessions"},
		{http.MethodPost, "/v1/orgs/" + aliceOrg + "/workspaces/w1/sessions"},
		{http.MethodPost, "/v1/orgs/" + aliceOrg + "/invites"},
		{http.MethodGet, "/v1/orgs/" + aliceOrg + "/workspaces/w1/fs?path=/"},
		{http.MethodGet, "/v1/orgs/" + aliceOrg + "/workspaces/w1/processes"},
		{http.MethodGet, "/v1/orgs/" + aliceOrg + "/workspaces/w1/resources"},
		{http.MethodGet, "/v1/orgs/" + aliceOrg + "/workspaces/w1/preflight"},
	} {
		code, b := do(t, h, tc.method, tc.path, bob, "")
		if code != http.StatusNotFound {
			t.Errorf("%s %s as a non-member got %d, want 404: %s", tc.method, tc.path, code, b)
		}
		// A 404 that names the organization would answer "does it exist" anyway.
		if strings.Contains(b, aliceOrg) {
			t.Errorf("%s %s leaked the organization slug: %s", tc.method, tc.path, b)
		}
	}
}

// ---------------------------------------------------------------------------
// Phase 4: authorisation INSIDE one organization.
//
// Everything above is about two organizations. These are about two people in
// the same one, which is where Phase 3 left nothing enforced at all.
// ---------------------------------------------------------------------------

// signUp returns a subject's access token and the slug of their own
// organization, having signed them up through the real path.
func signUp(t *testing.T, h http.Handler, iss *authtest.Issuer, subject string) (string, string) {
	t.Helper()
	tok := iss.AccessToken(t, subject)
	code, body := do(t, h, http.MethodGet, "/v1/orgs", tok, "")
	if code != http.StatusOK {
		t.Fatalf("%s signing up: %d %s", subject, code, body)
	}
	var out orgListResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil || len(out.Orgs) != 1 {
		t.Fatalf("%s owns %s", subject, body)
	}
	return tok, out.Orgs[0].Slug
}

// invite adds a subject to an organization at a role, through the real invite
// and redeem endpoints rather than by writing a membership row.
func invite(t *testing.T, h http.Handler, owner, org, joiner, role string) {
	t.Helper()
	code, body := do(t, h, http.MethodPost, "/v1/orgs/"+org+"/invites", owner,
		fmt.Sprintf(`{"role":%q}`, role))
	if code != http.StatusCreated {
		t.Fatalf("creating an invite: %d %s", code, body)
	}
	var iv struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(body), &iv); err != nil {
		t.Fatalf("invite: %v (%s)", err, body)
	}
	if code, b := do(t, h, http.MethodPost, "/v1/invites/"+iv.Code+"/redeem", joiner, ""); code != http.StatusOK {
		t.Fatalf("redeeming: %d %s", code, b)
	}
}

// createWS makes a workspace and returns its name.
//
// 202, not 201: creating a workspace places its task, and the record exists
// while the machine does not (increment 4).
func createWS(t *testing.T, h http.Handler, tok, org, body string) string {
	t.Helper()
	code, b := do(t, h, http.MethodPost, "/v1/orgs/"+org+"/workspaces", tok, body)
	if code != http.StatusAccepted {
		t.Fatalf("creating a workspace: %d %s", code, b)
	}
	var doc workspaceDoc
	if err := json.Unmarshal([]byte(b), &doc); err != nil {
		t.Fatalf("workspace: %v (%s)", err, b)
	}
	return doc.ID
}

// THE gap Phase 4 exists to close, at the workspace level.
//
// Being in an organization is not being in every workspace in it. Before this,
// membership of the organization was the only check any {wid} route made -- so
// a colleague could read the file tree, the process list and the command lines
// of a workspace nobody had added them to. That is silent, unlike attaching to
// a session, which is why section 2.5 calls it the more expensive gap.
func TestAnOrganizationMemberCannotReachAWorkspaceTheyAreNotIn(t *testing.T) {
	h, iss := authedServer(t)
	alice, aliceOrg := signUp(t, h, iss, "alice")
	bob, _ := signUp(t, h, iss, "bob")
	invite(t, h, alice, aliceOrg, bob, "user")

	ws := createWS(t, h, alice, aliceOrg, `{"name":"secret-project"}`)
	base := "/v1/orgs/" + aliceOrg + "/workspaces/" + ws

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, base},
		{http.MethodGet, base + "/sessions"},
		{http.MethodPost, base + "/sessions"},
		{http.MethodGet, base + "/preflight"},
		// The explorer trio, which had no check of any kind.
		{http.MethodGet, base + "/fs?path=/"},
		{http.MethodGet, base + "/processes"},
		{http.MethodGet, base + "/resources"},
		{http.MethodPatch, base},
		{http.MethodDelete, base},
	} {
		code, b := do(t, h, tc.method, tc.path, bob, `{"name":"renamed"}`)
		if code != http.StatusNotFound {
			t.Errorf("%s %s as a non-member of the workspace got %d, want 404: %s",
				tc.method, tc.path, code, b)
		}
		// The name is what is being withheld, so it must not come back in the
		// refusal -- the same reasoning as the organization slug above.
		if strings.Contains(b, ws) {
			t.Errorf("%s %s leaked the workspace name: %s", tc.method, tc.path, b)
		}
	}

	// And the workspace does not appear in what they may list.
	code, b := do(t, h, http.MethodGet, "/v1/orgs/"+aliceOrg+"/workspaces", bob, "")
	if code != http.StatusOK {
		t.Fatalf("listing as a member: %d %s", code, b)
	}
	if strings.Contains(b, ws) {
		t.Errorf("a workspace they were never added to appears in the listing: %s", b)
	}
}

// The other half: an organization OWNER reaches everything in it without being
// added to each workspace one at a time (section 2.5's "downward" rule).
// Without this the test above would pass against a check that refuses everyone.
func TestAnOrganizationOwnerReachesEveryWorkspaceInIt(t *testing.T) {
	h, iss := authedServer(t)
	alice, aliceOrg := signUp(t, h, iss, "alice")
	bob, _ := signUp(t, h, iss, "bob")
	// Bob is an OWNER of alice's organization, alice's workspace is not his.
	invite(t, h, alice, aliceOrg, bob, "owner")

	ws := createWS(t, h, alice, aliceOrg, `{"name":"shared-thing"}`)

	code, b := do(t, h, http.MethodGet, "/v1/orgs/"+aliceOrg+"/workspaces/"+ws, bob, "")
	if code != http.StatusOK {
		t.Fatalf("an organization owner could not read a workspace in it: %d %s", code, b)
	}
}

// startSession writes a session RECORD for a subject, without going through
// POST .../sessions.
//
// That endpoint places a workspace task, and this package has no runner -- it
// would answer 503 before reaching anything worth testing. The session record
// is setup here rather than the subject under test; what is under test is what
// the endpoint handlers do with one once it exists.
func startSession(t *testing.T, srv *Server, iss *authtest.Issuer, subject, org, ws string) string {
	t.Helper()
	sc := scopeOf(t, srv, iss, subject, org)
	wsRec, err := srv.getWorkspaceByName(t.Context(), sc, ws)
	if err != nil {
		t.Fatalf("resolving workspace %s: %v", ws, err)
	}
	sess, err := srv.createSessionRecord(t.Context(), sc, wsRec.ID)
	if err != nil {
		t.Fatalf("creating a session record: %v", err)
	}
	return sess.ID
}

// scopeOf resolves a subject against an organization, the way a request does.
func scopeOf(t *testing.T, srv *Server, iss *authtest.Issuer, subject, org string) scope {
	t.Helper()
	p, err := srv.resolvePrincipal(t.Context(), auth.Principal{Issuer: iss.URL, Subject: subject})
	if err != nil {
		t.Fatalf("resolving %s: %v", subject, err)
	}
	o, err := srv.dir.OrgBySlug(t.Context(), org)
	if err != nil {
		t.Fatalf("resolving %s: %v", org, err)
	}
	return scope{
		Scope:     store.Scope{TenantID: o.ID, UserID: p.UserID},
		Org:       o,
		Principal: p,
	}
}

// SECTION 2.5's owner-scoped attach, which is the gap this phase opened with.
//
// Endpoint negotiation minted a control credential for ANY session id its
// caller could name, so a colleague could land in another person's live Claude
// Code with a keyboard. Both attachers write the same PTY stdin, so that is not
// observation -- it is co-driving somebody's agent.
func TestAColleagueCannotGetAControlCredentialForMySession(t *testing.T) {
	srv, h, iss := authedServerFull(t)
	alice, aliceOrg := signUp(t, h, iss, "alice")
	bob, _ := signUp(t, h, iss, "bob")
	// Bob owns the ORGANIZATION, so he can see the session -- this is the
	// strongest caller who still must not be handed a keyboard.
	invite(t, h, alice, aliceOrg, bob, "owner")

	ws := createWS(t, h, alice, aliceOrg, `{"name":"alices-work"}`)
	sid := startSession(t, srv, iss, "alice", aliceOrg, ws)
	warmWorkspace(t, srv, aliceOrg, ws)
	endpoint := "/v1/orgs/" + aliceOrg + "/sessions/" + sid + "/endpoint"

	// Control is refused, and the refusal says what to do instead rather than
	// leaving the caller to guess.
	code, b := do(t, h, http.MethodGet, endpoint, bob, "")
	if code != http.StatusForbidden {
		t.Fatalf("a colleague was handed a credential for another's session: %d %s", code, b)
	}
	if !strings.Contains(b, "--viewer") {
		t.Errorf("the refusal does not name the way to watch instead: %s", b)
	}

	// Asking explicitly for viewer is allowed, and comes back as viewer. It is
	// conspicuous -- the attach bumps the count alice sees in `lem` -- which is
	// what makes it acceptable where silent reading would not be.
	code, b = do(t, h, http.MethodGet, endpoint+"?mode=viewer", bob, "")
	if code != http.StatusOK {
		t.Fatalf("an owner could not watch a session in their organization: %d %s", code, b)
	}
	var ep endpointResponse
	if err := json.Unmarshal([]byte(b), &ep); err != nil {
		t.Fatalf("endpoint: %v (%s)", err, b)
	}
	if ep.Mode != tunnel.ModeViewer {
		t.Errorf("mode came back %q, want viewer", ep.Mode)
	}

	// And the credential really carries viewer, so the relay enforces it
	// without asking anyone. A mode that lived only in the response would be
	// advice rather than authorisation.
	// Read the claims straight off the signature. Redeeming is the RELAY's job
	// now, and consuming the nonce here would test that service through this
	// one -- what matters at this end is what was signed.
	claims, err := srv.signer.VerifyAttach(ep.Credential)
	if err != nil {
		t.Fatalf("the minted credential did not verify: %v", err)
	}
	if claims.Mode != tunnel.ModeViewer {
		t.Errorf("the signed credential says mode=%q; the relay would let them type", claims.Mode)
	}
}

// The session's own user still gets control, or the test above would pass
// against an endpoint that refuses everybody.
func TestMyOwnSessionStillGivesMeControl(t *testing.T) {
	srv, h, iss := authedServerFull(t)
	alice, aliceOrg := signUp(t, h, iss, "alice")

	ws := createWS(t, h, alice, aliceOrg, `{"name":"mine"}`)
	sid := startSession(t, srv, iss, "alice", aliceOrg, ws)
	warmWorkspace(t, srv, aliceOrg, ws)

	code, b := do(t, h, http.MethodGet,
		"/v1/orgs/"+aliceOrg+"/sessions/"+sid+"/endpoint", alice, "")
	if code != http.StatusOK {
		t.Fatalf("alice could not attach to her own session: %d %s", code, b)
	}
	var ep endpointResponse
	if err := json.Unmarshal([]byte(b), &ep); err != nil {
		t.Fatalf("endpoint: %v (%s)", err, b)
	}
	if ep.Mode != tunnel.ModeControl {
		t.Errorf("mode came back %q, want control", ep.Mode)
	}
}

// A workspace member who is not the session's user and does not own the
// workspace cannot see the session at all -- 404, matching what their own
// listing says about it. A 403 here would confirm the session exists.
func TestAPlainWorkspaceMemberCannotSeeAnothersSession(t *testing.T) {
	srv, h, iss := authedServerFull(t)
	alice, aliceOrg := signUp(t, h, iss, "alice")
	bob, _ := signUp(t, h, iss, "bob")
	invite(t, h, alice, aliceOrg, bob, "user")

	ws := createWS(t, h, alice, aliceOrg, `{"name":"team-thing"}`)
	sid := startSession(t, srv, iss, "alice", aliceOrg, ws)

	// Alice adds bob to the workspace as a plain member. His user id comes from
	// the organization's member list, which is the only place a client can get
	// one -- an email would not do, since neither has relayed an ID token.
	addMember(t, h, alice, aliceOrg, ws, otherMember(t, h, alice, aliceOrg), roleUser)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/orgs/" + aliceOrg + "/sessions/" + sid + "/endpoint"},
		{http.MethodPost, "/v1/orgs/" + aliceOrg + "/sessions/" + sid + "/stop"},
		{http.MethodPost, "/v1/orgs/" + aliceOrg + "/sessions/" + sid + "/resume"},
		{http.MethodDelete, "/v1/orgs/" + aliceOrg + "/sessions/" + sid},
	} {
		code, b := do(t, h, tc.method, tc.path, bob, "")
		if code != http.StatusNotFound {
			t.Errorf("%s %s as a plain member got %d, want 404: %s", tc.method, tc.path, code, b)
		}
	}
}

// warmWorkspace makes a workspace look like one that is already up: a live
// tunnel, and a preflight verdict of `skipped`.
//
// Both halves are needed because endpoint negotiation now places the task and
// waits for a Bedrock verdict before minting a credential (see handleEndpoint).
// Without the tunnel it answers 503 for want of a runner; without the verdict
// it waits out preflightWait to fail open, which is correct behaviour and a
// twenty-second test.
//
// `skipped` rather than a passing report because that is what a workspace not
// using Bedrock actually reports, which is every workspace in this package.
func warmWorkspace(t *testing.T, srv *Server, org, ws string) {
	t.Helper()
	tenant := srv.OrgIDForSlug(org)
	srv.reg.AddWorkspace(fakeTunnel(t, tenant, ws))
	srv.preflights.put(tenant, tunnel.PreflightReport{WorkspaceID: ws, Skipped: true})
}

// otherMember returns the user id of the one organization member who is not the
// caller. It reads GET /v1/orgs/{org}/members, which exists precisely because a
// membership verb takes a user id and nothing else hands one out.
func otherMember(t *testing.T, h http.Handler, tok, org string) string {
	t.Helper()
	code, b := do(t, h, http.MethodGet, "/v1/orgs/"+org+"/members", tok, "")
	if code != http.StatusOK {
		t.Fatalf("listing organization members: %d %s", code, b)
	}
	var out memberListResponse
	if err := json.Unmarshal([]byte(b), &out); err != nil {
		t.Fatalf("members: %v (%s)", err, b)
	}
	for _, m := range out.Members {
		if !m.Me {
			return m.UserID
		}
	}
	t.Fatalf("no other member in %s: %s", org, b)
	return ""
}

func addMember(t *testing.T, h http.Handler, tok, org, ws, userID, role string) {
	t.Helper()
	code, b := do(t, h, http.MethodPut,
		"/v1/orgs/"+org+"/workspaces/"+ws+"/members/"+userID, tok,
		fmt.Sprintf(`{"role":%q}`, role))
	if code != http.StatusOK {
		t.Fatalf("adding %s to %s: %d %s", userID, ws, code, b)
	}
}
