package e2e_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Organizations, end to end.
//
// The properties under test are the ones Phase 3 claims: a user signs up into
// their own organization without anybody seeding a row, two users in one
// deployment cannot see each other, and an invite is the only way across.
//
// A second USER is a second SUBJECT against the same control plane, holding its
// own token. That is what a deployment actually looks like, and it only became
// expressible when the suite started signing real tokens: it used to take a
// second control-plane instance with a second dev principal, because one
// process had exactly one identity to offer.

// person is a second principal against the same control plane.
type person struct {
	s     *stack
	token string
	org   string
	orgID string
	name  string
	role  string
}

// otherUser mints a token for a fresh subject and signs them up.
func (s *stack) otherUser(t *testing.T) *person {
	t.Helper()
	subject := newSubject()
	p := &person{s: s, token: s.iss.AccessToken(t, subject)}
	orgs := p.orgs(t)
	if len(orgs) != 1 {
		t.Fatalf("a fresh principal owns %d organizations, want 1", len(orgs))
	}
	p.org, p.name, p.role = orgs[0].Slug, orgs[0].Name, orgs[0].Role
	p.orgID = s.cp.OrgIDForSlug(p.org)
	return p
}

// do issues a request as this person. Their token is what makes them somebody
// else; nothing about the address they call distinguishes them.
func (p *person) do(t *testing.T, method, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, p.s.baseURL+path, nil)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(body))
}

type orgDocT struct {
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	Personal bool   `json:"personal"`
}

func (p *person) orgs(t *testing.T) []orgDocT {
	t.Helper()
	code, body := p.do(t, http.MethodGet, "/v1/orgs")
	if code != http.StatusOK {
		t.Fatalf("list organizations: %d: %s", code, body)
	}
	var out struct {
		Orgs []orgDocT `json:"orgs"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("list organizations: %v", err)
	}
	return out.Orgs
}

// get and post return the status, so a test can assert on what a non-member is
// told rather than only on a happy path.
func (p *person) get(t *testing.T, path string) (int, string) {
	t.Helper()
	return p.do(t, http.MethodGet, path)
}

func (p *person) post(t *testing.T, path string) (int, string) {
	t.Helper()
	return p.do(t, http.MethodPost, path)
}

// Sign-up is implicit and happens on the first authenticated request. Nothing
// seeds a row: the suite's own stack proves it, because every other test in
// this package depends on it having worked.
func TestSignUpCreatesAPersonalOrganization(t *testing.T) {
	s := newStack(t, "cat")

	// The stack discovered its organization the same way a client does, so
	// reaching this far already means sign-up ran. What is worth asserting is
	// its shape.
	resp, err := s.hGet(s.baseURL + "/v1/orgs")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Orgs []orgDocT `json:"orgs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Orgs) != 1 {
		t.Fatalf("got %d organizations, want exactly one", len(out.Orgs))
	}
	org := out.Orgs[0]
	if !org.Personal {
		t.Error("the organization created at sign-up is not marked personal")
	}
	if org.Role != "owner" {
		t.Errorf("the signer is %q of their own organization, want owner", org.Role)
	}
	// Three words, which is what a person types and what a URL carries.
	if parts := strings.Split(org.Slug, "-"); len(parts) != 3 {
		t.Errorf("slug %q is not three words", org.Slug)
	}
	// Named after the slug until an ID token arrives with an email. Without an
	// identity provider it never does, so this is the resting state rather than
	// a transient one -- worth pinning so a future change does not leave it
	// blank instead.
	if org.Name != org.Slug {
		t.Errorf("name %q, want the slug %q until an email arrives", org.Name, org.Slug)
	}
}

// THE isolation test at the HTTP layer. The store tests prove the database
// refuses; this proves nothing above it hands out a way around.
func TestAnotherOrganizationIsNotReachable(t *testing.T) {
	s := newStack(t, "cat")
	other := s.otherUser(t)

	// A workspace exists in the first user's organization.
	s.newSession("w1")

	// The second user sees only their own, and it is not the first's.
	orgs := other.orgs(t)
	if len(orgs) != 1 || orgs[0].Slug == s.org {
		t.Fatalf("the second user sees %+v", orgs)
	}

	// Naming the first user's organization gets 404 rather than 403: whether an
	// organization exists is not a stranger's business, and a slug travels in
	// URLs and chat messages.
	code, body := other.get(t, "/v1/orgs/"+s.org+"/workspaces")
	if code != http.StatusNotFound {
		t.Errorf("listing another organization's workspaces got %d %q, want 404", code, body)
	}
	code, _ = other.get(t, "/v1/orgs/"+s.org+"/workspaces/w1/sessions")
	if code != http.StatusNotFound {
		t.Errorf("listing another organization's sessions got %d, want 404", code)
	}
	code, _ = other.post(t, "/v1/orgs/"+s.org+"/workspaces/w1/sessions")
	if code != http.StatusNotFound {
		t.Errorf("creating a session in another organization got %d, want 404", code)
	}

	// And an organization that does not exist at all is indistinguishable from
	// one they are simply not in -- which is the point of using 404 for both.
	code, _ = other.get(t, "/v1/orgs/ninety-cobalt-nonesuch/workspaces")
	if code != http.StatusNotFound {
		t.Errorf("a nonexistent organization got %d, want 404", code)
	}
}

// An invite is the only way across, and redeeming it adds a membership rather
// than moving anybody. Everyone keeps their own organization, which is what
// removes the ordering problem a one-organization-per-user model would have had.
func TestAnInviteAddsAMembershipWithoutMovingAnyone(t *testing.T) {
	s := newStack(t, "cat")
	other := s.otherUser(t)
	ownOrg := other.org

	s.newSession("shared-ws")

	// Owner-only, and the first user is the owner of their own organization.
	code, body := s.post("/invites")
	if code != http.StatusCreated {
		t.Fatalf("creating an invite got %d: %s", code, body)
	}
	var invite struct {
		Code string `json:"code"`
		Org  string `json:"org"`
		Role string `json:"role"`
	}
	if err := json.Unmarshal([]byte(body), &invite); err != nil {
		t.Fatalf("invite response: %v (%s)", err, body)
	}
	if invite.Org != s.org {
		t.Errorf("invite names organization %q, want %q", invite.Org, s.org)
	}
	if invite.Role != "user" {
		t.Errorf("the default invite role is %q, want user", invite.Role)
	}
	// The code is a bearer credential for membership, so it must not be
	// guessable from anything the caller already knows.
	if len(invite.Code) < 24 {
		t.Errorf("invite code %q is short enough to guess", invite.Code)
	}

	code, body = other.post(t, "/v1/invites/"+invite.Code+"/redeem")
	if code != http.StatusOK {
		t.Fatalf("redeeming got %d: %s", code, body)
	}

	// Two organizations now, and the personal one is still theirs.
	orgs := other.orgs(t)
	if len(orgs) != 2 {
		t.Fatalf("after redeeming, the second user is in %d organizations: %+v", len(orgs), orgs)
	}
	byslug := map[string]orgDocT{}
	for _, o := range orgs {
		byslug[o.Slug] = o
	}
	if own, ok := byslug[ownOrg]; !ok || !own.Personal || own.Role != "owner" {
		t.Errorf("the redeemer lost or changed their own organization: %+v", byslug[ownOrg])
	}
	joined, ok := byslug[s.org]
	if !ok {
		t.Fatalf("the invited organization is missing: %+v", orgs)
	}
	if joined.Role != "user" {
		t.Errorf("joined as %q, want the role the invite granted", joined.Role)
	}

	// The membership is real: what was 404 a moment ago now answers.
	code, body = other.get(t, "/v1/orgs/"+s.org+"/workspaces")
	if code != http.StatusOK {
		t.Fatalf("listing the joined organization got %d: %s", code, body)
	}

	// But it is EMPTY, and that is the rule rather than a gap. Belonging to an
	// organization is not the same as being in its workspaces: a plain member
	// sees what they own or were added to, which is why listWorkspaces splits on
	// the role at all. Joining a company should not hand someone every
	// colleague's sandbox.
	if strings.Contains(body, "shared-ws") {
		t.Errorf("a plain organization member can see a workspace they were never "+
			"added to: %s", body)
	}
}

// An owner sees everything in the organization, which is what makes the split in
// listWorkspaces worth having two queries for. Without this the previous test
// would also pass against a listing that is simply broken.
func TestAnOwnerSeesEveryWorkspaceInTheOrganization(t *testing.T) {
	s := newStack(t, "cat")
	other := s.otherUser(t)

	s.newSession("shared-ws")

	code, body := s.postJSON("/invites", `{"role":"owner"}`)
	if code != http.StatusCreated {
		t.Fatalf("creating an owner invite got %d: %s", code, body)
	}
	var invite struct {
		Code string `json:"code"`
		Role string `json:"role"`
	}
	if err := json.Unmarshal([]byte(body), &invite); err != nil {
		t.Fatalf("invite response: %v (%s)", err, body)
	}
	if invite.Role != "owner" {
		t.Fatalf("asked for an owner invite, got %q", invite.Role)
	}
	if code, b := other.post(t, "/v1/invites/"+invite.Code+"/redeem"); code != http.StatusOK {
		t.Fatalf("redeeming got %d: %s", code, b)
	}

	code, body = other.get(t, "/v1/orgs/"+s.org+"/workspaces")
	if code != http.StatusOK {
		t.Fatalf("listing as an owner got %d: %s", code, body)
	}
	if !strings.Contains(body, "shared-ws") {
		t.Errorf("an organization owner cannot see a workspace in it: %s", body)
	}
}

// A plain member is not an owner. Being able to see an organization must not
// carry the right to invite more people into it.
func TestAPlainMemberCannotInvite(t *testing.T) {
	s := newStack(t, "cat")
	other := s.otherUser(t)

	_, body := s.post("/invites")
	var invite struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(body), &invite); err != nil {
		t.Fatalf("invite response: %v (%s)", err, body)
	}
	if code, b := other.post(t, "/v1/invites/"+invite.Code+"/redeem"); code != http.StatusOK {
		t.Fatalf("redeeming got %d: %s", code, b)
	}

	code, b := other.post(t, "/v1/orgs/"+s.org+"/invites")
	if code != http.StatusForbidden {
		t.Errorf("a plain member issued an invite: %d %s", code, b)
	}
}

// Expired, exhausted and never-existed are deliberately one answer: telling
// them apart turns a code into an oracle for which codes exist.
func TestABadInviteCodeSaysNothingUseful(t *testing.T) {
	s := newStack(t, "cat")
	other := s.otherUser(t)

	code, body := other.post(t, "/v1/invites/not-a-real-code/redeem")
	if code != http.StatusNotFound {
		t.Fatalf("an unknown code got %d: %s", code, body)
	}
	for _, leak := range []string{"expired", "exhausted", "uses"} {
		if strings.Contains(strings.ToLower(body), leak) {
			t.Errorf("the refusal distinguishes failure modes (%q): %s", leak, body)
		}
	}
}

// A runner proves which organization it belongs to. The shared token it
// replaces let any holder register as anyone and receive their placements.
func TestARunnerCannotRegisterAsAnotherOrganization(t *testing.T) {
	s := newStack(t, "cat")
	other := s.otherUser(t)

	// The second organization's credential, presented against the first's.
	stolen := s.cp.RunnerToken(other.orgID)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet,
		s.baseURL+"/v1/tunnel/runner?tenant="+s.orgID, nil)
	req.Header.Set("Authorization", "Bearer "+stolen)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a runner registered for another organization: %d", resp.StatusCode)
	}

	// The real credential still works -- otherwise this would pass for the
	// wrong reason.
	if s.cp.RunnerCount(s.orgID) == 0 {
		t.Error("the suite's own runner is not registered, so the check above proves nothing")
	}
}
