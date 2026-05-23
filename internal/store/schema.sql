-- One cell's schema. Every table is tenant-scoped and every table has RLS.
--
-- Two mechanisms guard tenancy, deliberately:
--
--   1. Queries carry an explicit tenant_id predicate.
--   2. RLS policies enforce the same thing at the database.
--
-- Belt and braces, and not redundant. RLS alone makes the isolation INVISIBLE:
-- a reviewer reading ListWorkspaces sees no tenancy at all, and the generated Go
-- has no tenant parameter, so a missing SET LOCAL would silently widen every
-- query in the process. The predicate makes the intent auditable at the call
-- site; the policy makes it true even if someone forgets.

-- FORCE, not just ENABLE.
--
-- ENABLE exempts the table OWNER, which is normally the role that ran the
-- migration -- and if the application connects as that role, every policy below
-- is silently inert. That is the single most common way an RLS deployment turns
-- out to have been decorative. FORCE removes the exemption, and the app role is
-- separately required not to hold BYPASSRLS (see AssertNoBypassRLS).

-- A user belongs to the CELL, not to one organization.
--
-- This is the shape a user being in many organizations forces. A tenant_id
-- column here would have said the opposite, and the global UNIQUE below would
-- then have made "the same person in two organizations" unrepresentable rather
-- than merely unimplemented.
--
-- home_tenant_id is the organization created for them at sign-up. It is not a
-- membership -- there is a real org_member row for it too -- but it is the
-- default a client resolves to and the one organization they cannot leave.
CREATE TABLE IF NOT EXISTS app_user (
    id             uuid PRIMARY KEY,
    home_tenant_id uuid NOT NULL,
    idp_issuer     text NOT NULL,
    idp_subject    text NOT NULL,
    -- Captured from a validated ID token, never from a request body. NULL until
    -- the user has signed in somewhere that issues one.
    email          text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (idp_issuer, idp_subject)
);

-- Membership of an organization, and the role in it.
--
-- 'owner' is what was called the console role's 'admin'. Naming it owner keeps
-- one vocabulary for "may administer this thing" across both scopes an object
-- can be owned in -- an organization and a workspace -- rather than two words
-- for one idea. The two are independent: a plain member of an organization can
-- own a workspace inside it.
CREATE TABLE IF NOT EXISTS org_member (
    tenant_id  uuid NOT NULL,
    user_id    uuid NOT NULL REFERENCES app_user (id) ON DELETE CASCADE,
    role       text NOT NULL CHECK (role IN ('owner', 'user')),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, user_id)
);

CREATE TABLE IF NOT EXISTS workspace (
    id            uuid PRIMARY KEY,
    tenant_id     uuid NOT NULL,
    name          text NOT NULL,
    owner_user_id uuid    NOT NULL REFERENCES app_user (id) ON DELETE RESTRICT,
    -- Who may use this workspace, which is NOT the same question as who
    -- administers it (that is workspace_member.role).
    --
    -- This reinstates something the comment below says membership replaced, so
    -- it needs its reason stated rather than left as a contradiction. 'org' was
    -- dropped because it "would have needed a user list to evaluate anyway" --
    -- true when a workspace was one developer's sandbox and the question was
    -- "who is in this set". It is no longer the question. Every request is
    -- already scoped to an organization before a row is read (the {org} path
    -- segment, section 2.6), so 'org' evaluates to the membership check that has
    -- ALREADY happened, and costs nothing. 'team' is still not here, because it
    -- still names an entity that does not exist.
    --
    --   owner   -- only the owner, plus organization owners
    --   members -- explicit workspace_member rows, which is what the console's
    --              members tab manages
    --   org     -- anybody in the organization
    access_scope  text NOT NULL DEFAULT 'owner'
        CONSTRAINT workspace_access_scope_valid
            CHECK (access_scope IN ('owner', 'members', 'org')),
    -- The high-water mark for member uids in this workspace.
    --
    -- A counter rather than MAX(workspace_member.uid) + 1, and the difference is
    -- not cosmetic: removing a member deletes their row, so a MAX would drop
    -- back and hand the next person that member's number -- along with the home
    -- directory still sitting on the volume under it. Files outlive membership,
    -- so the allocator has to as well.
    --
    -- Same shape as generation below, for the same reason: incremented with
    -- UPDATE ... RETURNING so no read-modify-write can race.
    next_uid      integer NOT NULL DEFAULT 2000,
    -- Feeds the ECS RunTask client-token, which is why it is the one field that
    -- genuinely needs durability (section 2.8). Incremented with
    -- `SET generation = generation + 1 ... RETURNING`, which makes the
    -- read-modify-write race that broke it once structurally impossible.
    generation    bigint  NOT NULL DEFAULT 0,
    task_ref      text,
    -- Deliberately NOT carrying 'warm_hold', which section 2.4 names as a
    -- compute state. Every comparison against this column would have to learn
    -- the new value, and missing one is silent. Warm hold is derivable instead:
    -- status 'active', no running sessions, warm_hold_until set.
    status        text    NOT NULL DEFAULT 'stopped'
        CHECK (status IN ('stopped', 'starting', 'active')),
    -- Why the LAST placement attempt failed, or NULL if the last one did not.
    --
    -- It exists because placement stopped being something a caller waits for.
    -- While create blocked until the tunnel was up, a failure was the response
    -- body and the client read it; a background placement has nobody left to
    -- answer, so without this column the failure is a log line in a service the
    -- customer cannot see, and the row simply never becomes active.
    --
    -- Scoped to ONE attempt on purpose: every status write clears it and only a
    -- failed placement sets it (queries.sql). A message that outlived the
    -- attempt it describes would be read as the reason the CURRENT state is
    -- what it is, which is exactly what it would not be.
    last_error    text,
    -- Section 2.4's auto-stop cascade. Since Phase 6 these are not billing
    -- hygiene: a workspace is a shared machine, so an OOM kills the task and
    -- with it EVERY member's sessions.
    --
    -- Default 7200 (2 h), and on by default -- decision #7 settles that
    -- auto-stop is on with a per-session opt-out. 0 disables it here.
    idle_timeout_secs integer NOT NULL DEFAULT 7200
        CONSTRAINT workspace_idle_timeout_valid CHECK (idle_timeout_secs >= 0),
    -- How long the task stays up after its LAST session stops, so somebody who
    -- quit and immediately reconnected does not pay a cold start (section 2.4).
    --
    -- 3600 (1 h), raised from 300 on 2026-08-03. Five minutes was sized for a
    -- workspace as ONE developer's sandbox; since Phase 6 it is a shared
    -- machine, so the hold is amortised across a team rather than spent on the
    -- person who happened to quit last, and anyone stepping away from their
    -- desk came back to a cold start. The measurements are the argument: warm
    -- attach is 1.7 s against 22.6 s cold, and a cold start that has to restore
    -- an EBS snapshot is nearer 50 s.
    --
    -- It is the expensive default in this table -- an idle 2 vCPU / 8 GiB task
    -- is roughly $0.09/h -- which is why it stays per-workspace overridable.
    warm_hold_secs integer NOT NULL DEFAULT 3600
        CONSTRAINT workspace_warm_hold_valid CHECK (warm_hold_secs >= 0),
    -- When the warm hold expires. A COLUMN rather than a timer in the control
    -- plane's memory, and that is the whole point: a time.AfterFunc dies with
    -- the process, so a redeploy mid-hold would leave the task running with
    -- nobody timing it -- the exact failure warm hold exists to prevent, made
    -- permanent. NULL means no hold is running.
    warm_hold_until timestamptz,
    -- The EBS snapshot a replacement task restores its disk from, or NULL if
    -- this workspace has never been stopped with anything on it.
    --
    -- It exists because ECS attaches ONE volume per task, it must be a NEW
    -- volume, and there is no re-attach: the only way a workspace's disk
    -- outlives its task is to snapshot the volume and create the next one from
    -- the snapshot. So this column IS the workspace's filesystem between tasks,
    -- and losing it loses every member's home and conversation.
    --
    -- Which is why it is written only AFTER a snapshot completes, never
    -- optimistically, and why the previous snapshot is deleted only after this
    -- column names the new one. A crash between the two leaks a snapshot, which
    -- costs money; the other order loses the workspace.
    snapshot_id   text,
    -- When this workspace would have stopped, but a background process in some
    -- session is holding it open (section 2.4). NULL means it is not in that
    -- state.
    --
    -- It is NOT a reason to stop anything. A member who left a dev server
    -- running did so on purpose, and reaping the session would kill it -- so
    -- the behaviour is to keep it alive and make the cost VISIBLE, which is
    -- what this column is for. Nobody goes home on Friday intending to bill all
    -- weekend; they just cannot see that they are.
    --
    -- A timestamp rather than a boolean so the console can say how long, which
    -- is the part that makes it actionable. It is also the ceiling this would
    -- need if it ever grows one: now() - idle_pinned_since > N.
    idle_pinned_since timestamptz,
    -- Admission control (section 2.4). Fargate task size is fixed at launch, so
    -- the control plane refuses rather than letting an OOM be the discovery
    -- mechanism.
    --
    --   unlimited            -- the user's call; warned about at create time
    --   max_sessions         -- a simple count; predictable, ignores real load
    --   min_free_memory_pct  -- admit while the supervisor reports that much of
    --                           the task's limit free
    --
    -- A PERCENTAGE rather than the absolute min_free_memory_mb section 2.4
    -- first recorded: an absolute floor is wrong on every tier but the one it
    -- was chosen for. It is paired with a constant absolute floor in the code,
    -- because a Claude Code session's footprint does not scale with task size
    -- (see mgmtapi/admission.go).
    admission_policy text NOT NULL DEFAULT 'min_free_memory_pct'
        CONSTRAINT workspace_admission_policy_valid
            CHECK (admission_policy IN ('unlimited', 'max_sessions', 'min_free_memory_pct')),
    max_sessions integer NOT NULL DEFAULT 4
        CONSTRAINT workspace_max_sessions_valid CHECK (max_sessions > 0),
    min_free_memory_pct integer NOT NULL DEFAULT 20
        CONSTRAINT workspace_min_free_pct_valid
            CHECK (min_free_memory_pct BETWEEN 0 AND 100),
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name)
);

-- Explicit membership of one workspace. This is what access_scope = 'members'
-- reads, and it carries the role either way -- a workspace can be readable by
-- the whole organization and still have exactly one owner.
CREATE TABLE IF NOT EXISTS workspace_member (
    workspace_id uuid NOT NULL REFERENCES workspace (id) ON DELETE CASCADE,
    user_id      uuid NOT NULL REFERENCES app_user (id) ON DELETE CASCADE,
    -- Denormalised so RLS can scope this table without a join. A policy that
    -- had to join to workspace would run on every row of every query here.
    tenant_id    uuid NOT NULL,
    role         text NOT NULL CHECK (role IN ('owner', 'user')),
    -- The POSIX uid this member's sessions run at inside the workspace task.
    --
    -- Allocated here rather than derived, because it has to survive a
    -- replacement task: the volume persists across placements while the
    -- container filesystem does not, so a uid recomputed per task would leave
    -- every home owned by a stranger. Hashing a user uuid into a range would
    -- collide silently, and the symptom of a collision is one member reading
    -- another's home -- so it is allocated, unique within the workspace, and
    -- never reused.
    --
    -- From 2000 up: the image's `node` user is 1000 (LEMUL_SESSION_UID) and
    -- everything below that is system.
    --
    -- Nullable because it is filled lazily on first session, including for
    -- access_scope = 'org' workspaces, which have no row here until somebody
    -- actually starts something.
    uid          integer
        CONSTRAINT workspace_member_uid_range CHECK (uid IS NULL OR uid >= 2000),
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, user_id),
    -- Multiple NULLs are permitted by a UNIQUE constraint, which is exactly
    -- what is wanted: many members with no uid yet, never two with the same one.
    CONSTRAINT workspace_member_uid_unique UNIQUE (workspace_id, uid)
);

CREATE TABLE IF NOT EXISTS session (
    -- IS Claude Code's conversation id, not a key mapped to one (section 12.7).
    -- That is why it is a uuid: --session-id requires one, and adopting the
    -- format means resume needs no lookup table.
    id           uuid PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES workspace (id) ON DELETE CASCADE,
    tenant_id    uuid NOT NULL,
    -- Required by the listing rules: a workspace owner sees every session, a
    -- member sees only their own, and neither is expressible without this.
    user_id      uuid        NOT NULL REFERENCES app_user (id) ON DELETE CASCADE,
    -- Section 2.4's per-session idle override. NULL inherits the workspace's,
    -- which is the common case; 0 is the explicit opt-out that section 2.4
    -- requires, for the user who knows their session is meant to sit there.
    --
    -- Nullable rather than defaulted, because "not set" and "set to the same
    -- number the workspace happens to use today" are different: the first
    -- follows a later change to the workspace, and the second does not.
    idle_timeout_secs integer
        CONSTRAINT session_idle_timeout_valid
            CHECK (idle_timeout_secs IS NULL OR idle_timeout_secs >= 0),
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- Per-organization settings, one row per organization AT MOST.
--
-- Absence means defaults. That is deliberate: the alternative is writing a row
-- during sign-up, which puts a second write on the one path that already has
-- the most ordering constraints in this schema (see app_user's policy above).
-- Readers fall back to the defaults; a write upserts.
--
-- In the CELL, not the directory. The directory is deliberately tiny -- it
-- exists only to answer "which cell holds this principal" and break the circle
-- of asking a cell which cell to ask -- and product configuration in the global
-- tier would make every settings read a cross-tier one.
CREATE TABLE IF NOT EXISTS org_settings (
    tenant_id  uuid PRIMARY KEY,
    -- Default FALSE: only organization owners create workspaces.
    --
    -- The setting an operator actually wants is a permission, not a mechanism.
    -- Gating only the implicit personal-workspace path would leave any member
    -- able to create a private workspace by naming one, which is the same
    -- outcome by another route and therefore no bound on task count at all.
    members_can_create_workspaces boolean NOT NULL DEFAULT false,
    updated_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE org_settings ENABLE ROW LEVEL SECURITY;
ALTER TABLE org_settings FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON org_settings;
CREATE POLICY tenant_isolation ON org_settings
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- Columns added after the tables above first shipped.
--
-- This file is applied whole and is the only migration mechanism there is, so a
-- new column has to appear in BOTH places: in the CREATE TABLE, for an empty
-- database, and here, for one that already exists. Without the second,
-- -apply-schema silently no-ops against a developer's warm database and the
-- first query naming the column is what reports it.
-- The constraints are named and re-applied rather than inlined only above,
-- because ADD COLUMN carries no CHECK: a migrated database would otherwise hold
-- the column WITHOUT its constraint while a fresh one holds both, and the two
-- would disagree about what is storable. Dropping by name first makes this
-- idempotent on either.
ALTER TABLE workspace ADD COLUMN IF NOT EXISTS access_scope text NOT NULL DEFAULT 'owner';
-- Retired with the personal workspace itself: "a workspace with access_scope =
-- owner" says the same thing without a second concept, a reserved name prefix
-- and a creation path that conjured a task from a command with no arguments.
-- Dropping the column drops its CHECK with it.
ALTER TABLE workspace DROP COLUMN IF EXISTS is_personal;
ALTER TABLE workspace ADD COLUMN IF NOT EXISTS next_uid integer NOT NULL DEFAULT 2000;
ALTER TABLE workspace DROP CONSTRAINT IF EXISTS workspace_access_scope_valid;
ALTER TABLE workspace ADD CONSTRAINT workspace_access_scope_valid
    CHECK (access_scope IN ('owner', 'members', 'org'));

-- Section 2.4's lifecycle policy, added in Phase 6 increment 3.
ALTER TABLE workspace ADD COLUMN IF NOT EXISTS idle_timeout_secs integer NOT NULL DEFAULT 7200;
ALTER TABLE workspace DROP CONSTRAINT IF EXISTS workspace_idle_timeout_valid;
ALTER TABLE workspace ADD CONSTRAINT workspace_idle_timeout_valid
    CHECK (idle_timeout_secs >= 0);
ALTER TABLE workspace ADD COLUMN IF NOT EXISTS warm_hold_secs integer NOT NULL DEFAULT 3600;
-- ADD COLUMN IF NOT EXISTS is a no-op once the column exists, DEFAULT included,
-- so raising the number above would silently leave every already-migrated
-- database on 300. This is the line that actually converges them.
ALTER TABLE workspace ALTER COLUMN warm_hold_secs SET DEFAULT 3600;
ALTER TABLE workspace DROP CONSTRAINT IF EXISTS workspace_warm_hold_valid;
ALTER TABLE workspace ADD CONSTRAINT workspace_warm_hold_valid
    CHECK (warm_hold_secs >= 0);
ALTER TABLE workspace ADD COLUMN IF NOT EXISTS warm_hold_until timestamptz;
ALTER TABLE workspace ADD COLUMN IF NOT EXISTS idle_pinned_since timestamptz;
ALTER TABLE workspace ADD COLUMN IF NOT EXISTS admission_policy text NOT NULL DEFAULT 'min_free_memory_pct';
ALTER TABLE workspace DROP CONSTRAINT IF EXISTS workspace_admission_policy_valid;
ALTER TABLE workspace ADD CONSTRAINT workspace_admission_policy_valid
    CHECK (admission_policy IN ('unlimited', 'max_sessions', 'min_free_memory_pct'));
ALTER TABLE workspace ADD COLUMN IF NOT EXISTS max_sessions integer NOT NULL DEFAULT 4;
ALTER TABLE workspace DROP CONSTRAINT IF EXISTS workspace_max_sessions_valid;
ALTER TABLE workspace ADD CONSTRAINT workspace_max_sessions_valid
    CHECK (max_sessions > 0);
ALTER TABLE workspace ADD COLUMN IF NOT EXISTS min_free_memory_pct integer NOT NULL DEFAULT 20;
ALTER TABLE workspace DROP CONSTRAINT IF EXISTS workspace_min_free_pct_valid;
ALTER TABLE workspace ADD CONSTRAINT workspace_min_free_pct_valid
    CHECK (min_free_memory_pct BETWEEN 0 AND 100);

-- Phase 6 increment 4: placement happens on create, in the background, so a
-- failure has no request left to be reported on.
ALTER TABLE workspace ADD COLUMN IF NOT EXISTS last_error text;

ALTER TABLE session ADD COLUMN IF NOT EXISTS idle_timeout_secs integer;
ALTER TABLE session DROP CONSTRAINT IF EXISTS session_idle_timeout_valid;
ALTER TABLE session ADD CONSTRAINT session_idle_timeout_valid
    CHECK (idle_timeout_secs IS NULL OR idle_timeout_secs >= 0);

ALTER TABLE workspace_member ADD COLUMN IF NOT EXISTS uid integer;
ALTER TABLE workspace_member DROP CONSTRAINT IF EXISTS workspace_member_uid_range;
ALTER TABLE workspace_member ADD CONSTRAINT workspace_member_uid_range
    CHECK (uid IS NULL OR uid >= 2000);
ALTER TABLE workspace_member DROP CONSTRAINT IF EXISTS workspace_member_uid_unique;
ALTER TABLE workspace_member ADD CONSTRAINT workspace_member_uid_unique
    UNIQUE (workspace_id, uid);

CREATE INDEX IF NOT EXISTS session_by_workspace ON session (workspace_id);
CREATE INDEX IF NOT EXISTS workspace_member_by_user ON workspace_member (user_id);
CREATE INDEX IF NOT EXISTS workspace_by_owner ON workspace (owner_user_id);
-- Not decoration: app_user's policy is an EXISTS against this table, so it runs
-- on every user read. The primary key indexes (tenant_id, user_id); this covers
-- the other direction, which is how "which organizations am I in" is answered.
CREATE INDEX IF NOT EXISTS org_member_by_user ON org_member (user_id);

-- Row-level security.
--
-- current_setting(..., true) -- missing_ok -- returns NULL when the GUC has
-- never been set, and `tenant_id = NULL` is NULL rather than TRUE, so an
-- unscoped connection sees NOTHING. Fail-closed. Without missing_ok the call
-- RAISES, which is also safe but turns a missing scope into an error nobody can
-- read; returning no rows is the same guarantee with a better failure.
--
-- The nullif is not decoration. Once a custom GUC has been set in a session,
-- set_config(..., is_local => true) reverts it at transaction end to the EMPTY
-- STRING rather than to unset -- so current_setting returns '' on any pooled
-- connection that has previously carried a scope, and ''::uuid raises
-- "invalid input syntax for type uuid". Which means the failure would not have
-- appeared on a fresh connection, only on a reused one, and therefore not until
-- the pool was warm. Found by the unscoped-read test.
--
-- Both settings need it, not just app.tenant_id: org_member's policy reads
-- app.user_id, and it reverts the same way on the same pooled connections.

-- app_user is the one table that cannot use the predicate above, because a user
-- has no tenant_id -- being in many organizations is the whole point. Its
-- isolation is membership instead: inside organization A's transaction you see
-- exactly organization A's members, which is the same guarantee reached a
-- different way.
--
-- The home_tenant_id disjunct is not redundant, and removing it breaks sign-up
-- rather than merely tightening it. The user row has to be INSERTed before its
-- org_member row can exist, because org_member.user_id references app_user --
-- so at that moment the EXISTS is false and the row fails its own WITH CHECK.
-- The disjunct is what lets a user be created into the organization being
-- created for them, and nothing wider: home_tenant_id always has a matching
-- org_member row a moment later, so it grants no visibility the EXISTS does not
-- also grant afterwards.
ALTER TABLE app_user ENABLE ROW LEVEL SECURITY;
ALTER TABLE app_user FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON app_user;
CREATE POLICY tenant_isolation ON app_user
    USING (home_tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid
        OR EXISTS (SELECT 1
                   FROM org_member m
                   WHERE m.user_id = app_user.id
                     AND m.tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid))
    WITH CHECK (home_tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid
        OR EXISTS (SELECT 1
                   FROM org_member m
                   WHERE m.user_id = app_user.id
                     AND m.tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid));

-- org_member is the first table to read app.user_id, which InTenant has been
-- setting on every transaction since it existed with nothing consuming it.
--
-- The asymmetry is deliberate. READS are widened by "or it is my own
-- membership", because "which organizations am I in" is the one question that
-- is inherently cross-organization -- it is what the console's switcher and the
-- CLI's --org default are, and scoping it to one organization would mean
-- already knowing the answer. It exposes nothing but rows about yourself.
--
-- WRITES are not widened. WITH CHECK stays tenant-only, so a membership can
-- only ever be created in the organization the transaction is scoped to. That
-- is what stops "add myself to your organization" being one INSERT away from a
-- caller who can already read their own rows.
ALTER TABLE org_member ENABLE ROW LEVEL SECURITY;
ALTER TABLE org_member FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON org_member;
CREATE POLICY tenant_isolation ON org_member
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid
        OR user_id = nullif(current_setting('app.user_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE workspace ENABLE ROW LEVEL SECURITY;
ALTER TABLE workspace FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON workspace;
CREATE POLICY tenant_isolation ON workspace
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE workspace_member ENABLE ROW LEVEL SECURITY;
ALTER TABLE workspace_member FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON workspace_member;
CREATE POLICY tenant_isolation ON workspace_member
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

ALTER TABLE session ENABLE ROW LEVEL SECURITY;
ALTER TABLE session FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON session;
CREATE POLICY tenant_isolation ON session
    USING (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

-- The workspace volume (section 9 item 0). A workspace's disk outlives its task
-- as an EBS snapshot, because ECS gives a task a new volume or a volume from a
-- snapshot, and never an existing one.
ALTER TABLE workspace ADD COLUMN IF NOT EXISTS snapshot_id text;
