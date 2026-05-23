package mgmtapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Phase 6: a workspace is a shared machine, so who may USE one became its own
// property rather than following from a membership row.
//
// The pairing to keep in mind while reading these: access_scope answers "may I
// start a session here", workspace_member.role answers "may I administer this".
// A workspace open to the whole organization still has exactly one owner.

// listWorkspaceNames returns what a caller can see, through the real endpoint.
func listWorkspaceNames(t *testing.T, h http.Handler, tok, org string) []string {
	t.Helper()
	code, b := do(t, h, http.MethodGet, "/v1/orgs/"+org+"/workspaces", tok, "")
	if code != http.StatusOK {
		t.Fatalf("listing workspaces: %d %s", code, b)
	}
	var out workspaceListResponse
	if err := json.Unmarshal([]byte(b), &out); err != nil {
		t.Fatalf("workspace list: %v (%s)", err, b)
	}
	names := make([]string, 0, len(out.Workspaces))
	for _, ws := range out.Workspaces {
		names = append(names, ws.ID)
	}
	return names
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

// The property the whole increment exists for: an org-scoped workspace admits a
// colleague nobody added to it.
//
// Checked at BOTH doors deliberately. Reaching it and listing it are different
// code paths -- accessTo and ListWorkspacesForUser -- and a workspace you can
// use but cannot see is as broken as one you can see but cannot use.
func TestAnOrgScopedWorkspaceAdmitsAnyMemberOfTheOrganization(t *testing.T) {
	h, iss := authedServer(t)
	alice, org := signUp(t, h, iss, "alice")
	bob, _ := signUp(t, h, iss, "bob")
	invite(t, h, alice, org, bob, "user")

	ws := createWS(t, h, alice, org, `{"name":"team-box","access_scope":"org"}`)

	code, b := do(t, h, http.MethodGet, "/v1/orgs/"+org+"/workspaces/"+ws, bob, "")
	if code != http.StatusOK {
		t.Fatalf("bob reaching an org-scoped workspace: %d %s, want 200", code, b)
	}
	if names := listWorkspaceNames(t, h, bob, org); !contains(names, ws) {
		t.Errorf("bob can use %s but does not see it in his listing: %v", ws, names)
	}
}

// The control for the test above. Without it, "org scope works" would also pass
// if every workspace had quietly become visible to everyone.
func TestAMembersScopedWorkspaceStillRefusesANonMember(t *testing.T) {
	h, iss := authedServer(t)
	alice, org := signUp(t, h, iss, "alice")
	bob, _ := signUp(t, h, iss, "bob")
	invite(t, h, alice, org, bob, "user")

	ws := createWS(t, h, alice, org, `{"name":"secret-box","access_scope":"members"}`)

	code, b := do(t, h, http.MethodGet, "/v1/orgs/"+org+"/workspaces/"+ws, bob, "")
	if code != http.StatusNotFound {
		t.Errorf("bob reaching a members-scoped workspace: %d %s, want 404", code, b)
	}
	if names := listWorkspaceNames(t, h, bob, org); contains(names, ws) {
		t.Errorf("bob sees a workspace he is not a member of: %v", names)
	}
}

// Omitting the field must give the CLOSED answer.
//
// The direction matters more than the value: a client that predates this field
// sends no access_scope, and if absent meant "org" every workspace created by
// an old client would silently be readable by the whole organization.
func TestAWorkspaceCreatedWithoutAnAccessScopeIsPrivate(t *testing.T) {
	h, iss := authedServer(t)
	alice, org := signUp(t, h, iss, "alice")
	bob, _ := signUp(t, h, iss, "bob")
	invite(t, h, alice, org, bob, "user")

	ws := createWS(t, h, alice, org, `{"name":"unspecified"}`)

	code, b := do(t, h, http.MethodGet, "/v1/orgs/"+org+"/workspaces/"+ws, alice, "")
	if code != http.StatusOK {
		t.Fatalf("reading it back: %d %s", code, b)
	}
	var doc workspaceDoc
	if err := json.Unmarshal([]byte(b), &doc); err != nil {
		t.Fatalf("workspace: %v (%s)", err, b)
	}
	if doc.AccessScope != accessOwner {
		t.Errorf("access_scope defaulted to %q, want %q", doc.AccessScope, accessOwner)
	}
	if code, _ := do(t, h, http.MethodGet, "/v1/orgs/"+org+"/workspaces/"+ws, bob, ""); code != http.StatusNotFound {
		t.Errorf("a workspace created with no access_scope admitted a non-member: %d", code)
	}
}

// A bad value is a sentence, not a constraint violation surfacing as a 500.
func TestAnUnknownAccessScopeIsRefused(t *testing.T) {
	h, iss := authedServer(t)
	alice, org := signUp(t, h, iss, "alice")

	code, b := do(t, h, http.MethodPost, "/v1/orgs/"+org+"/workspaces", alice,
		`{"name":"weird","access_scope":"everyone"}`)
	if code != http.StatusBadRequest {
		t.Errorf("an unknown access_scope got %d %s, want 400", code, b)
	}
}

// Widening after the fact reaches the same place as creating it wide, and
// narrowing takes it back. Both directions, because a rule that only ever opens
// is not a rule.
func TestChangingTheAccessScopeChangesWhoCanReachIt(t *testing.T) {
	h, iss := authedServer(t)
	alice, org := signUp(t, h, iss, "alice")
	bob, _ := signUp(t, h, iss, "bob")
	invite(t, h, alice, org, bob, "user")

	ws := createWS(t, h, alice, org, `{"name":"movable"}`)
	path := "/v1/orgs/" + org + "/workspaces/" + ws

	if code, b := do(t, h, http.MethodGet, path, bob, ""); code != http.StatusNotFound {
		t.Fatalf("bob before widening: %d %s, want 404", code, b)
	}
	if code, b := do(t, h, http.MethodPatch, path, alice, `{"access_scope":"org"}`); code != http.StatusOK {
		t.Fatalf("widening: %d %s", code, b)
	}
	if code, b := do(t, h, http.MethodGet, path, bob, ""); code != http.StatusOK {
		t.Errorf("bob after widening: %d %s, want 200", code, b)
	}
	if code, b := do(t, h, http.MethodPatch, path, alice, `{"access_scope":"owner"}`); code != http.StatusOK {
		t.Fatalf("narrowing: %d %s", code, b)
	}
	if code, b := do(t, h, http.MethodGet, path, bob, ""); code != http.StatusNotFound {
		t.Errorf("bob after narrowing: %d %s, want 404", code, b)
	}
}

// A plain member may not rescope a workspace they can merely use.
//
// This is the pairing the increment turns on: access_scope says who may USE it,
// role says who may ADMINISTER it, and widening one must not follow from the
// other. Without this, the first member to be let in could let everybody else
// in too.
func TestAPlainMemberCannotRescopeAWorkspace(t *testing.T) {
	h, iss := authedServer(t)
	alice, org := signUp(t, h, iss, "alice")
	bob, _ := signUp(t, h, iss, "bob")
	invite(t, h, alice, org, bob, "user")

	ws := createWS(t, h, alice, org, `{"name":"team-box","access_scope":"org"}`)
	path := "/v1/orgs/" + org + "/workspaces/" + ws

	if code, b := do(t, h, http.MethodGet, path, bob, ""); code != http.StatusOK {
		t.Fatalf("bob can use it: %d %s", code, b)
	}
	code, b := do(t, h, http.MethodPatch, path, bob, `{"access_scope":"members"}`)
	if code != http.StatusForbidden {
		t.Errorf("a plain member rescoping got %d %s, want 403", code, b)
	}
}

// ---------------------------------------------------------------------------
// Who may create a workspace at all.
// ---------------------------------------------------------------------------

// Default closed: a workspace is a task somebody pays for from the moment it is
// placed, so members creating them is something an owner opts into.
func TestAMemberCannotCreateAWorkspaceByDefault(t *testing.T) {
	h, iss := authedServer(t)
	alice, org := signUp(t, h, iss, "alice")
	bob, _ := signUp(t, h, iss, "bob")
	invite(t, h, alice, org, bob, "user")

	code, b := do(t, h, http.MethodPost, "/v1/orgs/"+org+"/workspaces", bob, `{"name":"bobs-box"}`)
	if code != http.StatusForbidden {
		t.Fatalf("a member creating a workspace got %d %s, want 403", code, b)
	}
	// A dead end is worse than a refusal: the message has to name who can change
	// it, or the member's only move is to ask us.
	if !strings.Contains(b, "owner") {
		t.Errorf("the refusal does not say who can allow it: %s", b)
	}
	// The generated-name branch of the same handler. One gate has to cover every
	// way in, and `{"name":null}` reaches the create path by a different route
	// than a named workspace does.
	code, b = do(t, h, http.MethodPost, "/v1/orgs/"+org+"/workspaces", bob, `{"name":null}`)
	if code != http.StatusForbidden {
		t.Errorf("a member creating a generated-name workspace got %d %s, want 403", code, b)
	}
}

// And the owner is never gated by it -- an owner who could not create a
// workspace could not turn the setting on either.
func TestAnOwnerCreatesWorkspacesRegardlessOfTheSetting(t *testing.T) {
	h, iss := authedServer(t)
	alice, org := signUp(t, h, iss, "alice")

	if ws := createWS(t, h, alice, org, `{"name":"owners-box"}`); ws == "" {
		t.Fatal("the owner could not create a workspace under the default setting")
	}
}

func TestTurningTheSettingOnLetsMembersCreateWorkspaces(t *testing.T) {
	h, iss := authedServer(t)
	alice, org := signUp(t, h, iss, "alice")
	bob, _ := signUp(t, h, iss, "bob")
	invite(t, h, alice, org, bob, "user")

	code, b := do(t, h, http.MethodPatch, "/v1/orgs/"+org+"/settings", alice,
		`{"members_can_create_workspaces":true}`)
	if code != http.StatusOK {
		t.Fatalf("turning the setting on: %d %s", code, b)
	}
	if ws := createWS(t, h, bob, org, `{"name":"bobs-box"}`); ws == "" {
		t.Fatal("bob still could not create a workspace")
	}
}

// A member may read the setting -- they have to be able to find out why they
// were refused -- but only an owner may change it.
func TestOnlyAnOwnerChangesTheSettings(t *testing.T) {
	h, iss := authedServer(t)
	alice, org := signUp(t, h, iss, "alice")
	bob, _ := signUp(t, h, iss, "bob")
	invite(t, h, alice, org, bob, "user")

	if code, b := do(t, h, http.MethodGet, "/v1/orgs/"+org+"/settings", bob, ""); code != http.StatusOK {
		t.Errorf("a member reading the settings: %d %s, want 200", code, b)
	}
	code, b := do(t, h, http.MethodPatch, "/v1/orgs/"+org+"/settings", bob,
		`{"members_can_create_workspaces":true}`)
	if code != http.StatusForbidden {
		t.Errorf("a member changing the settings got %d %s, want 403", code, b)
	}
	// And it did not take effect anyway.
	if code, _ := do(t, h, http.MethodPost, "/v1/orgs/"+org+"/workspaces", bob,
		`{"name":"sneaky"}`); code != http.StatusForbidden {
		t.Errorf("the refused setting change took effect anyway: %d", code)
	}
}

// Settings are per organization, which is the one thing a single-tenant test
// can never show. Alice owns both, so the only difference is the organization.
func TestTheSettingIsPerOrganization(t *testing.T) {
	h, iss := authedServer(t)
	alice, orgA := signUp(t, h, iss, "alice")
	bob, orgB := signUp(t, h, iss, "bob")
	// Bob owns orgB and is a plain member of orgA; alice the reverse.
	invite(t, h, alice, orgA, bob, "user")
	invite(t, h, bob, orgB, alice, "user")

	if code, b := do(t, h, http.MethodPatch, "/v1/orgs/"+orgA+"/settings", alice,
		`{"members_can_create_workspaces":true}`); code != http.StatusOK {
		t.Fatalf("turning it on in orgA: %d %s", code, b)
	}

	// Bob creates in orgA, where it is on.
	if code, b := do(t, h, http.MethodPost, "/v1/orgs/"+orgA+"/workspaces", bob,
		`{"name":"in-a"}`); code != http.StatusAccepted {
		t.Errorf("bob in orgA (setting on) got %d %s, want 202", code, b)
	}
	// Alice cannot create in orgB, where it is untouched -- even though she just
	// turned it on in her own.
	if code, b := do(t, h, http.MethodPost, "/v1/orgs/"+orgB+"/workspaces", alice,
		`{"name":"in-b"}`); code != http.StatusForbidden {
		t.Errorf("alice in orgB (setting off) got %d %s, want 403", code, b)
	}
}

// PATCH omitting a field must leave it alone rather than reset it.
//
// A plain bool cannot tell a missing key from an explicit false, so without the
// pointer this passes today and silently turns the setting off the first time
// somebody PATCHes a different field.
func TestPatchingSettingsLeavesUnmentionedFieldsAlone(t *testing.T) {
	h, iss := authedServer(t)
	alice, org := signUp(t, h, iss, "alice")

	if code, b := do(t, h, http.MethodPatch, "/v1/orgs/"+org+"/settings", alice,
		`{"members_can_create_workspaces":true}`); code != http.StatusOK {
		t.Fatalf("turning it on: %d %s", code, b)
	}
	if code, b := do(t, h, http.MethodPatch, "/v1/orgs/"+org+"/settings", alice, `{}`); code != http.StatusOK {
		t.Fatalf("empty patch: %d %s", code, b)
	}
	code, b := do(t, h, http.MethodGet, "/v1/orgs/"+org+"/settings", alice, "")
	if code != http.StatusOK {
		t.Fatalf("reading back: %d %s", code, b)
	}
	var set orgSettings
	if err := json.Unmarshal([]byte(b), &set); err != nil {
		t.Fatalf("settings: %v (%s)", err, b)
	}
	if !set.MembersCanCreateWorkspaces {
		t.Error("an empty PATCH reset a setting it did not mention")
	}
}
