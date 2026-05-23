-- name: UpsertCell :one
INSERT INTO cell (id, region, dsn_secret)
VALUES ($1, $2, $3)
ON CONFLICT (id) DO UPDATE SET region = excluded.region, dsn_secret = excluded.dsn_secret
RETURNING *;

-- name: GetCell :one
SELECT * FROM cell WHERE id = $1;

-- name: ListCells :many
SELECT * FROM cell ORDER BY id;

-- name: CreateTenant :one
INSERT INTO tenant (id, cell_id, slug, name, is_personal, plan)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetTenant :one
SELECT * FROM tenant WHERE id = $1;

-- The {org} path segment resolves through here. A slug is what a user types, so
-- this is on every org-scoped request.
-- name: GetTenantBySlug :one
SELECT * FROM tenant WHERE slug = $1;

-- Names for a set of ids. The organizations a user belongs to are known in the
-- CELL (org_member) while their names live here, so the switcher is one query
-- per tier rather than a join -- the two tiers are separate databases by
-- design, and a join across them does not exist to be written.
-- name: GetTenantsByIDs :many
SELECT * FROM tenant WHERE id = ANY ($1::uuid[]) ORDER BY created_at;

-- Naming an organization properly needs an email, and an access token minted
-- for an API resource carries none -- it arrives later, with the ID token. So
-- sign-up writes a placeholder and this replaces it, ONCE: name_generated is
-- what keeps a later rename from being silently overwritten by a re-login.
-- name: NameTenant :execrows
UPDATE tenant
SET name = $2, name_generated = false
WHERE id = $1
  AND name_generated;

-- Resolve an authenticated principal to their HOME organization and the cell
-- holding it.
--
-- The two-step request path starts here: nothing tenant-scoped can be queried
-- until this has answered, because the answer names the database to ask. It
-- returns one organization, not all of them -- the rest are memberships, and
-- memberships live in the cell this row points at.
-- name: LookupPrincipal :one
SELECT t.id AS tenant_id, t.slug, t.name, t.cell_id, t.plan, c.dsn_secret, c.region
FROM directory d
         JOIN tenant t ON t.id = d.tenant_id
         JOIN cell c ON c.id = t.cell_id
WHERE d.idp_issuer = $1
  AND d.idp_subject = $2;

-- Bind a principal to their home organization.
--
-- DO NOTHING rather than DO UPDATE: two concurrent first requests from the same
-- new user must converge on ONE organization, and whichever lost the race has
-- to adopt the winner's rather than overwrite it. The caller re-reads. The
-- loser's freshly created tenant row is then orphaned and gets deleted, which
-- is why this returns the row count rather than the row.
-- name: BindPrincipal :execrows
INSERT INTO directory (idp_issuer, idp_subject, tenant_id)
VALUES ($1, $2, $3)
ON CONFLICT (idp_issuer, idp_subject) DO NOTHING;

-- Only ever called to clean up after losing the race above, and only for a
-- tenant this transaction just created and then failed to bind.
-- name: DeleteTenant :exec
DELETE FROM tenant WHERE id = $1;

-- name: CreateInviteCode :one
INSERT INTO invite_code (code, tenant_id, role, expires_at, max_uses)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetInviteCode :one
SELECT * FROM invite_code WHERE code = $1;

-- Redeem in one statement so the use count cannot be checked and then
-- incremented by two callers at once. Zero rows back means expired, exhausted,
-- or never existed -- all of which are "this code does not work", and the caller
-- deliberately cannot tell them apart.
-- name: RedeemInviteCode :one
UPDATE invite_code
SET uses = uses + 1
WHERE code = $1
  AND (expires_at IS NULL OR expires_at > now())
  AND (max_uses IS NULL OR uses < max_uses)
RETURNING *;

-- name: DeleteInviteCode :exec
DELETE FROM invite_code WHERE code = $1;
