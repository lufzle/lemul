package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The organization default is the one piece of guessing this product does, so
// it is worth pinning exactly where it stops guessing.
//
// The server never picks one -- every organization-scoped route names it in the
// path -- which means these rules are the whole of the convenience, and getting
// them wrong is how `lem rm` ends up pointed at the wrong customer.

// fakeOrgs serves GET /v1/orgs and resets the memoised resolution, which is
// package state a second test in the same binary would otherwise inherit.
func fakeOrgs(t *testing.T, body string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/orgs", func(w http.ResponseWriter, _ *http.Request) {
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

func TestOneOrganizationNeedsNoFlag(t *testing.T) {
	fakeOrgs(t, `{"orgs":[
		{"slug":"forty-crimson-windmill","name":"Dario's Org","role":"owner","personal":true}
	]}`)

	got, err := currentOrg()
	if err != nil {
		t.Fatalf("currentOrg: %v", err)
	}
	if got != "forty-crimson-windmill" {
		t.Errorf("resolved %q", got)
	}
}

// The refusal, and the reason the whole mechanism exists. Someone in their own
// organization and a customer's must never have a destructive verb fall back to
// whichever a default happened to name.
func TestSeveralOrganizationsRequireTheFlag(t *testing.T) {
	fakeOrgs(t, `{"orgs":[
		{"slug":"forty-crimson-windmill","name":"Dario's Org","role":"owner","personal":true},
		{"slug":"nine-cobalt-mallard","name":"Acme","role":"user","personal":false}
	]}`)

	_, err := currentOrg()
	if err == nil {
		t.Fatal("resolved an organization while the caller belonged to two")
	}
	// The error has to be actionable: a user who does not know the slugs cannot
	// act on "--org is required" alone.
	for _, want := range []string{"--org", "forty-crimson-windmill", "nine-cobalt-mallard", "Acme"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// An explicit flag is authority, not a hint: it must not be second-guessed
// against the list, or acting in an organization would depend on a listing call
// succeeding.
func TestExplicitOrgIsUsedWithoutListing(t *testing.T) {
	listed := false
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/orgs", func(w http.ResponseWriter, _ *http.Request) {
		listed = true
		fmt.Fprint(w, `{"orgs":[]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	url := srv.URL
	server = &url
	chosen := "nine-cobalt-mallard"
	org = &chosen
	orgOnce = sync.Once{}
	orgSlug, orgErr = "", nil

	got, err := currentOrg()
	if err != nil {
		t.Fatalf("currentOrg: %v", err)
	}
	if got != "nine-cobalt-mallard" {
		t.Errorf("resolved %q, want the flag's value", got)
	}
	if listed {
		t.Error("listed organizations despite --org being given")
	}
}

func TestOrgURLNestsThePath(t *testing.T) {
	fakeOrgs(t, `{"orgs":[{"slug":"nine-cobalt-mallard","name":"Acme","role":"user"}]}`)

	u, err := orgURL("/workspaces/api/sessions")
	if err != nil {
		t.Fatalf("orgURL: %v", err)
	}
	if !strings.HasSuffix(u, "/v1/orgs/nine-cobalt-mallard/workspaces/api/sessions") {
		t.Errorf("orgURL built %q", u)
	}
}

// The hint strings a user pastes back have to carry --org exactly when their
// next command would need it.
func TestOrgArgEchoesOnlyWhatWasGiven(t *testing.T) {
	none := ""
	org = &none
	if got := orgArg(); got != "" {
		t.Errorf("orgArg() = %q with no flag given, want empty", got)
	}
	given := "nine-cobalt-mallard"
	org = &given
	if got := orgArg(); got != "--org nine-cobalt-mallard " {
		t.Errorf("orgArg() = %q", got)
	}
}
