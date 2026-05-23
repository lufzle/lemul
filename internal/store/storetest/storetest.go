// Package storetest starts a throwaway Postgres for tests that need a real one.
//
// It exists because the control plane's state IS a database now: there is no
// in-process fallback to test against, and a fake would not exercise the thing
// most worth exercising, which is row-level security.
//
// Both tiers land in one database. In a deployment a cell is its own database
// and the directory is separate -- that is what "a cell is at minimum its own
// database" means -- but the table names do not collide, and a second container
// per test package would buy isolation between two schemas that are never
// queried through one connection anyway.
//
// The container is shared across a package's tests and never explicitly
// terminated -- testcontainers' reaper removes it when the test process exits.
// Binding its lifetime to any one test's t.Cleanup was tried and is wrong: the
// container went away when whichever test happened to start it finished.
package storetest

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/lufzle/lemul/internal/directory"
	"github.com/lufzle/lemul/internal/store"
)

const (
	appRole = "lemul_app"
	appPass = "lemul_app_password"
)

var (
	once   sync.Once
	appDSN string
	skip   string
)

// DSN returns a connection string for a role WITHOUT BYPASSRLS, with both
// schemas already applied.
//
// The role is the point. Connecting as the superuser -- which is what the
// postgres module hands out -- makes every policy inert, so a suite that used it
// would pass while proving nothing.
//
// Skips rather than fails when Docker is unavailable, so `go test ./...` stays
// useful on a machine without it.
func DSN(t *testing.T) string {
	t.Helper()
	once.Do(start)
	if skip != "" {
		t.Skip(skip)
	}
	return appDSN
}

// Open returns a Store on a fresh schema.
func Open(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), DSN(t))
	if err != nil {
		t.Fatalf("storetest: open: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

// OpenDirectory returns a Directory on the same database.
func OpenDirectory(t *testing.T) *directory.Directory {
	t.Helper()
	d, err := directory.Open(context.Background(), DSN(t))
	if err != nil {
		t.Fatalf("storetest: open directory: %v", err)
	}
	t.Cleanup(d.Close)
	return d
}

func start() {
	if os.Getenv("LEMUL_SKIP_DB_TESTS") != "" {
		skip = "LEMUL_SKIP_DB_TESTS is set"
		return
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
		skip = "cannot start postgres: " + err.Error()
		return
	}
	adminDSN, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		skip = "postgres connection string: " + err.Error()
		return
	}
	if err := prepare(ctx, adminDSN); err != nil {
		skip = "preparing postgres: " + err.Error()
		return
	}
	appDSN = strings.Replace(adminDSN, "postgres:postgres@", appRole+":"+appPass+"@", 1)
}

// prepare applies both schemas as the owner, then creates the unprivileged role
// the application connects as.
func prepare(ctx context.Context, adminDSN string) error {
	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		return err
	}
	defer admin.Close()

	// The directory first, because it is the tier the other one's tenant ids
	// come from. There are no cross-tier foreign keys -- deliberately, they are
	// separate databases in a deployment -- so the order does not matter to
	// Postgres; it matters to whoever reads this.
	if _, err := admin.Exec(ctx, directory.SchemaSQL); err != nil {
		return fmt.Errorf("apply directory schema: %w", err)
	}
	if _, err := admin.Exec(ctx, store.SchemaSQL); err != nil {
		return fmt.Errorf("apply cell schema: %w", err)
	}
	for _, stmt := range []string{
		fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s' NOBYPASSRLS`, appRole, appPass),
		fmt.Sprintf(`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO %s`, appRole),
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("%s: %w", stmt, err)
		}
	}
	return nil
}
