package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeControlPlane serves a canned session list and records whether a session
// was created.
func fakeControlPlane(t *testing.T, listBody string) (created *bool) {
	t.Helper()
	flag := false
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/workspaces/{wid}/sessions", func(w http.ResponseWriter, r *http.Request) {
		if listBody == "" {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, listBody)
	})
	mux.HandleFunc("POST /v1/workspaces/{wid}/sessions", func(w http.ResponseWriter, r *http.Request) {
		flag = true
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"id":"s-brandnew","workspace_id":"w1","status":"created"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	url := srv.URL
	server = &url
	return &flag
}

// The problem this solves: losing a session id used to strand the session, since
// nothing could find it again.
func TestPickSessionReattachesToAnIdleSession(t *testing.T) {
	created := fakeControlPlane(t, `{"sessions":[
		{"id":"s-newest","status":"running","attachers":0,"created_at":"2026-07-29T10:00:02Z"},
		{"id":"s-older","status":"running","attachers":0,"created_at":"2026-07-29T10:00:01Z"}
	]}`)

	sid, reattached, err := pickSession("w1")
	if err != nil {
		t.Fatalf("pickSession: %v", err)
	}
	if !reattached || sid != "s-newest" {
		t.Errorf("got (%q, reattached=%v), want (s-newest, true)", sid, reattached)
	}
	if *created {
		t.Error("created a session when an idle one was available")
	}
}

// Both attachers write the same PTY stdin, so joining an in-use session by
// default would silently make two people co-drive one Claude Code. Opening a
// second terminal must give a second session instead.
func TestPickSessionDoesNotJoinAnAttachedSession(t *testing.T) {
	created := fakeControlPlane(t, `{"sessions":[
		{"id":"s-busy","status":"running","attachers":1,"created_at":"2026-07-29T10:00:00Z"}
	]}`)

	sid, reattached, err := pickSession("w1")
	if err != nil {
		t.Fatalf("pickSession: %v", err)
	}
	if reattached {
		t.Errorf("joined a session that already had a client: %q", sid)
	}
	if !*created || sid != "s-brandnew" {
		t.Errorf("got %q, want a newly created session", sid)
	}
}

// A session record outlives its process, so a stopped one is not something to
// attach to -- resuming it is the lifecycle work, not this.
func TestPickSessionIgnoresStoppedSessions(t *testing.T) {
	created := fakeControlPlane(t, `{"sessions":[
		{"id":"s-dead","status":"stopped","attachers":0,"created_at":"2026-07-29T10:00:00Z"}
	]}`)

	sid, reattached, err := pickSession("w1")
	if err != nil {
		t.Fatalf("pickSession: %v", err)
	}
	if reattached {
		t.Errorf("reattached to a stopped session: %q", sid)
	}
	if !*created {
		t.Error("did not create a session")
	}
}

func TestPickSessionCreatesWhenWorkspaceIsEmpty(t *testing.T) {
	created := fakeControlPlane(t, `{"sessions":[]}`)
	sid, reattached, err := pickSession("w1")
	if err != nil {
		t.Fatalf("pickSession: %v", err)
	}
	if reattached || sid != "s-brandnew" {
		t.Errorf("got (%q, %v), want a new session", sid, reattached)
	}
	if !*created {
		t.Error("did not create a session")
	}
}

// Listing is a convenience. A control plane that cannot list must still be able
// to start a session, or a cosmetic failure becomes a hard one.
func TestPickSessionFallsBackToCreateWhenListingFails(t *testing.T) {
	created := fakeControlPlane(t, "")
	sid, reattached, err := pickSession("w1")
	if err != nil {
		t.Fatalf("pickSession returned an error instead of creating: %v", err)
	}
	if reattached || sid != "s-brandnew" {
		t.Errorf("got (%q, %v), want a new session", sid, reattached)
	}
	if !*created {
		t.Error("did not fall back to creating a session")
	}
}

func TestListSessionsParsesTheResponse(t *testing.T) {
	fakeControlPlane(t, `{"sessions":[
		{"id":"s-1","status":"running","attachers":2,"rows":40,"cols":100,"created_at":"2026-07-29T10:00:00Z"}
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
}

func TestListSessionsReportsHTTPErrors(t *testing.T) {
	fakeControlPlane(t, "")
	_, err := listSessions("w1")
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %v, want an HTTP 500", err)
	}
}
