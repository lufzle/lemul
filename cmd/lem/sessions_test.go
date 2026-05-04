package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeControlPlane serves a canned session list and records whether a session
// was created.
//
// It answers the ORGANIZATION-wide listing as well as the per-workspace one,
// because `lem --session <id>` resolves a prefix without being told a
// workspace -- the endpoint that acts on a session is org-scoped, so demanding
// one purely to look an id up would ask for something the API does not need.
func fakeControlPlane(t *testing.T, listBody string) (created *bool) {
	t.Helper()
	flag := false
	list := func(w http.ResponseWriter, _ *http.Request) {
		if listBody == "" {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, listBody)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/orgs/{org}/sessions", list)
	mux.HandleFunc("GET /v1/orgs/{org}/workspaces/{wid}/sessions", list)
	mux.HandleFunc("POST /v1/orgs/{org}/workspaces/{wid}/sessions", func(w http.ResponseWriter, _ *http.Request) {
		flag = true
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"id":"s-brandnew","workspace_id":"w1","status":"created"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	url := srv.URL
	server = &url
	// Set explicitly rather than discovered, so these cases test session
	// selection rather than the organization default. currentOrg memoises, and
	// a sync.Once shared across tests in one binary would make whichever ran
	// first decide for all of them.
	slug := testOrg
	org = &slug
	orgOnce = sync.Once{}
	orgSlug, orgErr = "", nil

	empty := ""
	workspace = &empty
	return &flag
}

const testOrg = "one-amber-windmill"

// `lem` ALWAYS starts a new session, which is the change from Phase 1's
// reattach-by-default.
//
// Two reasons it had to go, and the second is the one that matters. Opening a
// second terminal should give a second session -- that is what the shared
// filesystem is for. And "the newest idle session in this workspace" is not
// necessarily YOURS: a workspace is shared, a session is not (section 2.5), so
// the old default could drop somebody into a colleague's conversation with a
// keyboard, chosen for them by a client.
func TestBareLemAlwaysCreatesANewSession(t *testing.T) {
	created := fakeControlPlane(t, `{"sessions":[
		{"id":"s-idle","workspace":"w1","status":"running","attachers":0,"mine":true,
		 "created_at":"2026-07-29T10:00:02Z"}
	]}`)

	sid, err := createSession("w1")
	if err != nil {
		t.Fatalf("createSession: %v", err)
	}
	if !*created || sid != "s-brandnew" {
		t.Errorf("got %q, want a newly created session even with an idle one present", sid)
	}
}

// The workspace has to exist first: implicit creation is gone, so a typo can no
// longer manufacture one. The failure must name the verb that would.
func TestCreatingASessionInAnUnknownWorkspaceNamesTheCreateVerb(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/orgs/{org}/workspaces/{wid}/sessions", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no such workspace", http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	url := srv.URL
	server = &url
	slug := testOrg
	org = &slug
	orgOnce = sync.Once{}
	orgSlug, orgErr = "", nil

	_, err := createSession("typo")
	if err == nil {
		t.Fatal("an unknown workspace did not fail")
	}
	if !strings.Contains(err.Error(), "workspace create") {
		t.Errorf("the failure does not name the way to create one: %v", err)
	}
}

func TestListSessionsParsesTheResponse(t *testing.T) {
	fakeControlPlane(t, `{"sessions":[
		{"id":"s-1","workspace":"w1","status":"running","attachers":2,"rows":40,"cols":100,
		 "mine":true,"created_at":"2026-07-29T10:00:00Z"}
	]}`)
	got, err := listSessions("w1")
	if err != nil {
		t.Fatalf("listSessions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d sessions, want 1", len(got))
	}
	s := got[0]
	if s.ID != "s-1" || s.Status != "running" || s.Attachers != 2 || s.Rows != 40 || s.Cols != 100 {
		t.Errorf("decoded wrongly: %+v", s)
	}
	if !s.Mine || s.Workspace != "w1" {
		t.Errorf("mine/workspace decoded wrongly: %+v", s)
	}
}

func TestListSessionsReportsHTTPErrors(t *testing.T) {
	fakeControlPlane(t, "")
	_, err := listSessions("w1")
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %v, want an HTTP 500", err)
	}
}

// Session ids are 36-character UUIDs (§12.7), so a prefix is the difference
// between retyping an id and not.
func TestResolveSessionAcceptsAUniquePrefix(t *testing.T) {
	fakeControlPlane(t, `{"sessions":[
		{"id":"522f6d19-b559-477f-88ae-f53322baeca3","workspace":"w1","status":"running"},
		{"id":"05ccae56-b83b-4a6f-ad66-469465258e4c","workspace":"w2","status":"stopped"}
	]}`)

	got, ws, err := resolveSession("522f6d19")
	if err != nil {
		t.Fatalf("resolveSession: %v", err)
	}
	if got != "522f6d19-b559-477f-88ae-f53322baeca3" {
		t.Errorf("got %q", got)
	}
	// The workspace comes back with it, because the caller needs it for the
	// detach message and never had to type it.
	if ws != "w1" {
		t.Errorf("workspace = %q, want w1", ws)
	}
}

// A stopped session resolves exactly like a running one. Attaching to it
// RESUMES it -- the supervisor picks --resume from the transcript on the
// workspace volume -- so filtering stopped sessions out here would hide the
// only way back to a conversation.
func TestResolveSessionFindsAStoppedSession(t *testing.T) {
	fakeControlPlane(t, `{"sessions":[
		{"id":"05ccae56-b83b-4a6f-ad66-469465258e4c","workspace":"w2","status":"stopped"}
	]}`)

	got, ws, err := resolveSession("05ccae56")
	if err != nil {
		t.Fatalf("a stopped session could not be resolved: %v", err)
	}
	if got != "05ccae56-b83b-4a6f-ad66-469465258e4c" || ws != "w2" {
		t.Errorf("got (%q, %q)", got, ws)
	}
}

// Picking one would at best waste the user's time and at worst attach them to
// the wrong agent run, so ambiguity is an error that names the candidates.
func TestResolveSessionRefusesAnAmbiguousPrefix(t *testing.T) {
	fakeControlPlane(t, `{"sessions":[
		{"id":"aa11-one","workspace":"w1","status":"running"},
		{"id":"aa11-two","workspace":"w2","status":"running"}
	]}`)

	_, _, err := resolveSession("aa11")
	if err == nil {
		t.Fatal("no error for a prefix matching two sessions")
	}
	// Naming the workspace beside each is what makes the list actionable:
	// two ids differing at character five are not something a person can tell
	// apart, and the workspace usually is.
	for _, want := range []string{"aa11-one", "aa11-two", "w1", "w2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q: %v", want, err)
		}
	}
}

// An id that is also a prefix of a longer one must resolve to itself rather
// than becoming ambiguous.
func TestResolveSessionPrefersAnExactMatch(t *testing.T) {
	fakeControlPlane(t, `{"sessions":[
		{"id":"abc","workspace":"w1","status":"running"},
		{"id":"abcdef","workspace":"w1","status":"running"}
	]}`)

	got, _, err := resolveSession("abc")
	if err != nil {
		t.Fatalf("resolveSession: %v", err)
	}
	if got != "abc" {
		t.Errorf("got %q, want the exact match", got)
	}
}

func TestResolveSessionReportsNoMatch(t *testing.T) {
	fakeControlPlane(t, `{"sessions":[{"id":"s-1","workspace":"w1","status":"running"}]}`)
	if _, _, err := resolveSession("nope"); err == nil {
		t.Fatal("no error for a prefix matching nothing")
	}
}

// Listing failing is now fatal to resolution rather than falling back to using
// the string verbatim.
//
// The fallback made sense while a session id was all `--session` took. It stops
// making sense now that the workspace comes back with it: attaching needs a
// name for the detach message, and inventing one would print a command that
// does not work. A listing failure is also the thing most likely to mean the
// caller cannot see that session at all, which is not a case to paper over.
func TestResolveSessionFailsWhenListingFails(t *testing.T) {
	fakeControlPlane(t, "")
	if _, _, err := resolveSession("s-verbatim"); err == nil {
		t.Fatal("resolution succeeded against a control plane that could not list")
	}
}
