package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The workspace default, which replaced "your personal workspace, created on
// demand".
//
// That old default had a bare `lem` conjure a Fargate task out of a command
// with no arguments -- which stopped being defensible once a workspace is a
// shared machine somebody pays for, and once an organization can say who may
// create one. So the rule is now the same one currentOrg uses, and these pin it
// for the same reason: this is the whole of the guessing, and a wrong guess
// lands somebody in a workspace their colleagues are sharing.

// fakeWorkspaces serves the org listing and the workspace listing, and resets
// the memoised organization so a second test in this binary does not inherit it.
func fakeWorkspaces(t *testing.T, body string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/orgs", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"orgs":[{"slug":"forty-crimson-windmill","name":"D","role":"owner","personal":true}]}`)
	})
	mux.HandleFunc("GET /v1/orgs/{org}/workspaces", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	url := srv.URL
	server = &url
	none := ""
	org = &none
	orgOnce = sync.Once{}
	orgSlug, orgErr = "", nil
}

func TestOneWorkspaceNeedsNoFlag(t *testing.T) {
	fakeWorkspaces(t, `{"workspaces":[
		{"id":"team-box","status":"stopped","access_scope":"org"}
	]}`)

	got, err := currentWorkspace()
	if err != nil {
		t.Fatalf("currentWorkspace: %v", err)
	}
	if got != "team-box" {
		t.Errorf("resolved %q", got)
	}
}

// The refusal, and the reason the rule exists at all. On a shared machine the
// cost of guessing is not a wasted keystroke -- it is starting work in a
// workspace other people are in.
func TestSeveralWorkspacesRequireTheFlag(t *testing.T) {
	fakeWorkspaces(t, `{"workspaces":[
		{"id":"team-box","status":"active","access_scope":"org"},
		{"id":"my-box","status":"stopped","access_scope":"owner"}
	]}`)

	_, err := currentWorkspace()
	if err == nil {
		t.Fatal("resolved a workspace while the caller could reach two")
	}
	// Actionable or it is not a refusal: somebody who does not know the names
	// cannot act on "--workspace is required" alone.
	for _, want := range []string{"--workspace", "team-box", "my-box"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// None is its own answer, and it must name the verb.
//
// This is the case the personal workspace used to paper over by creating one.
// Nothing creates a workspace implicitly any more, so the only way out of here
// is a command -- and a user who is not told it has nowhere to go.
func TestNoWorkspacesNamesTheVerbThatMakesOne(t *testing.T) {
	fakeWorkspaces(t, `{"workspaces":[]}`)

	_, err := currentWorkspace()
	if err == nil {
		t.Fatal("resolved a workspace when the caller had none")
	}
	if !strings.Contains(err.Error(), "lem workspace create") {
		t.Errorf("the refusal does not name the verb that creates one: %v", err)
	}
}

// A bare `lem` must not create anything. The old default issued a POST; this
// one is a read, and the difference is a Fargate task nobody asked for.
func TestResolvingAWorkspaceNeverCreatesOne(t *testing.T) {
	var methods []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/orgs", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"orgs":[{"slug":"forty-crimson-windmill","name":"D","role":"owner","personal":true}]}`)
	})
	mux.HandleFunc("/v1/orgs/{org}/workspaces", func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		fmt.Fprint(w, `{"workspaces":[{"id":"only-one","status":"stopped","access_scope":"owner"}]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	url := srv.URL
	server = &url
	none := ""
	org = &none
	orgOnce = sync.Once{}
	orgSlug, orgErr = "", nil

	if _, err := currentWorkspace(); err != nil {
		t.Fatalf("currentWorkspace: %v", err)
	}
	for _, m := range methods {
		if m != http.MethodGet {
			t.Errorf("resolving a workspace issued a %s; it must only read", m)
		}
	}
}
