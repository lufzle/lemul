package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"strings"
	"sync"
	"text/tabwriter"
)

// Picking an organization is the CLIENT's job.
//
// No API assumes one -- every organization-scoped route names it in the path --
// because a server that guesses is a server that eventually guesses wrong on a
// destructive verb. The convenience of not typing --org every time belongs
// here, where the rule can be simple and visible:
//
//	--org given            use it
//	exactly one to choose  use it
//	more than one          refuse, and say which
//
// The refusal is the point. Someone in their own organization and a customer's
// should never have `lem rm` fall back to whichever one a default happened to
// name.

type orgDoc struct {
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	Personal bool   `json:"personal"`
}

// resolvedOrg is memoised because several commands build more than one URL and
// the answer cannot change within a single invocation.
var (
	orgOnce sync.Once
	orgSlug string
	orgErr  error
)

// listOrgs asks which organizations the caller belongs to.
func listOrgs() ([]orgDoc, error) {
	u := strings.TrimRight(*server, "/") + "/v1/orgs"
	resp, err := request(http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("list organizations: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list organizations: %s: %s",
			resp.Status, strings.TrimSpace(string(body)))
	}
	var out struct {
		Orgs []orgDoc `json:"orgs"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("list organizations: bad response: %w", err)
	}
	return out.Orgs, nil
}

// currentOrg resolves the organization this invocation acts in.
func currentOrg() (string, error) {
	orgOnce.Do(func() {
		if *org != "" {
			orgSlug = *org
			return
		}
		orgs, err := listOrgs()
		if err != nil {
			orgErr = err
			return
		}
		switch len(orgs) {
		case 1:
			orgSlug = orgs[0].Slug
		case 0:
			// Not reachable through a normal sign-in -- everyone gets their own
			// organization -- so if it happens, saying so plainly beats an
			// empty {org} segment and a 404.
			orgErr = fmt.Errorf("you are not a member of any organization")
		default:
			orgErr = fmt.Errorf("you belong to %d organizations, so --org is required:\n%s",
				len(orgs), orgLines(orgs))
		}
	})
	return orgSlug, orgErr
}

func orgLines(orgs []orgDoc) string {
	var b strings.Builder
	for _, o := range orgs {
		fmt.Fprintf(&b, "  --org %-28s %s (%s)\n", o.Slug, o.Name, o.Role)
	}
	return strings.TrimRight(b.String(), "\n")
}

// orgURL builds an organization-scoped URL. Every organization-scoped request
// goes through it, so the {org} segment cannot be forgotten at one call site.
func orgURL(path string) (string, error) {
	slug, err := currentOrg()
	if err != nil {
		return "", err
	}
	return strings.TrimRight(*server, "/") + "/v1/orgs/" + neturl.PathEscape(slug) + path, nil
}

// runOrgList implements `lem orgs`.
func runOrgList() error {
	orgs, err := listOrgs()
	if err != nil {
		return err
	}
	if len(orgs) == 0 {
		fmt.Println("you are not a member of any organization")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ORG\tNAME\tROLE\t")
	for _, o := range orgs {
		kind := ""
		if o.Personal {
			kind = "your own"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", o.Slug, o.Name, o.Role, kind)
	}
	return w.Flush()
}

// runInvite implements `lem invite`, which issues a code for the organization
// being acted in. Owners only, enforced by the control plane.
func runInvite(role string) error {
	u, err := orgURL("/invites")
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]string{"role": role})
	if err != nil {
		return err
	}
	resp, err := request(http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("invite: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("invite: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Code      string `json:"code"`
		Org       string `json:"org"`
		Role      string `json:"role"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("invite: bad response: %w", err)
	}
	fmt.Printf("invite code for %s (%s), valid until %s:\n\n  %s\n\n"+
		"they redeem it with: lem join %s\n", out.Org, out.Role, out.ExpiresAt, out.Code, out.Code)
	return nil
}

// runJoin implements `lem join <code>`.
func runJoin(code string) error {
	u := strings.TrimRight(*server, "/") + "/v1/invites/" + neturl.PathEscape(code) + "/redeem"
	resp, err := request(http.MethodPost, u, nil)
	if err != nil {
		return fmt.Errorf("join: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("join: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var out struct {
		Org    string `json:"org"`
		Name   string `json:"name"`
		Role   string `json:"role"`
		Joined bool   `json:"joined"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("join: bad response: %w", err)
	}
	if !out.Joined {
		fmt.Printf("you were already a member of %s (%s)\n", out.Name, out.Org)
		return nil
	}
	fmt.Printf("joined %s as %s\n\nact in it with: lem --org %s ...\n",
		out.Name, out.Role, out.Org)
	return nil
}

// orgArg echoes --org back into a command a user is told to run.
//
// Only when they gave it, which is exactly when it is required: someone in one
// organization never needs it, and printing it for them would teach a flag that
// their own next command would not want. Someone in several always needs it, and
// a hint they cannot paste is worse than no hint.
func orgArg() string {
	if *org == "" {
		return ""
	}
	return "--org " + *org + " "
}
