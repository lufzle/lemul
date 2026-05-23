-- Every query carries an explicit tenant_id predicate even though RLS enforces
-- the same thing. See schema.sql: the policy makes it true, the predicate makes
-- it visible to whoever reads the call site.
--
-- app_user is the exception, and only because it HAS no tenant_id -- a user
-- belongs to the cell and to many organizations. Its queries therefore lean on
-- the policy alone, which is worth noticing rather than skimming past: these
-- are the only reads in this file where nothing at the call site says what
-- limits them.

-- name: CreateUser :one
INSERT INTO app_user (id, home_tenant_id, idp_issuer, idp_subject, email)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetUser :one
SELECT * FROM app_user WHERE id = $1;

-- name: GetUserBySubject :one
SELECT * FROM app_user WHERE idp_issuer = $1 AND idp_subject = $2;

-- Everyone in one organization, for the owner columns the console renders.
-- Through org_member rather than over app_user, so the tenant predicate is
-- expressible here like everywhere else.
-- name: ListOrgMembers :many
SELECT u.*, m.role
FROM org_member m
         JOIN app_user u ON u.id = m.user_id
WHERE m.tenant_id = $1
ORDER BY u.created_at;

-- Only ever called with an email read from a VALIDATED ID token. An access
-- token minted for an API resource carries no identity claims, so the value has
-- to come from a signed assertion rather than from a request body.
-- name: SetUserEmail :one
UPDATE app_user SET email = $2 WHERE id = $1 RETURNING *;

-- name: AddOrgMember :one
INSERT INTO org_member (tenant_id, user_id, role)
VALUES ($1, $2, $3)
ON CONFLICT (tenant_id, user_id) DO UPDATE SET role = excluded.role
RETURNING *;

-- Redemption must not quietly demote someone who is already an owner, so it
-- does NOT overwrite an existing role -- unlike AddOrgMember, which is an
-- administrative act by someone who meant it.
-- name: JoinOrg :execrows
INSERT INTO org_member (tenant_id, user_id, role)
VALUES ($1, $2, $3)
ON CONFLICT (tenant_id, user_id) DO NOTHING;

-- The authorisation primitive for an organization: what, if anything, is this
-- user to it. Absent means no access, and the caller answers 404 rather than
-- 403 -- whether an organization exists is not a stranger's business.
-- name: GetOrgMembership :one
SELECT * FROM org_member WHERE tenant_id = $1 AND user_id = $2;

-- Which organizations am I in. The one query that is deliberately NOT scoped to
-- a single organization, and the reason org_member's policy also admits rows by
-- user_id -- see schema.sql.
-- name: ListOrgsForUser :many
SELECT * FROM org_member WHERE user_id = $1 ORDER BY created_at;

-- name: SetOrgRole :execrows
UPDATE org_member SET role = $3 WHERE tenant_id = $1 AND user_id = $2;

-- name: RemoveOrgMember :execrows
DELETE FROM org_member WHERE tenant_id = $1 AND user_id = $2;

-- Organization settings. No row means defaults, so callers treat ErrNotFound as
-- an answer rather than a failure (see Server.orgSettings).
-- name: GetOrgSettings :one
SELECT * FROM org_settings WHERE tenant_id = $1;

-- name: UpsertOrgSettings :one
INSERT INTO org_settings (tenant_id, members_can_create_workspaces)
VALUES ($1, $2)
ON CONFLICT (tenant_id) DO UPDATE
    SET members_can_create_workspaces = excluded.members_can_create_workspaces,
        updated_at                    = now()
RETURNING *;

-- name: CreateWorkspace :one
INSERT INTO workspace (id, tenant_id, name, owner_user_id, access_scope)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetWorkspace :one
SELECT * FROM workspace WHERE id = $1 AND tenant_id = $2;

-- name: GetWorkspaceByName :one
SELECT * FROM workspace WHERE name = $1 AND tenant_id = $2;

-- Admin view: everything in the tenant.
-- name: ListWorkspaces :many
SELECT * FROM workspace WHERE tenant_id = $1 ORDER BY name;

-- What a non-admin may see: workspaces open to the whole organization, plus the
-- ones they own or were added to. Creating a workspace also writes an owner
-- membership row, so the creator's own arrives through the same route -- one
-- rule instead of a special case.
--
-- EXISTS rather than the JOIN this replaced, and that is not a stylistic
-- preference: with the org disjunct added, a JOIN returns a workspace TWICE for
-- a user who is both an explicit member and covered by access_scope = 'org'.
-- EXISTS cannot duplicate a row. It also matches how app_user's RLS policy asks
-- the same question, so there is one shape for "is this person a member".
-- name: ListWorkspacesForUser :many
SELECT w.*
FROM workspace w
WHERE w.tenant_id = $1
  AND (w.access_scope = 'org'
    OR EXISTS (SELECT 1
               FROM workspace_member m
               WHERE m.workspace_id = w.id
                 AND m.user_id = $2))
ORDER BY w.name;

-- Rename, which is the reason id and name are separate columns at all. Refused
-- by the caller while a task is live: everything OUTSIDE the database is keyed
-- on the name, so renaming under a running task orphans its tunnel and
-- invalidates the credential it is holding.
-- name: RenameWorkspace :one
UPDATE workspace SET name = $3 WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- Unlike a rename, this is safe while a task is live: nothing outside the
-- database is keyed on the access scope, so widening or narrowing it changes
-- who may ask for a session and nothing else. Sessions already running are not
-- evicted -- see handleSetAccessScope.
-- name: SetWorkspaceAccessScope :one
UPDATE workspace SET access_scope = $3 WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- Clearing last_error here is the whole of the rule that keeps it honest: a
-- status write is what a placement ATTEMPT does, so anything the previous
-- attempt recorded stops applying at that moment. Written as part of the same
-- statement rather than as a second one, because the two orders are not equally
-- wrong -- clearing after the status write leaves a window in which the row
-- reads `starting` with the last failure still attached to it, and that is
-- precisely the row a console is polling.
-- name: SetWorkspaceStatus :exec
UPDATE workspace SET status = $3, task_ref = $4, last_error = NULL
WHERE id = $1 AND tenant_id = $2;

-- The other half: a placement that failed, recorded where the console and the
-- next caller can both read it. One statement with the status, so a row can
-- never say `stopped` with no reason or carry a reason while claiming to be on
-- its way up.
--
-- The STATUS is the caller's to choose, and the choice is "may a task exist".
-- Nothing was placed before dispatch, so that is `stopped`; a task that was
-- dispatched and never dialed in may well be out there, so that stays
-- `starting` with its reference -- which is also what earns it the reconnect
-- grace on the next attempt instead of a second task (see ensureWorkspace).
-- name: FailWorkspacePlacement :exec
UPDATE workspace SET status = $3, task_ref = $4, last_error = $5
WHERE id = $1 AND tenant_id = $2;

-- Section 2.4's lifecycle policy, all of it in one statement so a PATCH cannot
-- half-apply.
-- name: SetWorkspacePolicy :one
UPDATE workspace SET
    idle_timeout_secs   = $3,
    warm_hold_secs      = $4,
    admission_policy    = $5,
    max_sessions        = $6,
    min_free_memory_pct = $7
WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- The warm-hold deadline, set when the last session in a workspace stops and
-- cleared when one appears again. NULL means no hold is running.
--
-- Deliberately NOT touching generation. An idle or warm-hold stop is a stop,
-- not a placement: advancing the generation would invalidate the credential of
-- a task that is alive and about to reconnect, and the ECS driver would then
-- adopt that same still-running task and hand back its ARN -- leaving a task
-- that can never authenticate again (internal/creds/creds.go).
-- name: SetWarmHold :exec
UPDATE workspace SET warm_hold_until = $3 WHERE id = $1 AND tenant_id = $2;

-- Section 2.4: this workspace would be stopping, and the only thing holding it
-- open is a background process inside a session. Recorded so the console can
-- say so; nothing acts on it, because the member who started that process meant
-- to, and stopping the session would kill it.
-- name: SetIdlePinned :exec
UPDATE workspace SET idle_pinned_since = $3 WHERE id = $1 AND tenant_id = $2;

-- The workspace's disk between tasks (section 9 item 0).
--
-- Two statements rather than one UPDATE, and the split is the whole design.
-- ECS cannot re-attach a volume, so this column is the only thing connecting a
-- stopped workspace to its filesystem: it is written when a snapshot COMPLETES,
-- and the snapshot it replaces is deleted only after that write lands. A crash
-- between them leaks a snapshot and costs money. The other order loses every
-- member's home, which is not a cost, it is the product failing.
--
-- Returns the row so the caller learns which snapshot it just displaced,
-- without a second read that could observe a different one.
-- name: SetWorkspaceSnapshot :one
UPDATE workspace SET snapshot_id = $3 WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- name: SetSessionIdleTimeout :one
UPDATE session SET idle_timeout_secs = $3 WHERE id = $1 AND tenant_id = $2
RETURNING *;

-- Atomic. The previous JSON store read the record, incremented, and wrote back
-- a copy taken BEFORE the increment, so the counter never left zero and two
-- genuinely different placements derived the same ECS client-token -- which ECS
-- answers with the original task (section 2.8). A single UPDATE ... RETURNING
-- cannot express that bug.
-- name: NextGeneration :one
UPDATE workspace SET generation = generation + 1
WHERE id = $1 AND tenant_id = $2
RETURNING generation;

-- name: DeleteWorkspace :execrows
DELETE FROM workspace WHERE id = $1 AND tenant_id = $2;

-- name: AddMember :one
INSERT INTO workspace_member (workspace_id, user_id, tenant_id, role)
VALUES ($1, $2, $3, $4)
ON CONFLICT (workspace_id, user_id) DO UPDATE SET role = excluded.role
RETURNING *;

-- name: RemoveMember :execrows
DELETE FROM workspace_member WHERE workspace_id = $1 AND user_id = $2 AND tenant_id = $3;

-- The authorisation primitive: what, if anything, is this user to this
-- workspace. Absent means no access at all.
-- name: GetMembership :one
SELECT * FROM workspace_member WHERE workspace_id = $1 AND user_id = $2 AND tenant_id = $3;

-- name: ListMembers :many
SELECT m.*, u.email
FROM workspace_member m
         JOIN app_user u ON u.id = m.user_id
WHERE m.workspace_id = $1
  AND m.tenant_id = $2
ORDER BY m.created_at;

-- name: CreateSession :one
INSERT INTO session (id, workspace_id, tenant_id, user_id)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetSession :one
SELECT * FROM session WHERE id = $1 AND tenant_id = $2;

-- What a workspace OWNER sees: every session in it, whoever started them.
-- name: ListSessions :many
SELECT * FROM session WHERE workspace_id = $1 AND tenant_id = $2 ORDER BY created_at DESC;

-- What a plain member sees: only their own. Two named queries rather than one
-- query with a conditional, because the conditional is the part worth being
-- able to read -- and each is independently testable.
-- name: ListSessionsForUser :many
SELECT * FROM session
WHERE workspace_id = $1 AND tenant_id = $2 AND user_id = $3
ORDER BY created_at DESC;

-- Every session in the organization, for an organization owner.
-- name: ListOrgSessions :many
SELECT * FROM session WHERE tenant_id = $1 ORDER BY created_at DESC;

-- What everyone else may see across the whole organization: their own sessions,
-- plus every session in a workspace they own. The same rule ListSessions and
-- ListSessionsForUser split per workspace, expressed once for all of them --
-- which is what `lem --session <id>` needs, since it resolves a prefix without
-- being told a workspace.
-- name: ListOrgSessionsVisible :many
SELECT s.*
FROM session s
WHERE s.tenant_id = $1
  AND (s.user_id = $2
    OR EXISTS (SELECT 1
               FROM workspace_member m
               WHERE m.workspace_id = s.workspace_id
                 AND m.user_id = $2
                 AND m.tenant_id = $1
                 AND m.role = 'owner'))
ORDER BY s.created_at DESC;

-- name: DeleteSession :execrows
DELETE FROM session WHERE id = $1 AND tenant_id = $2;

-- Ensure a membership row exists without touching an existing role.
--
-- DO NOTHING rather than AddMember's DO UPDATE, and the difference is the whole
-- reason this exists separately: a workspace open to the whole organization has
-- no rows until somebody starts a session, and creating one on the way past must
-- not be able to demote the workspace's owner to 'user'. Same reasoning as
-- JoinOrg above.
-- name: EnsureMember :exec
INSERT INTO workspace_member (workspace_id, user_id, tenant_id, role)
VALUES ($1, $2, $3, $4)
ON CONFLICT (workspace_id, user_id) DO NOTHING;

-- Take the next POSIX uid for this workspace, atomically.
--
-- Reads like NextGeneration and for the same reason: a read-modify-write here
-- would let two concurrent first-sessions take the same number, and the thing
-- the number keeps apart is one member's home from another's.
--
-- The counter only ever climbs, so a departed member's uid is never handed out
-- again -- their files are still on the volume under it.
-- name: NextWorkspaceUID :one
UPDATE workspace SET next_uid = next_uid + 1
WHERE id = $1 AND tenant_id = $2
RETURNING next_uid - 1;

-- Assign a uid to a member who has none.
--
-- `AND uid IS NULL` is what makes it safe to call on every session: somebody who
-- already has one is not renumbered, and a caller that lost the race gets no
-- rows rather than overwriting the winner.
-- name: SetMemberUID :one
UPDATE workspace_member SET uid = $4
WHERE workspace_id = $1 AND user_id = $2 AND tenant_id = $3 AND uid IS NULL
RETURNING *;
