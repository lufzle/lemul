-- The global directory: routing, and nothing else.
--
-- A cell is at minimum its own database, so "which cell holds this tenant"
-- cannot itself live inside a cell -- you would need the answer to find the
-- place that has it. This tier exists to break that circle and is deliberately
-- kept as small as possible.
--
-- What is NOT here is the point. The full user row lives in the cell, so every
-- foreign key stays inside one database: workspace_member.user_id -> app_user.id
-- is a real constraint rather than a cross-database reference nothing enforces.
-- It also makes promoting a tenant to a dedicated cell a dump plus one row
-- update here, instead of a migration that splits records across two tiers.
--
-- No RLS on this tier. RLS separates tenants from each other inside a cell; the
-- directory is read by the control plane BEFORE it knows which tenant is asking,
-- so there is no principal to scope it to. It is operator data.
--
-- A tenant is an ORGANIZATION to everyone outside this code. The internal name
-- stays because it is what the isolation mechanism is named after -- RLS scopes
-- on tenant_id, and renaming the column would make every policy read as if it
-- enforced something else.
--
-- A user belongs to MANY organizations (org_member, in the cell). What the
-- directory still maps one-to-one is a principal to their HOME organization --
-- the one created for them at sign-up -- and through it to a cell. The
-- invariant that keeps one directory row sufficient: every organization a user
-- belongs to lives in the same cell as their home organization, which invite
-- redemption enforces. Without it a user's memberships could span cells and no
-- single lookup could answer "which database do I ask".

CREATE TABLE IF NOT EXISTS cell (
    id         text PRIMARY KEY,
    region     text NOT NULL,
    -- A reference to a secret, never the DSN. This table is the one an operator
    -- is most likely to SELECT * from while debugging routing.
    dsn_secret text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS tenant (
    id      uuid PRIMARY KEY,
    cell_id text NOT NULL REFERENCES cell (id),
    -- What a user types: `lem --org forty-crimson-swallow`, and the {org}
    -- segment of every org-scoped URL. Generated (internal/orgslug) rather than
    -- derived from the name, because a name is display text -- it has
    -- apostrophes and spaces, it is not unique, and it changes.
    slug    text NOT NULL UNIQUE,
    -- Display only. "Ada's Org".
    name    text NOT NULL,
    -- True until someone renames the organization. Sign-up cannot name it
    -- properly: an access token minted for an API resource carries no identity
    -- claims, so the email arrives later with the ID token (PUT /v1/identity),
    -- and this is what says the placeholder is still safe to overwrite.
    name_generated boolean NOT NULL DEFAULT true,
    -- The organization created FOR a user at sign-up. They cannot leave it and
    -- it cannot be deleted while they exist, so there is always somewhere for
    -- their personal workspace to live.
    is_personal    boolean NOT NULL DEFAULT false,
    -- 'individual' is what a self-serve sign-up with no invite code gets.
    plan       text        NOT NULL DEFAULT 'individual'
        CHECK (plan IN ('individual', 'team', 'dedicated')),
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Which cell holds this principal, and which organization is their own. A thin
-- index, written once when a user first appears.
--
-- Keyed on (issuer, subject) rather than subject alone because each tenant may
-- bring its own identity provider: a subject is unique only WITHIN an issuer,
-- so a subject-only key would collide the day the second one arrives with their
-- own Okta.
--
-- tenant_id is the HOME organization, not "the" organization. Memberships live
-- in the cell; this row exists to answer "which database" without already
-- knowing the answer.
CREATE TABLE IF NOT EXISTS directory (
    idp_issuer  text        NOT NULL,
    idp_subject text        NOT NULL,
    tenant_id   uuid        NOT NULL REFERENCES tenant (id) ON DELETE CASCADE,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (idp_issuer, idp_subject)
);

-- Invite codes add an EXISTING user to an existing organization. They no longer
-- decide which tenant a sign-up lands in: every user gets their own
-- organization at sign-up regardless, so redemption is a membership insert and
-- there is no ordering to get wrong.
--
-- The role is the one the redeemer gets in that organization, and it is the
-- same vocabulary as org_member.role in the cell.
CREATE TABLE IF NOT EXISTS invite_code (
    code       text PRIMARY KEY,
    tenant_id  uuid NOT NULL REFERENCES tenant (id) ON DELETE CASCADE,
    role       text NOT NULL DEFAULT 'user'
        CHECK (role IN ('owner', 'user')),
    expires_at timestamptz,
    -- NULL means unlimited. Checked against uses at redemption.
    max_uses   integer,
    uses       integer     NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now()
);
