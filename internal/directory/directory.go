// Package directory is the global tier: which cell holds which principal, and
// which organization is their own.
//
// It exists to break one circle. A cell is at minimum its own database, so
// "which cell do I ask" cannot be answered by asking a cell. Everything else is
// deliberately kept out -- the full user row, memberships, workspaces and
// sessions all live in the cell, which is what keeps every foreign key inside
// one database.
//
// No RLS here, and no InTenant to enforce it. This tier is read BEFORE the
// control plane knows which organization is asking, so there is no principal to
// scope it to; it is operator data, and the corresponding guarantee is that
// nothing tenant-owned is stored in it.
package directory

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lufzle/lemul/internal/directory/directorydb"
	"github.com/lufzle/lemul/internal/orgslug"
)

//go:embed schema.sql
var schemaSQL string

// SchemaSQL is exported for tests, which apply it as an owner -- the
// application role deliberately holds no DDL privileges.
var SchemaSQL = schemaSQL

// ErrNotFound is what a missing row looks like to callers, so they do not have
// to know that pgx spells it pgx.ErrNoRows.
var ErrNotFound = pgx.ErrNoRows

// Directory owns a connection pool for the global tier.
type Directory struct {
	pool *pgxpool.Pool
}

// Open connects. Unlike store.Open there is no BYPASSRLS assertion, because
// there are no policies here for a privileged role to see through.
func Open(ctx context.Context, dsn string) (*Directory, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("directory: connect: %w", err)
	}
	return &Directory{pool: pool}, nil
}

// OpenPool wraps an existing pool. Tests use it to share one container.
func OpenPool(pool *pgxpool.Pool) *Directory { return &Directory{pool: pool} }

func (d *Directory) Close() { d.pool.Close() }

// ApplySchema creates the tables on a DDL-capable connection. Idempotent, and
// an operator step: see store.ApplySchema for why the application role cannot
// do this itself.
func ApplySchema(ctx context.Context, dsn string) error {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("directory: connect for schema: %w", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("directory: apply schema: %w", err)
	}
	return nil
}

// Tx runs fn inside a transaction.
//
// The counterpart to store.InTenant, minus the scoping -- which is the whole
// difference between the tiers, and the reason this is a separate function
// rather than a shared helper that takes an optional tenant.
func (d *Directory) Tx(ctx context.Context, fn func(*directorydb.Queries) error) error {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("directory: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(directorydb.New(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("directory: commit: %w", err)
	}
	return nil
}

// Q runs fn against the pool without a transaction, for single reads.
func (d *Directory) Q(ctx context.Context, fn func(*directorydb.Queries) error) error {
	return fn(directorydb.New(d.pool))
}

// Org is one organization as the rest of the program thinks of it. It exists so
// that "tenant", the internal name the isolation mechanism is built on, does
// not leak into handlers and responses that mean Organization.
type Org struct {
	ID     string
	Slug   string
	Name   string
	Plan   string
	CellID string
	// Personal is the organization created for a user at sign-up. They cannot
	// leave it, which is what guarantees there is always somewhere their
	// personal workspace can live.
	Personal bool
}

func orgOf(t directorydb.Tenant) Org {
	return Org{
		ID: t.ID, Slug: t.Slug, Name: t.Name,
		Plan: t.Plan, CellID: t.CellID, Personal: t.IsPersonal,
	}
}

// Home is a principal's home organization plus the cell holding it: everything
// the two-step request path needs from this tier.
type Home struct {
	Org
	DSNSecret string
	Region    string
}

// LookupHome resolves an authenticated principal. ErrNotFound means they have
// never signed in, which is the sign-up path rather than an error.
func (d *Directory) LookupHome(ctx context.Context, issuer, subject string) (Home, error) {
	var out Home
	err := d.Q(ctx, func(q *directorydb.Queries) error {
		row, err := q.LookupPrincipal(ctx, directorydb.LookupPrincipalParams{
			IdpIssuer: issuer, IdpSubject: subject,
		})
		if err != nil {
			return err
		}
		out = Home{
			Org: Org{
				ID: row.TenantID, Slug: row.Slug, Name: row.Name,
				Plan: row.Plan, CellID: row.CellID,
			},
			DSNSecret: row.DsnSecret,
			Region:    row.Region,
		}
		return nil
	})
	return out, err
}

// OrgBySlug resolves the {org} path segment.
func (d *Directory) OrgBySlug(ctx context.Context, slug string) (Org, error) {
	var out Org
	err := d.Q(ctx, func(q *directorydb.Queries) error {
		t, err := q.GetTenantBySlug(ctx, slug)
		out = orgOf(t)
		return err
	})
	return out, err
}

// OrgByID is how a membership row, which knows only an id, becomes something
// with a name.
func (d *Directory) OrgByID(ctx context.Context, id string) (Org, error) {
	var out Org
	err := d.Q(ctx, func(q *directorydb.Queries) error {
		t, err := q.GetTenant(ctx, id)
		out = orgOf(t)
		return err
	})
	return out, err
}

// Orgs resolves a set of ids, for the organization switcher. Returned in the
// order the database gives them (creation order), not the order asked for.
func (d *Directory) Orgs(ctx context.Context, ids []string) ([]Org, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var out []Org
	err := d.Q(ctx, func(q *directorydb.Queries) error {
		rows, err := q.GetTenantsByIDs(ctx, ids)
		if err != nil {
			return err
		}
		out = make([]Org, 0, len(rows))
		for _, t := range rows {
			out = append(out, orgOf(t))
		}
		return nil
	})
	return out, err
}

// slugAttempts bounds the retry on a slug collision.
//
// Ten is not a tuned number; it is "so many that a failure means the keyspace
// is exhausted rather than that we were unlucky". At ~130k combinations, ten
// consecutive collisions is not something that happens to a deployment with
// room left in it.
const slugAttempts = 10

// CreateOrg makes an organization with a generated slug, retrying past a
// collision. The name is a placeholder for a personal organization: sign-up has
// no email to build one from, because an access token minted for an API
// resource carries no identity claims. NameOrg fixes it when the ID token
// arrives.
func (d *Directory) CreateOrg(ctx context.Context, id, cellID, name string, personal bool) (Org, error) {
	var out Org
	for range slugAttempts {
		slug := orgslug.Generate()
		// An empty name means "no better idea yet", and the slug is the best
		// placeholder there is -- it is at least the thing the user types.
		//
		// Set HERE rather than through NameOrg afterwards, and that is not a
		// style choice: NameOrg spends `name_generated`, which is the one-shot
		// permission to replace a placeholder. Naming the organization after its
		// own slug through NameOrg therefore made the real name unreachable
		// forever, so every organization stayed called `forty-crimson-windmill`
		// no matter how many times its owner signed in.
		placeholder := name
		if placeholder == "" {
			placeholder = slug
		}
		err := d.Tx(ctx, func(q *directorydb.Queries) error {
			t, err := q.CreateTenant(ctx, directorydb.CreateTenantParams{
				ID: id, CellID: cellID, Slug: slug,
				Name: placeholder, IsPersonal: personal, Plan: "individual",
			})
			out = orgOf(t)
			return err
		})
		if err == nil {
			return out, nil
		}
		if !IsUniqueViolation(err) {
			return Org{}, fmt.Errorf("directory: create organization: %w", err)
		}
	}
	return Org{}, fmt.Errorf("directory: no free slug after %d attempts", slugAttempts)
}

// IsUniqueViolation distinguishes "someone else already inserted that" from
// every other way an insert can fail. Matching on the SQLSTATE rather than on
// the message, which is localised and version-dependent.
//
// Exported because the same distinction decides a retry in the cell: two
// concurrent first requests race on app_user's UNIQUE (idp_issuer,
// idp_subject), and the loser has to re-read rather than fail.
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// Bind records a principal's home organization, and reports whether this caller
// is the one that established it.
//
// False means a concurrent first request from the same principal won, and the
// caller must adopt that organization and discard the one it just created --
// two sign-ups converging on two organizations would give one person two homes
// and no way to say which is theirs.
func (d *Directory) Bind(ctx context.Context, issuer, subject, orgID string) (bool, error) {
	var won bool
	err := d.Tx(ctx, func(q *directorydb.Queries) error {
		n, err := q.BindPrincipal(ctx, directorydb.BindPrincipalParams{
			IdpIssuer: issuer, IdpSubject: subject, TenantID: orgID,
		})
		won = n > 0
		return err
	})
	return won, err
}

// DeleteOrg drops an organization. Only used to clean up the loser of the race
// in Bind, on a row this process created moments earlier and nothing else has
// seen.
func (d *Directory) DeleteOrg(ctx context.Context, id string) error {
	return d.Tx(ctx, func(q *directorydb.Queries) error {
		return q.DeleteTenant(ctx, id)
	})
}

// NameOrg gives a placeholder organization its real display name, once.
// Reports false if it already had one, which is not an error: a user signing in
// again should not undo a rename.
func (d *Directory) NameOrg(ctx context.Context, id, name string) (bool, error) {
	var renamed bool
	err := d.Tx(ctx, func(q *directorydb.Queries) error {
		n, err := q.NameTenant(ctx, directorydb.NameTenantParams{ID: id, Name: name})
		renamed = n > 0
		return err
	})
	return renamed, err
}

// EnsureCell registers a cell. Idempotent, and an operator concern -- there is
// exactly one today, and which database it names is deployment configuration.
func (d *Directory) EnsureCell(ctx context.Context, id, region, dsnSecret string) error {
	return d.Tx(ctx, func(q *directorydb.Queries) error {
		_, err := q.UpsertCell(ctx, directorydb.UpsertCellParams{
			ID: id, Region: region, DsnSecret: dsnSecret,
		})
		return err
	})
}

// Invite is an invitation to join an organization.
type Invite struct {
	Code  string
	OrgID string
	Role  string
}

// CreateInvite issues a code.
func (d *Directory) CreateInvite(ctx context.Context, code, orgID, role string, expiresAt *time.Time, maxUses *int32) (Invite, error) {
	var out Invite
	err := d.Tx(ctx, func(q *directorydb.Queries) error {
		iv, err := q.CreateInviteCode(ctx, directorydb.CreateInviteCodeParams{
			Code: code, TenantID: orgID, Role: role,
			ExpiresAt: expiresAt, MaxUses: maxUses,
		})
		out = Invite{Code: iv.Code, OrgID: iv.TenantID, Role: iv.Role}
		return err
	})
	return out, err
}

// RedeemInvite consumes one use and reports which organization and role it
// grants.
//
// The check and the increment are one statement, so two callers cannot both
// pass a max_uses of 1. ErrNotFound covers expired, exhausted and never-existed
// alike, and the caller deliberately cannot tell them apart -- distinguishing
// them turns a code into an oracle for which codes exist.
func (d *Directory) RedeemInvite(ctx context.Context, code string) (Invite, error) {
	var out Invite
	err := d.Tx(ctx, func(q *directorydb.Queries) error {
		iv, err := q.RedeemInviteCode(ctx, code)
		out = Invite{Code: iv.Code, OrgID: iv.TenantID, Role: iv.Role}
		return err
	})
	return out, err
}
