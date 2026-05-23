// Package store is one cell's database: users, workspaces, membership and
// sessions, all tenant-scoped and all behind row-level security.
//
// Every read and write goes through InTenant, which opens a transaction and
// scopes it before running anything. That is not a convenience wrapper -- it is
// the only place the tenant is established, so a query cannot quietly run
// unscoped. RLS then fails CLOSED if one ever does: the policies compare against
// a setting that is NULL when unset, and `tenant_id = NULL` is not TRUE, so an
// unscoped connection reads nothing rather than everything.
//
// The JSON file this replaces could express none of that, and could not express
// a second writer either: two control-plane instances sharing it would clobber
// each other on a whole-file rewrite, with the generation counter -- the one
// field that genuinely needs durability -- as the casualty.
package store

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lufzle/lemul/internal/store/celldb"
)

//go:embed schema.sql
var schemaSQL string

// SchemaSQL is exported for internal/store/storetest, which has to apply it as
// the OWNER -- the application role deliberately holds no DDL privileges.
var SchemaSQL = schemaSQL

// ErrNotFound is what a missing row looks like to callers, so they do not have
// to know that pgx spells it pgx.ErrNoRows.
var ErrNotFound = pgx.ErrNoRows

// Store owns a connection pool for one cell.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects and refuses a connection that cannot enforce tenancy.
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	s := &Store{pool: pool}
	if err := s.AssertNoBypassRLS(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// OpenPool wraps an existing pool. Tests use it to keep one container across
// cases; it deliberately does NOT assert BYPASSRLS, because a test that wants to
// prove the assertion fires has to be able to construct the failing case.
func OpenPool(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) Close() { s.pool.Close() }

// ApplySchema creates the tables and policies on a DDL-capable connection.
// Idempotent.
//
// It takes its own DSN rather than using the Store's pool, and that is not
// tidiness. The application role holds no DDL privileges -- deliberately, since
// it is also the role that must not hold BYPASSRLS -- so the pool this package
// otherwise uses is precisely the connection that CANNOT do this. A method on
// *Store would have been unusable by construction: Open refuses the only role
// with the privileges, and the role it accepts lacks them.
//
// Applied this way rather than by a migration tool, which is right only while
// there is no data worth preserving. The moment a deployment holds real
// organizations this has to become versioned migrations -- and the CHECK
// constraints will be the first thing that cannot be changed in place.
func ApplySchema(ctx context.Context, dsn string) error {
	return applySQL(ctx, dsn, schemaSQL, "store")
}

// applySQL runs one script on a throwaway connection.
func applySQL(ctx context.Context, dsn, script, who string) error {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("%s: connect for schema: %w", who, err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, script); err != nil {
		return fmt.Errorf("%s: apply schema: %w", who, err)
	}
	return nil
}

// AssertNoBypassRLS refuses a role that can see through the policies.
//
// This is the check that stops RLS being decorative. A role with BYPASSRLS --
// which every superuser has -- reads every tenant's rows while every policy in
// schema.sql sits there looking correct, and nothing in the application would
// notice. Postgres offers no per-session opt-out, so the only defence is
// refusing to start.
//
// The tables are additionally FORCE ROW LEVEL SECURITY, which covers the other
// half: ENABLE alone exempts the table OWNER, and the owner is usually whoever
// ran the migration.
func (s *Store) AssertNoBypassRLS(ctx context.Context) error {
	var bypass bool
	err := s.pool.QueryRow(ctx,
		`SELECT rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&bypass)
	if err != nil {
		return fmt.Errorf("store: checking BYPASSRLS: %w", err)
	}
	if bypass {
		return errors.New("store: this role holds BYPASSRLS, so row-level security would " +
			"not apply to it and any tenant could read every other tenant. Connect as an " +
			"application role created without BYPASSRLS")
	}
	return nil
}

// Scope is who a transaction runs as.
type Scope struct {
	TenantID string
	// UserID is set alongside the tenant so a policy can scope by user without
	// a schema change. Nothing reads it yet; it is already available at every
	// call site, and adding it later would mean revisiting all of them.
	UserID string
}

// InTenant runs fn inside a transaction scoped to a tenant.
//
// set_config with is_local=true rather than `SET LOCAL app.tenant_id = $1`,
// because SET takes no parameters -- interpolating the value into the statement
// would be an injection into the very setting the policies trust.
//
// Session pooling only. A transaction-pooling proxy would hand the same server
// connection to a later transaction with this scope still on it.
func (s *Store) InTenant(ctx context.Context, sc Scope, fn func(*celldb.Queries) error) error {
	if sc.TenantID == "" {
		return errors.New("store: refusing to run with an empty tenant scope")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`SELECT set_config('app.tenant_id', $1, true), set_config('app.user_id', $2, true)`,
		sc.TenantID, sc.UserID); err != nil {
		return fmt.Errorf("store: scope transaction: %w", err)
	}
	if err := fn(celldb.New(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// Unscoped runs fn with NO tenant set.
//
// For exactly two things: applying the schema, and the advisory lock below.
// Neither touches tenant data. Every policy still applies, so a query that
// reaches a tenant-scoped table through here reads nothing -- the intended
// failure rather than a limitation.
func (s *Store) Unscoped(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// WithPlacementLock serialises placement for one workspace across every
// control-plane instance.
//
// It replaces a per-process sync.Map, which was correct for exactly one
// instance. Two instances could each take a generation for the same workspace
// and derive DIFFERENT client tokens, so ECS idempotency did not save them:
// two tasks, two filesystems, split-brain on the snapshot (section 2.8).
//
// A transaction-level advisory lock releases on commit, rollback, or the
// connection dying -- which a lock table would not.
func (s *Store) WithPlacementLock(ctx context.Context, workspaceID string, fn func() error) error {
	return s.Unscoped(ctx, func(tx pgx.Tx) error {
		hi, lo := placementLockKey(workspaceID)
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2)`, hi, lo); err != nil {
			return fmt.Errorf("store: placement lock: %w", err)
		}
		return fn()
	})
}

// placementLockID namespaces our advisory locks so they cannot collide with
// anything else in the database that also picked a number.
const placementLockID = 0x6c656d75 // "lemu"

// placementLockKey hashes a workspace id into the two int32s the two-argument
// advisory lock form takes. Collisions are harmless: two workspaces sharing a
// key serialise against each other unnecessarily, which costs a little
// concurrency and no correctness.
func placementLockKey(workspaceID string) (int32, int32) {
	sum := sha256.Sum256([]byte(workspaceID))
	return placementLockID, int32(binary.BigEndian.Uint32(sum[:4]))
}
