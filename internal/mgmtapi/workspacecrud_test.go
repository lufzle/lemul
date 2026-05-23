package mgmtapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Workspace CRUD, and the guard rails that are not obvious from the handlers.

// `name` is required as a PROPERTY and explicit null means "generate one".
//
// Absent and null are different answers -- "you forgot" versus "you pick" --
// and a *string cannot tell them apart, so this is the only thing keeping the
// distinction real. Treating absent as "generate" would mean a client with a
// typo'd field name silently gets a random workspace instead of the one it
// asked for.
func TestAbsentAndNullNamesAreDifferent(t *testing.T) {
	h, iss := authedServer(t)
	tok, org := signUp(t, h, iss, "alice")
	path := "/v1/orgs/" + org + "/workspaces"

	if code, b := do(t, h, http.MethodPost, path, tok, `{}`); code != http.StatusBadRequest {
		t.Errorf("an absent name got %d, want 400: %s", code, b)
	}

	code, b := do(t, h, http.MethodPost, path, tok, `{"name":null}`)
	if code != http.StatusAccepted {
		t.Fatalf("an explicit null name got %d, want a generated workspace: %s", code, b)
	}
	var doc workspaceDoc
	if err := json.Unmarshal([]byte(b), &doc); err != nil {
		t.Fatalf("workspace: %v (%s)", err, b)
	}
	// Two words: an adjective and a vehicle. Three would be an organization
	// slug, and the two share a URL.
	if parts := strings.Split(doc.ID, "-"); len(parts) != 2 {
		t.Errorf("generated name %q is not two words", doc.ID)
	}
}

// THE id-vs-name trap, as a test.
//
// Everything outside the database is keyed on the NAME -- the tunnel registry,
// the supervisor's -workspace argv, the preflight store and the derived
// workspace credential. Renaming under a running task orphans its tunnel set
// and invalidates the credential it is holding, so its next dial is answered
// 401 and it exits by design, taking its sessions with it. Nothing would report
// that as a rename problem.
func TestRenameIsRefusedWhileATaskIsLive(t *testing.T) {
	srv, h, iss := authedServerFull(t)
	tok, org := signUp(t, h, iss, "alice")
	ws := createWS(t, h, tok, org, `{"name":"api"}`)
	path := "/v1/orgs/" + org + "/workspaces/" + ws

	// Renaming a stopped workspace is fine, and doing it first means the
	// refusal below cannot pass because renaming is broken outright.
	if code, b := do(t, h, http.MethodPatch, path, tok, `{"name":"api-two"}`); code != http.StatusOK {
		t.Fatalf("renaming a stopped workspace: %d %s", code, b)
	}
	path = "/v1/orgs/" + org + "/workspaces/api-two"

	// Now a task holding a tunnel, which is what makes the name load-bearing.
	srv.reg.AddWorkspace(fakeTunnel(t, srv.OrgIDForSlug(org), "api-two"))

	code, b := do(t, h, http.MethodPatch, path, tok, `{"name":"api-three"}`)
	if code != http.StatusConflict {
		t.Fatalf("renamed a workspace with a live task: %d %s", code, b)
	}
	if !strings.Contains(b, "name") {
		t.Errorf("the refusal does not say why the name matters: %s", b)
	}
}

// A workspace can only hold its own organization's users.
//
// Load-bearing rather than tidy: workspace_member's WITH CHECK is scoped to the
// tenant and nothing else, so without this check any uuid at all -- including a
// user belonging to another organization -- could be written in, and the row
// would then satisfy every policy that reads it.
func TestAUserFromAnotherOrganizationCannotBeAddedToAWorkspace(t *testing.T) {
	h, iss := authedServer(t)
	alice, aliceOrg := signUp(t, h, iss, "alice")
	bob, bobOrg := signUp(t, h, iss, "bob")

	ws := createWS(t, h, alice, aliceOrg, `{"name":"alices-work"}`)
	// Bob's own user id, learned from his own organization -- he is a stranger
	// to alice's.
	stranger := selfID(t, h, bob, bobOrg)

	code, b := do(t, h, http.MethodPut,
		"/v1/orgs/"+aliceOrg+"/workspaces/"+ws+"/members/"+stranger, alice, `{"role":"user"}`)
	if code != http.StatusNotFound {
		t.Fatalf("a user outside the organization was added: %d %s", code, b)
	}
}

// Removing the workspace's own owner would leave the record owned by somebody
// who can no longer reach it -- and, if they were the last owner, nobody who
// can undo it.
func TestTheWorkspaceOwnerCannotBeRemoved(t *testing.T) {
	h, iss := authedServer(t)
	alice, aliceOrg := signUp(t, h, iss, "alice")
	ws := createWS(t, h, alice, aliceOrg, `{"name":"alices-work"}`)
	me := selfID(t, h, alice, aliceOrg)

	code, b := do(t, h, http.MethodDelete,
		"/v1/orgs/"+aliceOrg+"/workspaces/"+ws+"/members/"+me, alice, "")
	if code != http.StatusConflict {
		t.Fatalf("the workspace owner was removed from it: %d %s", code, b)
	}
}

// selfID returns the caller's own user id, from the row the members listing
// marks as theirs.
func selfID(t *testing.T, h http.Handler, tok, org string) string {
	t.Helper()
	code, b := do(t, h, http.MethodGet, "/v1/orgs/"+org+"/members", tok, "")
	if code != http.StatusOK {
		t.Fatalf("listing members: %d %s", code, b)
	}
	var out memberListResponse
	if err := json.Unmarshal([]byte(b), &out); err != nil {
		t.Fatalf("members: %v (%s)", err, b)
	}
	for _, m := range out.Members {
		if m.Me {
			return m.UserID
		}
	}
	t.Fatalf("no row marked as the caller's: %s", b)
	return ""
}

// A name beginning with - is unaddressable by the client that mints it.
//
// Not hypothetical: `lem workspace create --help` created a workspace called
// --help on the deployed control plane on 2026-08-03, because flag parsing
// stops at the verb and the rune loop in validWorkspaceName saw only - and
// lowercase letters. Every `lem workspace rm` afterwards read the name as a
// flag, so the record was reachable only by uuid and only over HTTP.
//
// The rune loop still has to allow - INSIDE a name, since every generated name
// is two hyphenated words -- so a guard that merely banned the character would
// reject the server's own output.
func TestAWorkspaceNameCannotBeginWithADash(t *testing.T) {
	h, iss := authedServer(t)
	tok, org := signUp(t, h, iss, "alice")
	path := "/v1/orgs/" + org + "/workspaces"

	for _, name := range []string{"--help", "-h", "-workspace"} {
		code, b := do(t, h, http.MethodPost, path, tok, `{"name":"`+name+`"}`)
		if code != http.StatusBadRequest {
			t.Errorf("creating %q got %d, want 400: %s", name, code, b)
		}
	}

	// The character itself stays legal, or generated names would be refused.
	if code, b := do(t, h, http.MethodPost, path, tok, `{"name":"amber-lorry"}`); code != http.StatusAccepted {
		t.Errorf("a hyphenated name got %d, want 202: %s", code, b)
	}
}
