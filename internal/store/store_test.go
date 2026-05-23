package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/lufzle/lemul/internal/store/celldb"
)

// accessScopeOwner mirrors the CHECK in schema.sql, and the constant in mgmtapi
// that these tests cannot import (it is the layer above). Spelled here so a
// workspace built straight from the generated params carries a storable value.
const accessScopeOwner = "owner"

// One container for the whole package, owned by TestMain.
//
// It has to be TestMain rather than a sync.Once inside a helper: the first
// version registered cleanup against whichever test happened to start the
// container, so the container was terminated when THAT test finished and every
// later case failed to connect. The lifetime belongs to the package, so the
// package has to own it.
//
// One container rather than one per test because the isolation these tests need
// is between TENANTS -- which is the thing under test -- not between cases. Each
// case uses its own tenant uuids.
var (
	sharedDSN  string
	adminDSN   string
	skipReason string
)

const (
	appRole = "lemul_app"
	appPass = "lemul_app_password"
)

func TestMain(m *testing.M) {
	code, err := runWithPostgres(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "store tests:", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func runWithPostgres(m *testing.M) (int, error) {
	if os.Getenv("LEMUL_SKIP_DB_TESTS") != "" {
		skipReason = "LEMUL_SKIP_DB_TESTS is set"
		return m.Run(), nil
	}
	ctx := context.Background()
	c, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("lemul"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(90*time.Second)),
	)
	if err != nil {
		// No Docker is a skip, not a failure: `go test ./...` on a machine
		// without it should still be useful.
		skipReason = "cannot start postgres: " + err.Error()
		return m.Run(), nil
	}
	defer func() { _ = testcontainers.TerminateContainer(c) }()

	if adminDSN, err = c.ConnectionString(ctx, "sslmode=disable"); err != nil {
		return 0, err
	}
	if err := prepare(ctx); err != nil {
		return 0, err
	}
	return m.Run(), nil
}

// prepare applies the schema as the owner, then creates the unprivileged role
// the application uses.
//
// The role matters more than the container. Connecting as the superuser -- which
// is what the postgres module hands out -- makes every policy in schema.sql
// inert, so a suite that used it would pass while proving nothing.
func prepare(ctx context.Context) error {
	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		return err
	}
	defer admin.Close()

	if _, err := admin.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	for _, stmt := range []string{
		fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s' NOBYPASSRLS`, appRole, appPass),
		fmt.Sprintf(`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO %s`, appRole),
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("%s: %w", stmt, err)
		}
	}
	sharedDSN = strings.Replace(adminDSN, "postgres:postgres@", appRole+":"+appPass+"@", 1)
	return nil
}

func postgres(t *testing.T) string {
	t.Helper()
	if skipReason != "" {
		t.Skip(skipReason)
	}
	return sharedDSN
}

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), postgres(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// seedTenant creates an organization's first user -- its owner -- and returns
// the ids. It is the sign-up path in miniature: a user row whose home is the new
// organization, plus the owner membership that makes them visible in it.
func seedTenant(t *testing.T, s *Store, n int) (tenant, user string) {
	t.Helper()
	tenant = fmt.Sprintf("00000000-0000-4000-8000-0000000000%02d", n)
	user = fmt.Sprintf("00000000-0000-4000-9000-0000000000%02d", n)
	addUser(t, s, tenant, user, fmt.Sprintf("sub-%d", n), "owner")
	return tenant, user
}

// addUser creates a user homed in an organization and joins them to it.
//
// Both inside one transaction, which is also what proves app_user's policy
// works at all: the user row is inserted BEFORE its org_member row can exist,
// because org_member.user_id references it, so the insert has to pass its own
// WITH CHECK on the home_tenant_id disjunct alone.
func addUser(t *testing.T, s *Store, tenant, user, subject, role string) {
	t.Helper()
	ctx := context.Background()
	err := s.InTenant(ctx, Scope{TenantID: tenant, UserID: user}, func(q *celldb.Queries) error {
		if _, err := q.CreateUser(ctx, celldb.CreateUserParams{
			ID: user, HomeTenantID: tenant,
			IdpIssuer: "https://idp.test", IdpSubject: subject,
		}); err != nil {
			return err
		}
		_, err := q.AddOrgMember(ctx, celldb.AddOrgMemberParams{
			TenantID: tenant, UserID: user, Role: role,
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed user %s in %s: %v", user, tenant, err)
	}
}

// joinOrg adds an EXISTING user to another organization, which is what invite
// redemption does.
func joinOrg(t *testing.T, s *Store, tenant, user, role string) {
	t.Helper()
	ctx := context.Background()
	err := s.InTenant(ctx, Scope{TenantID: tenant, UserID: user}, func(q *celldb.Queries) error {
		_, err := q.AddOrgMember(ctx, celldb.AddOrgMemberParams{
			TenantID: tenant, UserID: user, Role: role,
		})
		return err
	})
	if err != nil {
		t.Fatalf("join %s to %s: %v", user, tenant, err)
	}
}

func mkWorkspace(t *testing.T, s *Store, tenant, user, name string) celldb.Workspace {
	t.Helper()
	var out celldb.Workspace
	err := s.InTenant(context.Background(), Scope{TenantID: tenant, UserID: user},
		func(q *celldb.Queries) error {
			w, err := q.CreateWorkspace(context.Background(), celldb.CreateWorkspaceParams{
				ID: newTestUUID(name), TenantID: tenant, Name: name, OwnerUserID: user,
				// Spelled out rather than left zero. The column's DEFAULT never
				// applies here, because the generated INSERT always names every
				// column -- so the zero value reaches the CHECK as '' and is
				// refused. mgmtapi.createWorkspace defaults it for the real
				// paths; anything going straight to the query says it itself.
				AccessScope: accessScopeOwner,
			})
			out = w
			return err
		})
	if err != nil {
		t.Fatalf("create workspace %q: %v", name, err)
	}
	return out
}

// THE test for this phase.
//
// Tenant A creates a workspace; tenant B must not be able to see it, by id or
// by listing. Not "the handler filters it out" -- the database does, so a
// handler that forgot its predicate still cannot leak.
func TestCrossTenantReadsFindNothing(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	tenantA, userA := seedTenant(t, s, 1)
	tenantB, userB := seedTenant(t, s, 2)

	ws := mkWorkspace(t, s, tenantA, userA, "secret-project")

	err := s.InTenant(ctx, Scope{TenantID: tenantB, UserID: userB}, func(q *celldb.Queries) error {
		if _, err := q.GetWorkspace(ctx, celldb.GetWorkspaceParams{ID: ws.ID, TenantID: tenantA}); err == nil {
			t.Error("tenant B read tenant A's workspace by id, naming A's tenant explicitly")
		}
		if _, err := q.GetWorkspace(ctx, celldb.GetWorkspaceParams{ID: ws.ID, TenantID: tenantB}); err == nil {
			t.Error("tenant B read tenant A's workspace by id")
		}
		list, err := q.ListWorkspaces(ctx, tenantA)
		if err != nil {
			return err
		}
		if len(list) != 0 {
			t.Errorf("tenant B listed %d of tenant A's workspaces", len(list))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("InTenant: %v", err)
	}
}

// The write side. RLS WITH CHECK must stop a row being INSERTED into another
// tenant, not merely stop it being read back.
func TestCrossTenantWritesAreRefused(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	tenantA, userA := seedTenant(t, s, 3)
	tenantB, userB := seedTenant(t, s, 4)
	_ = userA

	// Returned rather than swallowed. Postgres aborts the WHOLE transaction on
	// any statement error, so a callback that notes the failure and carries on
	// gets "commit unexpectedly resulted in rollback" from the commit instead --
	// which says nothing about what actually happened.
	err := s.InTenant(ctx, Scope{TenantID: tenantB, UserID: userB}, func(q *celldb.Queries) error {
		_, err := q.CreateWorkspace(ctx, celldb.CreateWorkspaceParams{
			ID: newTestUUID("smuggled"), TenantID: tenantA, Name: "smuggled", OwnerUserID: userB,
			AccessScope: accessScopeOwner,
		})
		return err
	})
	if err == nil {
		t.Fatal("tenant B inserted a workspace into tenant A")
	}
}

// Fail closed. An unscoped connection must read NOTHING rather than everything,
// which is what current_setting(..., true) returning NULL buys.
func TestUnscopedConnectionsSeeNothing(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	tenantA, userA := seedTenant(t, s, 5)
	mkWorkspace(t, s, tenantA, userA, "visible-to-a")

	var count int
	err := s.Unscoped(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM workspace`).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("an unscoped connection saw %d workspaces; RLS is not failing closed", count)
	}
}

// InTenant refuses an empty scope rather than running a query that RLS would
// then silently return nothing for -- a caller that forgot the tenant should
// get an error, not an empty result it might mistake for "none exist".
func TestInTenantRefusesAnEmptyScope(t *testing.T) {
	s := openStore(t)
	err := s.InTenant(context.Background(), Scope{}, func(*celldb.Queries) error {
		t.Error("the callback ran with no tenant scope")
		return nil
	})
	if err == nil {
		t.Fatal("InTenant accepted an empty scope")
	}
}

// The check that stops RLS being decorative.
func TestOpenRefusesARoleWithBypassRLS(t *testing.T) {
	postgres(t) // ensure the container and adminDSN exist
	if _, err := Open(context.Background(), adminDSN); err == nil {
		t.Fatal("Open accepted the superuser, whose BYPASSRLS makes every policy inert")
	} else if !strings.Contains(err.Error(), "BYPASSRLS") {
		t.Errorf("the refusal should name BYPASSRLS, got: %v", err)
	}
}

// The generation counter is what the ECS client-token derives from, so two
// concurrent increments must produce two different values. The JSON store's
// read-modify-write could not, which is how two placements once shared a token.
func TestNextGenerationIsAtomicUnderConcurrency(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	tenant, user := seedTenant(t, s, 7)
	ws := mkWorkspace(t, s, tenant, user, "raced")

	const n = 20
	got := make(chan int64, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.InTenant(ctx, Scope{TenantID: tenant, UserID: user}, func(q *celldb.Queries) error {
				g, err := q.NextGeneration(ctx, celldb.NextGenerationParams{ID: ws.ID, TenantID: tenant})
				if err == nil {
					got <- g
				}
				return err
			})
		}()
	}
	wg.Wait()
	close(got)

	seen := map[int64]bool{}
	for g := range got {
		if seen[g] {
			t.Fatalf("generation %d was handed out twice; two placements would derive "+
				"the same ECS client-token and the second would adopt the first's task", g)
		}
		seen[g] = true
	}
	if len(seen) != n {
		t.Errorf("got %d distinct generations from %d increments", len(seen), n)
	}
}

// The advisory lock serialises placement across instances, which a per-process
// mutex could not.
func TestPlacementLockSerialises(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()

	var mu sync.Mutex
	inside, maxInside := 0, 0
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.WithPlacementLock(ctx, "ws-contended", func() error {
				mu.Lock()
				inside++
				if inside > maxInside {
					maxInside = inside
				}
				mu.Unlock()
				time.Sleep(5 * time.Millisecond)
				mu.Lock()
				inside--
				mu.Unlock()
				return nil
			})
		}()
	}
	wg.Wait()
	if maxInside != 1 {
		t.Fatalf("%d holders were inside the placement lock at once", maxInside)
	}
}

func TestErrNotFoundIsWhatAMissingRowLooksLike(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	tenant, user := seedTenant(t, s, 8)

	err := s.InTenant(ctx, Scope{TenantID: tenant, UserID: user}, func(q *celldb.Queries) error {
		_, err := q.GetWorkspace(ctx, celldb.GetWorkspaceParams{
			ID: "00000000-0000-4000-8000-00000000ffff", TenantID: tenant,
		})
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("a missing row gave %v, want ErrNotFound", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("InTenant: %v", err)
	}
}

// newTestUUID makes a stable uuid from a label, so a failure names a workspace
// rather than a random string.
func newTestUUID(label string) string {
	h := 0
	for _, r := range label {
		h = h*31 + int(r)
	}
	return fmt.Sprintf("00000000-0000-4000-a000-%012x", h&0xffffffffffff)
}

// A user belongs to the cell and to many organizations, so app_user cannot be
// isolated by a tenant_id column -- it has none. Its policy is membership
// instead, and this is the test that says the substitution actually holds.
//
// Inside organization A's transaction you see exactly A's members. Not A's plus
// your own from elsewhere, and not everyone in the cell.
func TestUserVisibilityFollowsMembership(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()

	orgA, alice := seedTenant(t, s, 20)
	orgB, bob := seedTenant(t, s, 21)

	// Carol is homed in B and invited into A, so she is the case that separates
	// "member of this organization" from "homed here".
	carol := "00000000-0000-4000-9000-0000000000c0"
	addUser(t, s, orgB, carol, "sub-carol", "user")
	joinOrg(t, s, orgA, carol, "user")

	inOrg := func(org, actor string) map[string]bool {
		t.Helper()
		seen := map[string]bool{}
		err := s.InTenant(ctx, Scope{TenantID: org, UserID: actor}, func(q *celldb.Queries) error {
			rows, err := q.ListOrgMembers(ctx, org)
			if err != nil {
				return err
			}
			for _, u := range rows {
				seen[u.ID] = true
			}
			return nil
		})
		if err != nil {
			t.Fatalf("list members of %s: %v", org, err)
		}
		return seen
	}

	a := inOrg(orgA, alice)
	if !a[alice] || !a[carol] {
		t.Errorf("organization A should hold alice and carol, got %v", a)
	}
	if a[bob] {
		t.Error("bob is visible in an organization he is not a member of")
	}

	b := inOrg(orgB, bob)
	if !b[bob] || !b[carol] {
		t.Errorf("organization B should hold bob and carol, got %v", b)
	}
	if b[alice] {
		t.Error("alice is visible in an organization she is not a member of")
	}
}

// Reading a user row directly, rather than through the membership join, must
// obey the same rule -- otherwise the join is doing the work and the policy is
// decorative.
func TestReadingAnotherOrganizationsUserFindsNothing(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()

	orgA, alice := seedTenant(t, s, 22)
	orgB, bob := seedTenant(t, s, 23)

	err := s.InTenant(ctx, Scope{TenantID: orgB, UserID: bob}, func(q *celldb.Queries) error {
		if _, err := q.GetUser(ctx, alice); !errors.Is(err, ErrNotFound) {
			t.Errorf("bob read alice's user row by id: %v", err)
		}
		if _, err := q.GetUserBySubject(ctx, celldb.GetUserBySubjectParams{
			IdpIssuer: "https://idp.test", IdpSubject: "sub-22",
		}); !errors.Is(err, ErrNotFound) {
			t.Errorf("bob resolved alice by subject: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("InTenant: %v", err)
	}
	_ = orgA
}

// "Which organizations am I in" is the one question that cannot be asked from
// inside one, so org_member's policy also admits a row by user_id. This pins
// both halves: you see all of YOUR memberships, and none of anyone else's.
func TestListingMyOrganizationsCrossesOrganizations(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()

	orgA, alice := seedTenant(t, s, 24)
	orgB, bob := seedTenant(t, s, 25)
	joinOrg(t, s, orgB, alice, "user")

	mine := func(home, user string) map[string]string {
		t.Helper()
		roles := map[string]string{}
		err := s.InTenant(ctx, Scope{TenantID: home, UserID: user}, func(q *celldb.Queries) error {
			rows, err := q.ListOrgsForUser(ctx, user)
			if err != nil {
				return err
			}
			for _, m := range rows {
				roles[m.TenantID] = m.Role
			}
			return nil
		})
		if err != nil {
			t.Fatalf("list organizations for %s: %v", user, err)
		}
		return roles
	}

	got := mine(orgA, alice)
	if got[orgA] != "owner" {
		t.Errorf("alice should own her own organization, got %q", got[orgA])
	}
	if got[orgB] != "user" {
		t.Errorf("alice should be a plain user of B, got %q", got[orgB])
	}

	// The widening is scoped to the caller. Bob asking the same question from
	// inside his own organization must not see alice's membership of it.
	if b := mine(orgB, bob); len(b) != 1 || b[orgB] != "owner" {
		t.Errorf("bob sees %v, want only his own organization", b)
	}
}

// The read widening must not become a write widening. Being able to SEE your
// own memberships anywhere would be worth very little if it also let you INSERT
// one into an organization the transaction is not scoped to.
func TestJoiningAnUnscopedOrganizationIsRefused(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()

	orgA, _ := seedTenant(t, s, 26)
	orgB, bob := seedTenant(t, s, 27)

	err := s.InTenant(ctx, Scope{TenantID: orgB, UserID: bob}, func(q *celldb.Queries) error {
		_, err := q.AddOrgMember(ctx, celldb.AddOrgMemberParams{
			TenantID: orgA, UserID: bob, Role: "owner",
		})
		return err
	})
	if err == nil {
		t.Fatal("bob added himself to organization A from inside his own")
	}
}

// Sessions and workspaces still isolate on tenant_id, and a user in two
// organizations must not carry visibility between them.
func TestMembershipInTwoOrganizationsDoesNotMergeThem(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()

	orgA, alice := seedTenant(t, s, 28)
	orgB, bob := seedTenant(t, s, 29)
	joinOrg(t, s, orgB, alice, "user")

	mkWorkspace(t, s, orgA, alice, "a-only")
	mkWorkspace(t, s, orgB, bob, "b-only")

	err := s.InTenant(ctx, Scope{TenantID: orgB, UserID: alice}, func(q *celldb.Queries) error {
		list, err := q.ListWorkspaces(ctx, orgB)
		if err != nil {
			return err
		}
		if len(list) != 1 || list[0].Name != "b-only" {
			t.Errorf("acting in B, alice sees %d workspaces: %v", len(list), names(list))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("InTenant: %v", err)
	}
}

func names(ws []celldb.Workspace) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.Name)
	}
	return out
}

// One person, one row, whichever organization first resolves them. The global
// UNIQUE is what makes a concurrent first sign-up converge rather than create
// two users for one human -- and it has to hold ACROSS organizations, which is
// the part a per-tenant constraint would have got wrong.
func TestOneUserRowPerPrincipalAcrossOrganizations(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()

	orgA, _ := seedTenant(t, s, 30)
	orgB, _ := seedTenant(t, s, 31)
	_ = orgA

	dup := "00000000-0000-4000-9000-0000000000d0"
	err := s.InTenant(ctx, Scope{TenantID: orgB, UserID: dup}, func(q *celldb.Queries) error {
		_, err := q.CreateUser(ctx, celldb.CreateUserParams{
			ID: dup, HomeTenantID: orgB,
			IdpIssuer: "https://idp.test", IdpSubject: "sub-30", // already used by org A's owner
		})
		return err
	})
	if err == nil {
		t.Fatal("the same identity provider subject got a second user row in another organization")
	}
}

// The uid allocator, which is what keeps two members of one workspace out of
// each other's homes.
//
// Worth testing at this level rather than through a handler: every property here
// is a property of the statement, and three of the four are invisible from
// above.
func TestMemberUIDsAreAllocatedOncePerMemberAndNeverReused(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	tenant, owner := seedTenant(t, s, 11)
	second := "00000000-0000-4000-9000-000000000911"
	addUser(t, s, tenant, second, "sub-911", "user")
	ws := mkWorkspace(t, s, tenant, owner, "shared-box")

	alloc := func(user string) int32 {
		t.Helper()
		var got int32
		err := s.InTenant(ctx, Scope{TenantID: tenant, UserID: user}, func(q *celldb.Queries) error {
			if err := q.EnsureMember(ctx, celldb.EnsureMemberParams{
				WorkspaceID: ws.ID, UserID: user, TenantID: tenant, Role: "user",
			}); err != nil {
				return err
			}
			next, err := q.NextWorkspaceUID(ctx, celldb.NextWorkspaceUIDParams{
				ID: ws.ID, TenantID: tenant,
			})
			if err != nil {
				return err
			}
			m, err := q.SetMemberUID(ctx, celldb.SetMemberUIDParams{
				WorkspaceID: ws.ID, UserID: user, TenantID: tenant, Uid: &next,
			})
			if err != nil {
				return err
			}
			got = *m.Uid
			return nil
		})
		if err != nil {
			t.Fatalf("allocate for %s: %v", user, err)
		}
		return got
	}

	// The floor. Below 2000 sit the image's own `node` user at 1000 and
	// everything a distribution reserves, so colliding there would put a session
	// in the image's home rather than a member's.
	first := alloc(owner)
	if first != 2000 {
		t.Errorf("first uid was %d, want 2000", first)
	}

	// A second member must not get the same number: the uid IS the isolation.
	if got := alloc(second); got != 2001 {
		t.Errorf("second member got uid %d, want 2001", got)
	}

	// Idempotent. Re-running must not renumber somebody, because the files they
	// already own on the volume carry the old number -- a member who is
	// renumbered loses their home to nobody.
	err := s.InTenant(ctx, Scope{TenantID: tenant, UserID: owner}, func(q *celldb.Queries) error {
		n := int32(9999)
		_, err := q.SetMemberUID(ctx, celldb.SetMemberUIDParams{
			WorkspaceID: ws.ID, UserID: owner, TenantID: tenant, Uid: &n,
		})
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("an existing uid was overwritten: %v", err)
	}

	// A departed member's uid is not handed out again. Their files outlive the
	// membership row, so reuse would give somebody else their home.
	err = s.InTenant(ctx, Scope{TenantID: tenant, UserID: owner}, func(q *celldb.Queries) error {
		_, err := q.RemoveMember(ctx, celldb.RemoveMemberParams{
			WorkspaceID: ws.ID, UserID: second, TenantID: tenant,
		})
		return err
	})
	if err != nil {
		t.Fatalf("removing the second member: %v", err)
	}
	third := "00000000-0000-4000-9000-000000000912"
	addUser(t, s, tenant, third, "sub-912", "user")
	if got := alloc(third); got != 2002 {
		t.Errorf("a new member got uid %d, reusing a departed member's home", got)
	}
}

// Section 2.4's lifecycle policy arrives on a workspace by DEFAULT, and the
// defaults are what every workspace created before this column existed will
// have. Asserted rather than assumed, because the code reads these numbers
// without ever having written them.
func TestLifecyclePolicyDefaults(t *testing.T) {
	s := openStore(t)
	tenant, user := seedTenant(t, s, 41)
	ws := mkWorkspace(t, s, tenant, user, "policy-defaults")

	// Decision #7: auto-stop is ON by default, so a zero here would silently
	// disable the whole cascade for every workspace.
	if ws.IdleTimeoutSecs != 7200 {
		t.Errorf("idle_timeout_secs = %d, want 7200 (2 h)", ws.IdleTimeoutSecs)
	}
	// 1 h since 2026-08-03. A workspace is a shared machine, so the hold is
	// amortised across a team rather than charged to whoever quit last, and the
	// cold start it avoids is now an EBS snapshot restore rather than a bare
	// task start.
	if ws.WarmHoldSecs != 3600 {
		t.Errorf("warm_hold_secs = %d, want 3600 (1 h)", ws.WarmHoldSecs)
	}
	if ws.AdmissionPolicy != "min_free_memory_pct" {
		t.Errorf("admission_policy = %q, want min_free_memory_pct (decision #9)", ws.AdmissionPolicy)
	}
	if ws.MaxSessions != 4 {
		t.Errorf("max_sessions = %d, want 4", ws.MaxSessions)
	}
	if ws.MinFreeMemoryPct != 20 {
		t.Errorf("min_free_memory_pct = %d, want 20", ws.MinFreeMemoryPct)
	}
	// No hold is running on a workspace nobody has used.
	if ws.WarmHoldUntil != nil {
		t.Errorf("warm_hold_until = %v, want nil", ws.WarmHoldUntil)
	}
}

// The warm-hold deadline is a COLUMN rather than a timer in the control plane's
// memory, and this is the property that buys: it survives the process. A
// time.AfterFunc would die with a redeploy, leaving the task running with
// nobody timing it -- the exact failure warm hold exists to prevent, made
// permanent.
func TestWarmHoldDeadlineRoundTrips(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	tenant, user := seedTenant(t, s, 42)
	ws := mkWorkspace(t, s, tenant, user, "warm-hold")
	sc := Scope{TenantID: tenant, UserID: user}

	until := time.Now().Add(5 * time.Minute).UTC().Truncate(time.Millisecond)
	if err := s.InTenant(ctx, sc, func(q *celldb.Queries) error {
		return q.SetWarmHold(ctx, celldb.SetWarmHoldParams{
			ID: ws.ID, TenantID: tenant, WarmHoldUntil: &until,
		})
	}); err != nil {
		t.Fatalf("setting the warm hold: %v", err)
	}

	got := readWorkspace(t, s, sc, ws.ID)
	if got.WarmHoldUntil == nil {
		t.Fatal("the warm-hold deadline did not persist")
	}
	if d := got.WarmHoldUntil.Sub(until); d > time.Second || d < -time.Second {
		t.Errorf("warm_hold_until = %v, want %v", got.WarmHoldUntil, until)
	}

	// Clearing it is how a session appearing again cancels the hold, and NULL
	// has to mean "no hold" rather than "a hold at the zero time".
	if err := s.InTenant(ctx, sc, func(q *celldb.Queries) error {
		return q.SetWarmHold(ctx, celldb.SetWarmHoldParams{
			ID: ws.ID, TenantID: tenant, WarmHoldUntil: nil,
		})
	}); err != nil {
		t.Fatalf("clearing the warm hold: %v", err)
	}
	if got := readWorkspace(t, s, sc, ws.ID); got.WarmHoldUntil != nil {
		t.Errorf("the warm hold was not cleared: %v", got.WarmHoldUntil)
	}
}

// The database is the authority on what is storable, which is what makes the
// early refusal in mgmtapi a convenience rather than the only guard.
func TestUnstorableLifecyclePolicyIsRefused(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	tenant, user := seedTenant(t, s, 43)
	ws := mkWorkspace(t, s, tenant, user, "policy-check")
	sc := Scope{TenantID: tenant, UserID: user}

	for _, tc := range []struct {
		name string
		p    celldb.SetWorkspacePolicyParams
	}{
		{"an admission policy that does not exist", celldb.SetWorkspacePolicyParams{
			IdleTimeoutSecs: 60, WarmHoldSecs: 60,
			AdmissionPolicy: "whatever", MaxSessions: 1, MinFreeMemoryPct: 20,
		}},
		{"a percentage above 100", celldb.SetWorkspacePolicyParams{
			IdleTimeoutSecs: 60, WarmHoldSecs: 60,
			AdmissionPolicy: "min_free_memory_pct", MaxSessions: 1, MinFreeMemoryPct: 101,
		}},
		{"a negative idle timeout", celldb.SetWorkspacePolicyParams{
			IdleTimeoutSecs: -1, WarmHoldSecs: 60,
			AdmissionPolicy: "min_free_memory_pct", MaxSessions: 1, MinFreeMemoryPct: 20,
		}},
		{"zero max_sessions, which would admit nothing", celldb.SetWorkspacePolicyParams{
			IdleTimeoutSecs: 60, WarmHoldSecs: 60,
			AdmissionPolicy: "max_sessions", MaxSessions: 0, MinFreeMemoryPct: 20,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.p
			p.ID, p.TenantID = ws.ID, tenant
			err := s.InTenant(ctx, sc, func(q *celldb.Queries) error {
				_, err := q.SetWorkspacePolicy(ctx, p)
				return err
			})
			if err == nil {
				t.Error("the database accepted a policy its CHECK should refuse")
			}
		})
	}
}

func readWorkspace(t *testing.T, s *Store, sc Scope, id string) celldb.Workspace {
	t.Helper()
	var out celldb.Workspace
	err := s.InTenant(context.Background(), sc, func(q *celldb.Queries) error {
		w, err := q.GetWorkspace(context.Background(),
			celldb.GetWorkspaceParams{ID: id, TenantID: sc.TenantID})
		out = w
		return err
	})
	if err != nil {
		t.Fatalf("reading workspace %s: %v", id, err)
	}
	return out
}

// The workspace's disk between tasks (section 9 item 0).
//
// ECS attaches one volume per task, it must be a NEW volume, and there is no
// re-attach -- so this column is the ONLY thing connecting a stopped workspace
// to its filesystem. A row that loses it loses every member's home and
// conversation, with no error anywhere: the next placement simply creates an
// empty volume and the workspace comes back blank.
func TestTheWorkspaceSnapshotRoundTrips(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	tenant, user := seedTenant(t, s, 61)
	ws := mkWorkspace(t, s, tenant, user, "snapshot-round-trip")
	sc := Scope{TenantID: tenant, UserID: user}

	// A workspace that has never been stopped has no snapshot, and NULL has to
	// mean that rather than "restore from the empty string" -- which the ecs
	// driver would pass to RunTask as a snapshotId and be refused for.
	if got := readWorkspace(t, s, sc, ws.ID); got.SnapshotID != nil {
		t.Fatalf("a new workspace already names snapshot %q", *got.SnapshotID)
	}

	set := func(id *string) celldb.Workspace {
		t.Helper()
		var out celldb.Workspace
		if err := s.InTenant(ctx, sc, func(q *celldb.Queries) error {
			var err error
			out, err = q.SetWorkspaceSnapshot(ctx, celldb.SetWorkspaceSnapshotParams{
				ID: ws.ID, TenantID: tenant, SnapshotID: id,
			})
			return err
		}); err != nil {
			t.Fatalf("setting the snapshot: %v", err)
		}
		return out
	}

	first := "snap-0123456789abcdef0"
	if got := set(&first); got.SnapshotID == nil || *got.SnapshotID != first {
		t.Fatalf("RETURNING gave %v, want %q", got.SnapshotID, first)
	}
	if got := readWorkspace(t, s, sc, ws.ID); got.SnapshotID == nil || *got.SnapshotID != first {
		t.Errorf("snapshot_id = %v, want %q", got.SnapshotID, first)
	}

	// Replacing it is what every subsequent stop does, and the previous id is
	// what the caller then deletes -- so the write has to land before anything
	// acts on the old one.
	second := "snap-fedcba98765432100"
	if got := set(&second); *got.SnapshotID != second {
		t.Errorf("snapshot_id = %q after replacement, want %q", *got.SnapshotID, second)
	}

	// And clearing it, which is what deleting a workspace's disk means.
	if got := set(nil); got.SnapshotID != nil {
		t.Errorf("snapshot_id = %v after clearing", got.SnapshotID)
	}
}
