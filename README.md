# lemul-cc

> **Alpha software.** This project is still in **alpha** development.
> **Try at your own risk.** APIs, data layouts, and operational behaviour
> can change without notice. Do not use it for production workloads you
> cannot afford to lose.

Remote, sandboxed Claude Code workspaces that run in the **customer's own AWS
account** and bill to their Bedrock.

Licensed under the [GNU Affero General Public License v3.0](LICENSE)
(`AGPL-3.0`).

Architecture, tenancy, and the runner/supervisor split are documented in
this README and in [`docs/onboarding-bedrock.md`](docs/onboarding-bedrock.md).

**Multi-tenant rebuild in progress.** Phases 1 (derived credentials, reconnect
grace, gateway allowlist, protocol version, `log/slog`, lint + CI), 2 (Postgres
with row-level security, replacing the JSON store), 3 (identity, Organizations,
and a principal resolved per request), 4 (workspace CRUD and membership,
authorisation on every handler, the noun-verb CLI) and 5 (the Relay split) are
done, and so is **Phase 6 — shared org workspaces**: the access model, the
organization creation gate, per-member identity inside a task, idle detection /
warm hold / admission control, placement on create, and the console surfaces
have all landed.

A **tenant is an Organization** to everyone outside the code, and a user belongs
to many — see §12.8. Signing in creates your own, which you cannot leave; an
invite code adds you to someone else's. Everything an organization owns is
addressed under `/v1/orgs/{org}`, where `{org}` is a generated three-word slug
like `forty-crimson-windmill`, and **no endpoint assumes an organization** —
picking a sensible one is a client's job.

A **workspace is a shared machine**, not one developer's sandbox — several
members of one organization hold sessions in the same Fargate task (§2.3,
§12.10). That is what amortises the ~23 s cold start across a team instead of
charging it to each person. Each member's sessions run at **their own uid**, in
their own home, with their own `CLAUDE_CONFIG_DIR`; one **`/workspace/shared`** directory
is common, and what goes in it is the customer's business. `access_scope` says
who may use one — `owner` (which is what a "personal" workspace now is),
`members`, or `org` — and an organization setting
(`members_can_create_workspaces`, default off) says who may make one. See
"Shared workspaces" below for what actually enforces the boundary.

The control plane **requires** `-database-url`; there is no in-process fallback,
and `-apply-schema` is a development convenience because the application role
deliberately holds no DDL privileges. `-directory-url` defaults to the same DSN,
which keeps a single-cell development setup one database.

Running the tests needs Docker: `internal/store`, `internal/mgmtapi` and
`e2e` start a throwaway Postgres via testcontainers, and skip with a message
when Docker is absent. Use `make test` (or `-p 1`): running those packages
concurrently once cascaded into unrelated-looking failures from container
contention.

**Status: Phase 0 complete. The vertical slice works end to end** — the split,
sandbox image, both local drivers, gateway inference, preflight, telemetry and
session lifecycle (stop/resume/delete), now on real identity and real tenancy.
The `ecs` driver and the Terraform module have been applied against a real AWS
account several times, most recently on **2026-08-03** — and unlike every
earlier round, **both halves are currently deployed and joined**: the customer
module (runner + workspace tasks) in Fargate, and the vendor side
([`deploy/`](deploy)) on an EC2 host, with the runner dialling it over the
public internet. See "Deploying to Fargate" and `deploy/README.md`.
Phase 6 then made a workspace shared: `access_scope`, the organization creation
gate, and per-member uid, home and Claude Code state inside the task, asserted
end to end against the real image by `e2e/isolation_test.go`. **Idle detection,
warm hold and admission control then landed** — prerequisites rather than
billing hygiene, because an OOM takes down every member's sessions rather than
only your own. **Placement then moved to workspace create**, as a background
operation whose failures land on the record rather than in a log, which is also
what finally gave a cold start something to say for itself.

**Cold start was re-measured against the two-service split on 2026-08-02**, on a
real account, then destroyed again: 7 placements, **median 22.6 s** dispatch to
tunnel, against the 23 s measured before the split — so dialing two hostnames
instead of one costs nothing. The warm side had never been measured at all and
is the number §12.10 is argued from: a colleague joining a workspace somebody
else started reaches a prompt in **1.7 s**, roughly 16× faster, with the delta
almost exactly the placement.

**The model path on Fargate was exercised on 2026-08-03**, which neither earlier
run had done: a workspace task brokered a real Claude Code turn through a
customer-hosted gateway reached over the public internet, and the same session
then resumed on a restored volume. That closes the last "never run in AWS" gap
in the session path.

## Components

```
cmd/lem             the user's local client. Raw mode, WSS, Ctrl-] to detach
cmd/controlplane    the Management API: everything that knows something
cmd/relay           the session data path: PTY bytes, and nothing else
cmd/runner          one per organization, in their VPC. CONTROL ONLY, never on the data path
cmd/supervisor      one per workspace task. Owns N PTYs, dials out on TWO tunnels
cmd/keyprobe        diagnostic: what bytes does your terminal send for a key?
cmd/preflight       is this AWS account's Bedrock actually usable?
```

**The control plane is two services, and neither calls the other.** That is
§2.7 doing structural work rather than being an aspiration: as long as one
process saw both a PTY stream and a directory listing, "we route only
ciphertext; we hold no key that can decrypt your session" (§1.3's A3) would have
been false about the same connection it was claimed on. The relay holds no
database, no directory and no runner — asserted over its own import graph by
`TestTheRelayImportsNothingThatReadsCustomerData`, because nothing else would
notice that decaying.

They share exactly one thing: the signing key. The Management API mints the
credentials the relay verifies.

The runner/supervisor split is decision #6, *task-dials-out*. The runner places
tasks and nothing else, so it can crash, redeploy or auto-update while sessions
keep running. Each workspace task holds its own tunnel.

```
customer VPC                                control plane (two services)
┌──────────────────────────────┐           ┌────────────────────────┐
│ runner ──────────────────────┼─── WSS ──>│ Management API  :9000  │
│   ecs:RunTask/StopTask       │           │   store, directory,    │
│                              │           │   placement, authz     │
│ workspace task               │           └────────────────────────┘
│   supervisor ── control ─────┼─── WSS ──────────^
│              ── data ────────┼─── WSS ──>┌────────────────────────┐
│     ├─ PTY (session 1)       │           │ Relay           :9001  │
│     └─ PTY (session 2)       │           │   bytes, nothing else  │
└──────────────────────────────┘           └────────────────────────┘
                                                       ^
                                            user ──────┘
```

Nothing listens inbound in the customer's account.

**The supervisor's two tunnels fail differently, on purpose.** A 401 on the
**control** tunnel came from the service that read the workspace record, so it
means this task has been replaced — retrying cannot fix that, and a task that
keeps trying holds its volume and bills indefinitely, so it exits. A 401 on the
**data** tunnel means only that the relay would not take the credential, which a
redeploy produces; exiting there would end live PTYs for somebody else's fault,
so it retries and the sessions keep running unattached.

That asymmetry is load-bearing rather than tidy. The relay has no database, so
it cannot look a generation up — the task *announces* one and the credential is
verified against it. On its own that would accept a replaced task forever; what
removes it is the control tunnel's check.

## Packages

| Path | What it is |
|---|---|
| `internal/ptysession` | The heart of the split. N PTYs, replay ring, **mode prelude**, SIGWINCH nudge, viewer enforcement. PTY lifetime is independent of connection lifetime — that is what detach means. Each PTY carries its own `Credential` and `Dir`, so a session forks as its own member in their own home. |
| `internal/supervisor` | One per workspace task: owns the PTYs, brokers inference, serves the explorer reads, and answers the control tunnel. Root, so that the gateway credential and the uid boundary are both out of a session's reach. |
| `internal/supervisor/identity.go` | Who a session runs as. Homes and real `passwd` entries provisioned once per member per placement — real entries rather than just `HOME=`, because `whoami` failing breaks git in ways that never name the cause. **Refuses a session it has no identity for, but only when root**: unprivileged there is no boundary to fall short of, so the announced identity is informational and refusing would break every local setup to protect nothing. |
| `internal/mgmtapi/sessionidentity.go` | The other half: allocating a member's uid and telling the task about it. It travels on the **control** tunnel and only there — the relay holds no customer records, so a uid arriving from it would mean a compromised relay picks whose files a session can read. Sent from the only two places that precede a PTY fork: endpoint negotiation and resume. It is the **session's** owner, never the caller. |
| `internal/tunnel` | yamux over WSS, `[type:1][len:4][payload]` framing, and the control-message union. WebSocket frame types do not survive a yamux stream, so the tag is explicit. |
| `internal/proto` | The client↔relay contract: binary frames are PTY bytes, text frames are control. |
| `internal/agent` | The dial-out loop both the runner and the supervisor use. |
| `internal/driver` | Workspace runtime behind an interface: `local` (process), `docker` (the real sandbox image), `ecs` (Fargate). Exists from day one so our debugging environment shares the product's code path (§13). |
| `internal/driver/ecs` | `RunTask` with the §2.8 idempotency key as `clientToken`, adoption of an existing task by `startedBy` — a restarted runner has no memory of having placed one — and the workspace EBS volume: attached per task, restored from the previous task's snapshot, preserved again when it stops. The only driver that implements `driver.Snapshotter`. Unit-tested against fake ECS and EC2 clients; this is the one driver `e2e/` cannot run. |
| `terraform/` | The deployment: runner service, the roles of §2.1, workspace task definition, egress-only security groups, the workspace EBS volume and the infrastructure role ECS attaches it with, runner-token delivery. Applied and verified once **before the volume existed**, then destroyed — see "Deploying to Fargate". |
| `internal/registry` | Live tunnels. A **set** per key, never one — §2.8. Workspace tunnels are keyed on (organization, workspace **name**), because a name is unique only within an organization and keying on it alone would have joined two customers' `api` workspaces into one tunnel set. |
| `internal/mgmtapi` | The Management API: handlers, authorisation, placement, the runner and control tunnels. Holds the database, the directory and the signing key. |
| `internal/mgmtapi/placement.go` | Placement as a background operation: create answers `202` and starts the task behind it. The goroutine deliberately does **not** hold the request's context — that is cancelled the moment the `202` is written, so a placement inheriting it would be killed by its own success. Every failure is written to `workspace.last_error` by `ensureWorkspace` itself, so the blocking caller and the background one cannot disagree about what a failed placement leaves behind. |
| `internal/mgmtapi/events.go` | §2.6's workspace event stream, which existed in the API surface from Phase 1 and had never been built — its absence is why a cold start read as a hang. Opens with a snapshot, then changes only, by polling the record: a workspace's control tunnel is held by one replica, so an in-memory fan-out would silently deliver nothing to any other. |
| `internal/relay` | The session data path. Attach, the workspace data tunnel, a registry and a nonce set — and deliberately nothing that reads a customer row. |
| `internal/bedrock` | Three-layer model preflight. Layer 1 (availability) is advisory, layer 2 (a real `InvokeModel`) is authoritative, layer 3 maps errors to the actual fix. |
| `internal/gateway` | The supported inference path. One loopback proxy per session; the supervisor holds the gateway credential and injects attribution the session cannot forge. |
| `internal/store` | One cell's Postgres: users, organization and workspace membership, workspaces, sessions, and `org_settings`. Member uids come from a counter on the workspace (`next_uid`), **never `MAX(uid) + 1`** — removing a member deletes the row, so a maximum drops back and the next person inherits both that number and the home still on the volume under it. Every query runs through `InTenant`, which scopes a transaction before anything else can execute; RLS then fails **closed** if one ever does not. `app_user` is the one table isolated by membership rather than by a tenant column, because a user belongs to the cell and to many organizations. |
| `internal/directory` | The global tier: which cell holds which principal, and which organization is their own. Deliberately tiny -- a cell is its own database, so this exists only to break the circle of "ask the cell which cell to ask". |
| `internal/orgslug` | The three-word name an organization is addressed by. Generated rather than derived from the display name, which is not unique and changes. |
| `internal/wsname` | The two-word `[adjective]-[vehicle]` name a workspace gets when its creator does not pick one. Its adjective list deliberately holds no word that makes a disaster of a vehicle — every one of the 11,760 pairs ships, so `doomed-airliner` is not a joke available once. |
| `internal/statekey` | Where the signing key comes from, for both services and in one place. Two policies, and the asymmetry is the point: the Management API **owns** the key and creates one when it finds none, while the relay only ever verifies what the other signed, so a key of its own would reject every credential minted — reported as `unauthorized`, which reads as a bad token and sends whoever is debugging it to look at the client. It refuses to start instead. |
| `internal/authtest` | A local OIDC issuer for tests: an RSA key, a JWKS on `httptest`, real RS256 tokens. It is what let the unauthenticated dev-principal mode go. |

## Run it locally

```bash
go build -o bin/ ./cmd/...

# An identity provider is REQUIRED -- the control plane refuses to start without
# one. Logto + Mailpit, fully scripted; see auth-stack/README.md.
docker compose -f auth-stack/docker-compose.yml up -d
bun auth-stack/seed.ts > auth-stack/.env.generated
set -a; . auth-stack/.env.generated; set +a

# Postgres for the cell and the directory. One database is fine locally.
docker run -d --name lemul-pg -e POSTGRES_PASSWORD=postgres -p 5432:5432 postgres:17-alpine
ADMIN='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable'

# The application role, WITHOUT BYPASSRLS. The control plane refuses to start as
# anything else, because a role that can see through the policies makes every
# one of them decorative.
psql "$ADMIN" -c "CREATE ROLE lemul_app LOGIN PASSWORD 'lemul_app_password' NOBYPASSRLS"
psql "$ADMIN" -c "GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO lemul_app"

export LEMUL_DATABASE_URL='postgres://lemul_app:lemul_app_password@localhost:5432/postgres?sslmode=disable'
export LEMUL_SCHEMA_URL="$ADMIN"

# The four -auth-* flags default to the environment sourced above.
# -relay-url is what endpoint negotiation hands to clients and what a placed
# task dials for its data tunnel; without it there is nothing to attach to.
bin/controlplane -addr :9000 -state-dir ./.state -apply-schema -session-cmd claude \
                 -relay-url ws://localhost:9001

# The relay, in a second terminal. SAME -state-dir: it verifies credentials the
# control plane minted, so it needs that key and deliberately will not invent
# one -- a key nothing else shares rejects every token it is handed.
bin/relay -addr :9001 -state-dir ./.state

# Sign in; the device code is approved in a browser and the email lands in
# Mailpit at :8025. `lem` learns the issuer, audience and its own client id from
# GET /v1/auth/config, so it needs none of the variables above.
bin/lem login

# Sign-up happens on that first authenticated call; ask where you landed.
bin/lem orgs

# A runner serves ONE organization and proves it with a credential derived for
# that organization. Take the slug `lem orgs` printed.
eval "$(bin/controlplane -state-dir ./.state -print-runner-env forty-crimson-windmill)"

bin/runner -control-plane ws://localhost:9000 -supervisor ./bin/supervisor

bin/lem workspace create w1
bin/lem --workspace w1
```

**Pointing `lem` at a deployed control plane** is `-server`, or `LEMUL_SERVER`
so it does not have to be repeated on every command:

```bash
export LEMUL_SERVER=https://cp.<your-host>
lem login
lem workspace ls
```

It needs nothing else: `lem` fetches the issuer, audience and its own client id
from `GET /v1/auth/config` on whatever `-server` names, so a different
deployment is a different value and no other configuration.

> ⚠️ **The cached token is ONE file, not one per server.** It lives at
> `<user config dir>/lemul/token.json` and carries the issuer it was minted for,
> so signing in to a second control plane overwrites the first. The symptom is
> not a helpful error: the next command without `LEMUL_SERVER` set talks to
> localhost with a token minted for the remote issuer, which is rejected as
> unauthenticated — so it reads as a broken login rather than a misdirected one.
> Export the variable, or expect to re-run `lem login` when switching.

Three things there are easy to get wrong and fail in ways that do not name
themselves.

**`-print-runner-env` deliberately needs no identity provider.** It is the one
subcommand that runs before a deployment is configured, and an operator holding
only the signing key has to be able to produce a runner's credential from it. So
the auth check sits after it, not before.

**Two roles, two DSNs.** The application role deliberately holds no DDL, and it
must not hold `BYPASSRLS`; the role that can create the schema is exactly the one
the control plane refuses to run as. So `-apply-schema` takes `-schema-url`
separately. Grant the app role again after the first apply — the `GRANT … ON ALL
TABLES` above only covers tables that already exist.

**Only the control plane is told what a session runs.** `-session-cmd` is a flag
on `controlplane` and on nothing else. It used to be a flag on the relay too,
each defaulting to `claude` independently, so telling one and not the other
meant a session ran a different program depending on whether it was created by
attaching or by resuming — silently, because both answers are valid programs.
The Management API now states it once, at placement, and the supervisor is the
only thing that knows it (§2.5). `internal/statekey` is the other half of that
shape: the signing key is the one thing the two services genuinely do share, so
it has one implementation with two policies rather than two implementations.

**`-state-dir` matters more than it looks.** It is where the signing key lives,
and every credential in the deployment — workspace, attach and runner — is
derived from it. Without one the control plane generates a fresh key at boot and
says so, which orphans every running workspace task on restart.

```
lem                                   new session in your only workspace
lem --workspace w1                    new session in w1
lem --session <id>                    attach; reattaches if detached, resumes if stopped
lem --session <id> --viewer           watch, read-only

lem workspace ls                      WORKSPACE STATUS SESSIONS ACCESS OWNER CREATED
lem workspace create [name]           no name generates one, like drifting-schooner
lem workspace rm <name>
lem session ls [--workspace w1]
lem session rm --session <id>

lem orgs                              organizations you belong to
lem invite [-role owner]              a code that adds someone to this one
lem join <code>                       redeem one
```

**`lem` always starts a NEW session.** Reattaching is `--session`, explicitly.
Opening a second terminal should give a second session — that is what the shared
filesystem is for — and the old reattach-by-default picked "the newest idle
session in this workspace", which is not necessarily *yours*: a workspace is
shared and a session is not, so it could drop you into a colleague's
conversation with a keyboard.

**`--session` covers reattach AND resume**, which is why there is no `resume`
verb. Attach carries `Create`, the supervisor forks the PTY when it is absent,
and it picks `--resume` or `--session-id` by reading the transcript on the
workspace volume — the only honest source, since a replacement task comes up
with an empty disk (§12.7). A cold workspace is placed first by the same path.
Three cases a user cannot tell apart anyway, one flag.
`POST /sessions/{sid}/{stop,resume}` remain as HTTP endpoints for the console,
which has to be able to put an agent back to work without becoming its terminal.

There is no `--force` in the CLI. The server refuses a delete that would drop
conversations and says what is in the way; the console has a confirmation dialog
to justify a force flag where a terminal does not.

**Workspace names are generated** when you do not pick one: `[adjective]-[vehicle]`,
`drifting-schooner` (`internal/wsname`).

**`lem workspace ls` carries an ACCESS column** — `owner`, `members` or `org`.
On a shared machine that is the more useful column than OWNER: the question a
user is actually asking is whether their colleagues are in here too.

**`--workspace` follows the same rule as `--org`**, and for a sharper reason.
With one workspace you can reach, bare `lem` uses it; with several it refuses
and lists them; with none it names `lem workspace create`. There used to be a
personal workspace — `pw-` plus your email — created lazily by bare `lem`, and
it is gone: a workspace is now a shared machine somebody pays for, so a command
with no arguments must not be able to start a Fargate task, and an organization
that says who may create workspaces cannot mean it while one path creates them
on its own.

**`--org` is required exactly when it is ambiguous.** With one organization the
CLI uses it; with several it refuses and lists them, rather than picking. That
asymmetry is the point: the API never assumes an organization, so the only
guessing in the product happens here, where it can be made to stop before a
destructive verb lands on the wrong customer.

`--session` takes any **unique prefix**, resolved across the whole organization
rather than one workspace — the endpoint that acts on a session is org-scoped,
so demanding `--workspace` purely to look an id up would ask for something the
API does not need. Session ids are UUIDs because Claude Code's `--session-id`
requires one, which is worth the length: our id *is* the conversation id, so
nothing maps between them (§12.7).

Detach is `Ctrl-]`. The client recognises all three encodings a terminal may use
for it — the legacy `0x1d`, kitty `CSI 93;5u`, and modifyOtherKeys
`CSI 27;5;93~` — because Claude Code negotiates modes that stop emulators
sending the legacy one. `bin/keyprobe -cc` prints what your terminal actually
produces if detach ever stops working.

Pick a port nothing squats on. `9222` is Chrome DevTools and will silently win
for `localhost`, the same way OrbStack takes 4318.

The runner places the supervisor through the `local` driver, so this is the same
code path the ECS driver takes — only the placement substrate differs.

## Deploying to Fargate

```bash
cd terraform && terraform init
terraform apply -var-file=example.tfvars     # copy and fill it in first
```

Applied against a real account on 2026-07-31, and a workspace **did** come up:
`ecs:RunTask` placed the task, its supervisor dialled back, and a session ran
real Claude Code 2.1.220 as uid 1000. Pull 6 s, tunnel up 23 s after dispatch.
It was then destroyed — `terraform destroy` removed all 27 resources cleanly, so
the module is verified in both directions. Applied again on **2026-08-03** for
the workspace volume (24 resources, the S3 bucket having gone and the volume
infrastructure role having arrived), then destroyed. **Nothing is deployed
today.**

> **Run the runner as the ECS service, not by hand.** `runner_desired_count = 0`
> plus a local `runner -driver ecs` is how the 2026-07-31 and 2026-08-02 rounds
> were done, and it quietly invalidates anything you conclude about the volume.
> None of the runner task definition's environment exists in that mode, and an
> empty `-ecs-volume-name` **disables the volume entirely** — the workspace then
> comes back blank and the run looks like it passed. It also runs every ECS and
> EC2 call under *your* admin credentials, so the tag-scoped `ec2:CreateSnapshot`
> / `DeleteVolume` and the `iam:PassRole` on the infrastructure role — the whole
> §2.1 widening — go untested. In the service the runner holds the runner task
> role, which is the principal the design describes.

> **Set `-public-url` on the control plane.** It defaults to
> `ws://localhost<addr>`, which is what the workspace task is told to dial — and
> inside a Fargate task `localhost` is the task itself. The `docker` driver hides
> this because it rewrites loopback to `host.docker.internal`; the `ecs` driver
> does not, and should not. The symptom lands in the *workspace task's* log,
> while the control plane only says "did not dial in".

`terraform/iam.tf` is the file to read
first: it is the §2.1 split, and the two invariants that must never
break (the runner never gains `bedrock:*`, the task role never gains `ecs:*`)
are stated at the top of it.

**It is three roles since 2026-08-02, not two.** The workspace volume needs an
ECS *infrastructure* role — ECS assumes it to create and attach the disk — and
the runner gained tag-scoped `ec2:CreateSnapshot`, `DeleteVolume` and
`DeleteSnapshot`, because ECS will attach a volume for us but will not snapshot
one. The runner may only *pass* the infrastructure role, never assume it. The
task role **narrowed** in the same change: it held read/write on an S3 snapshot
bucket, and a session is a shell, so that was storage every member could reach.

**The volume was applied and verified on 2026-08-03**, then destroyed. It
attaches (30 GiB gp3, encrypted, `deleteOnTermination` false), the auto-stop
cascade snapshots it, and a restored workspace comes back with a member's home
and their conversation — the supervisor forking `claude --resume` after reading
the transcript off the restored disk. One bug did not survive the round: a
restored volume had no `filesystemType`, so ECS mounted an ext4 snapshot as
**xfs** and every restore died as `TaskFailedToStart` after nine minutes in
`PROVISIONING` with a null reason.

A workspace's volume is released when its task stops, by `Preserve`, which waits
for the task to reach `STOPPED` first — `ecs:StopTask` is asynchronous, and until
2026-08-03 acting immediately meant `DeleteVolume` was rejected `VolumeInUse` on
every stop while the snapshot was taken before the container had finished
writing. It releases whether or not it preserved, because `DELETE` skipping the
step orphaned the very disk it was avoiding a bill for.

> ⚠️ **Volumes orphaned before that fix are still there, and Terraform cannot
> remove them** — it never knew they existed. A crash between stopping a task
> and releasing its disk will always be able to leak one, so it is worth
> checking by tag when tearing down:
>
> ```bash
> aws ec2 describe-volumes --filters Name=tag-key,Values=lemul:workspace \
>   --query 'Volumes[?State==`available`].VolumeId' --output text
> aws ec2 describe-snapshots --owner-ids self \
>   --filters Name=tag-key,Values=lemul:managed --query 'Snapshots[].SnapshotId'
> ```

Sessions run at **uid 1000** inside the task while the supervisor stays root,
which is what keeps the gateway credential out of a session's reach — see
Inference below. `CLAUDE_CODE_SUBPROCESS_ENV_SCRUB` would do the same job with a
PID namespace and **cannot run on Fargate** (§12.6), so the task definition does
not set it.

## Tests

```bash
make test                                                 # -p 1; four packages need Docker
LEMUL_E2E_CLAUDE=1 go test ./e2e -run TestClaudeCode -v   # needs claude + a login

# The model path, against a real gateway. Spends real money.
# Start a LiteLLM (or compatible) gateway on :4000, then:
image/build.sh
LEMUL_E2E_GATEWAY=1 go test ./e2e -run TestGateway -v
```

`e2e/` runs the whole thing in one test binary: the real Management API, the
real **relay on its own listener**, a real runner, a real driver, and the **real
supervisor binary**, built by `TestMain`. Only the client is a harness.

The relay is a second listener rather than another handler on the first one, and
that is deliberate: mounting both on one mux would have been simpler and would
have left the split as the one arrangement nothing routinely exercises.
`fidelity_test.go` is the §4.1 suite ported from the S3 prototype — its job is
to prove the split did not regress what S3 established.

## The sandbox image

```bash
image/build.sh                      # cross-compiles the supervisor, builds the image
bin/runner -driver docker -image lemul-workspace:dev
```

Each workspace gets its own **docker named volume** at `/workspace`, named
`<-volume-prefix><workspace-id>` — the local stand-in for the EBS volume a
Fargate task gets. Volumes outlive their containers deliberately: that is what
carries a member's home and the shared directory across a replacement task.
`docker volume ls --filter name=lemul-vol-` finds them.

**A named volume and not a host bind mount, and the difference was measured**
(2026-08-02). A named volume is real ext4 inside the VM, so it carries uid, the
setgid bit and POSIX ACLs. A host path on macOS is virtiofs, which carries none
of them:

| `/workspace` backed by | member home | one member reading another's transcript | the shared directory |
|---|---|---|---|
| named volume | `700 lem2000:lem2000` | **DENIED** | `2775 root:lemul` + ACL |
| host bind mount | `700 root:root` | **ALLOWED** | `775 root:root`, `setfacl` failed |

The right-hand column is the bug that made the shared directory unwritable for a
whole phase, and the middle one is §13's trap: a local answer that is the
*opposite* of the real one. Neither announces itself — a bind-mounted workspace
looks like one that works.

**Per workspace, not one volume for all of them**: `/workspace` holds the homes
root, so a single volume would put two local workspaces on one filesystem, each
seeing the other's members' homes. And keyed on the workspace rather than on the
idempotency key the container name uses, or every replacement task would come up
with an empty disk.

This replaced `-project`, which bind-mounted a host directory at `/workspace`
**and defaulted to the working directory** — so `runner -driver docker` from
anywhere silently mounted that directory into every workspace it placed, over
the layout the entrypoint creates. To put a repo in front of Claude Code
locally, write it into the workspace's `shared/` directory, which is how a
customer would reach it.

Same image, entrypoint, managed settings and supervisor binary that Fargate will
run — only the placement substrate differs. Managed settings are rendered at
container start from environment, not baked, because one image has to serve both
a Bedrock workspace and local development against a host login (`-host-login`
mounts your Claude credentials in; never use it for a Bedrock workspace).

> ⚠️ **`-host-login` does not currently work**, and this predates the uid
> boundary. The image sets `CLAUDE_CONFIG_DIR=/workspace/.claude` as its
> default, and Claude Code falls back to `$HOME/.claude` only when that variable
> is unset — so a login mounted under `HOME` is read by nothing. Fixing it means
> mounting the credentials into the config dir, or leaving `CLAUDE_CONFIG_DIR`
> unset for that mode; neither is obviously right. (The image value is only the
> fallback now — a session with a member identity gets a config dir inside that
> member's home instead.)

**The entrypoint lays out the shared machine before anyone arrives.** It creates
the homes root `0755` root-owned — a member must not be able to plant a
directory where a colleague's home is about to go — and `/workspace/shared` as `2775
root:lemul` **with a default ACL**, not `chmod 777`. Both halves are needed:
setgid fixes the *group* of a new entry, the ACL fixes its *mode*, because umask
is per-process and every tool subprocess Claude Code spawns inherits whatever
the login shell set. Without the ACL a file Alice creates is `0644 alice:alice`
and Bob can read but not edit it, which surfaces days later. Homes themselves
are **not** created here: at container start nothing knows who the members are,
so the supervisor makes each one on that member's first session.

**Managed settings gained a `permissions` block**, plus
`allowManagedPermissionRulesOnly` and `disableSideloadFlags`. The first is
load-bearing — permission rules *merge* across scopes, a documented exception to
the precedence order, so without it a member widens the boundary from their own
`settings.json`. The second rejects `--mcp-config`, `--plugin-dir` and
`--plugin-url`, which otherwise walk around all of it from the command line.

> ⚠️ **Managed settings fail SILENTLY, two different ways.** Measured against the
> pinned 2.1.220: a bad **value** warns and then ignores that field while
> carrying on, and an unrecognised **key name** is accepted without a word
> (verified with a control — an invented key with a wrong type passes, while
> every real key rejects the same wrong type). Either leaves a workspace running
> with no boundary and nothing in the log. So the entrypoint **asserts** with
> `claude doctor` — it reads the settings, needs no credentials and makes no
> model call — and **exits 1** on a schema failure, because a task that cannot
> enforce the boundary should not accept sessions. `internal/supervisor`'s
> `managedsettings_test.go` reads `entrypoint.sh` and pins the key names against
> the set verified against the binary, so a typo fails a test rather than a
> customer. **Verify any new key against the binary before adding it.**

The entrypoint also seeds Claude Code's **first-run state**. A workspace task
starts with an empty `CLAUDE_CONFIG_DIR`, so without it the first thing a user
sees is the onboarding theme picker, with the per-directory trust prompt behind
it blocking every tool call. Seeded by merge, never overwrite, so a workspace
that comes back keeps whatever the user chose.

Container naming carries the idempotency key, so the container runtime itself
enforces one task per workspace generation.

## Shared workspaces

A workspace is a machine several members of one organization share. What keeps
one of them out of another's files is a **POSIX uid and mode `0700`** — nothing
else.

```
/workspace                    the volume; EVERYTHING durable is under it
  ├─ homes/<user>             one per member, 0700
  │    └─ .claude/            that member's CLAUDE_CONFIG_DIR
  └─ shared/                  2775 root:lemul + default ACL; everyone
/opt/lemul/workspace-skills   root-owned; installed through the supervisor
```

Everything durable is under **one** mount point, and that is the rule rather
than the layout. On Fargate `/workspace` is an EBS volume; ECS creates it per
task and there is no re-attaching it, so a workspace's filesystem between tasks
**is a snapshot** — stop preserves one, start restores from it. A directory
outside that mount does not have a different path, it has a different
*durability*: it survives nothing, and a snapshot of the workspace does not
contain it.

That was measured before it was fixed.
`TestWithoutAVolumeTheCascadeDestroysEveryHome` runs the real auto-stop cascade
with no volume — what every task placed before 2026-08-02 had — and a marker
written into a member's home is provably gone afterwards. It did not fail
loudly: with no transcript on the fresh disk `sessionArgs` passes `--session-id`
rather than `--resume`, so the user is told their session resumed and shown an
empty conversation.

Locally the volume is a docker named volume, which persists on its own and needs
no snapshot; only the `ecs` driver implements `driver.Snapshotter`.

The shared directory sat at `/shared` until 2026-08-02 for exactly that reason,
and nothing noticed, because with no volume configured at all the two are
equally ephemeral and behave identically. `TestTheSharedDirectoryIsOnTheWorkspaceMount`
and `TestTheSharedDirectoryMatchesTheImage` now pin both halves — the second
because the path is one fact in two files (`entrypoint.sh` creates it,
`identity.go` pre-trusts it), and disagreement shows up as a trust dialog nobody
can explain.

Homes live on the volume because a replacement task starts with an empty
container filesystem, and a home that did not survive one would take the
member's configuration and conversation history with it. `CLAUDE_CONFIG_DIR` is
the single variable that moves *all* of a member's Claude Code state — skills,
plugins, MCP servers, output style, transcripts — somewhere only they can read;
`sessionArgs` reads the transcript from there too, so resume cannot pick up a
colleague's conversation.

**Claude Code's permission system is not the isolation.** It governs what the
*agent* reaches for. A member with a Bash tool is a member with a shell, so
managed settings are a guardrail and a way to avoid an approval prompt on every
`/workspace/shared` access — never the fence.

**The uid arrives on the CONTROL tunnel and only there.** The relay holds no
customer records, so a uid coming from it would mean a compromised relay picks
whose files a session can read — the exact boundary being built. It is sent from
the only two places that can precede a PTY fork: endpoint negotiation, which
every attach must call first, and resume. And it is the **session's** owner,
never the caller: a workspace owner attaching with `--viewer` watches somebody
else's process, and that process goes on running as its owner.

**Workspace-wide skills are root-owned**, because a skill is instruction content
that runs in every member's session — a directory members could write would let
any one of them plant something every colleague's agent then loads.

`e2e/isolation_test.go` pins six cells against the real image: own home allowed,
another member's transcript and home listing denied, `/workspace/shared` write and
cross-edit allowed, the supervisor's `environ` denied. **The ALLOWED rows matter
as much as the DENIED ones** — a suite that only checked denials passes on an
image where nothing works at all. That is not theoretical: home isolation was
right on the first try while *no* member could write `/workspace/shared`, because
`useradd` never added them to the group owning it. Only a run in the real image
found it.

**And that test alone is not enough, which cost a second round.** It runs its
checks through `su`, and su initialises supplementary groups from `/etc/group` —
so it proves the IMAGE is arranged correctly and says nothing about the process
the product forks. `/workspace/shared` was unwritable by every member on every driver for
a full phase while it passed, because `sessionCredential` built a
`syscall.Credential` with no `Groups`, and Go runs `setgroups()` in the child
whenever a credential is set: an empty list **clears** the member's groups
rather than inheriting them. `TestASessionPTYCarriesTheSharedGroup` covers that
layer by writing to `/workspace/shared` through a real session PTY. The two decompose the
claim — the image is right, and the session gets what the image grants — and
the gap between them is where the bug lived. Found by two people using a
workspace, not by a test.

## Inference

Model traffic is brokered through a **customer-hosted gateway** (LiteLLM or
similar). Direct-to-Bedrock is a draft — see §12.5; Bedrock becomes one backend
*behind* the gateway rather than something Claude Code speaks natively.

Which backend is the customer's choice, not ours, and the local stack
demonstrates it: [`deploy/`](deploy) can run LiteLLM against OpenRouter or
Bedrock, and nothing above the gateway changes except the `-pins` names. Those
names are the coupling to watch — they are what Claude Code asks for, so a pin
naming a model the gateway does not serve kills the session at its first prompt.

```bash
bin/controlplane -gateway-url https://litellm.internal:4000 -gateway-key <key>
```

The credential never enters a session. The supervisor opens a loopback listener
per session, and each Claude Code process gets
`ANTHROPIC_BASE_URL=http://127.0.0.1:<its port>` plus a placeholder token. That
matters because **Claude Code's Bash tool inherits the environment** — anything
left there is handed to every command the agent runs, including ones the model
wrote.

**Be precise about what this buys.** The session can still *see* that base URL and
call the proxy — anything in the sandbox can. The intent is that it cannot take
the credential anywhere: a loopback port on an ephemeral number dies with the
session, whereas a leaked `sk-…` works from anywhere, indefinitely, for every
workspace.

**Scrubbing the environment is not enough on its own**, and the reason is worth
keeping. The supervisor holds the key in *its* environment, so a session sharing
its uid reads it straight back out of `/proc/<supervisor>/environ` — one `grep`,
available to every Bash tool call. Measured on 2026-07-31, before it was closed.

So **sessions run at their own uid** (`node`, 1000; the supervisor stays root
because it needs the credential, cgroups and PTY allocation). `environ` is mode
0400 owned by the reader's uid, which is what makes the boundary hold. The
intended fix was `CLAUDE_CODE_SUBPROCESS_ENV_SCRUB`, whose PID namespace does the
same job — but it needs bubblewrap and seccomp privileges **Fargate does not
grant** (§12.6), and a uid difference needs nothing from the platform.

Verified against real Claude Code 2.1.220 in the image: `claude` runs as `node`,
the supervisor as root, and from inside the session the real key has **zero**
matches across every `/proc/*/environ` it can open — only the deliberate
placeholder. `-session-uid 0` disables the boundary, and the supervisor logs a
warning when it is root, holding a credential, and unbounded.

And every call through the proxy is tagged with the workspace and
session from the supervisor's own port mapping, so **unattributed spend is not
possible** — verified from inside a sandbox, where a call carrying a forged
credential *and* forged tags was still recorded against the correct workspace and
session.

The capability is deliberately not fenced off: the agent legitimately has
inference and runs arbitrary code, so a Bash command can always just ask Claude
Code for a completion. What bounds it is a per-workspace budget at the gateway
(§12.4), not the proxy.

## Telemetry

Claude Code's OpenTelemetry is for **audit and behaviour**, not billing — the
gateway is the cost source of record (§12.4). Nothing but OTel answers what the
agent *did*: which tools ran, which the user rejected, which a hook blocked,
whether anyone flipped into bypass mode. It is optional infrastructure, gated on
an endpoint being configured.

Content stays redacted: all four `OTEL_LOG_*` flags are set off **explicitly**,
because `OTEL_LOG_ASSISTANT_RESPONSES` silently falls back to
`OTEL_LOG_USER_PROMPTS` when unset.

## Checking an AWS account

```bash
AWS_PROFILE=… bin/preflight -region us-east-2
```

Verifies each pinned model is genuinely usable. `AUTHORIZED` is not proof — a
model can report authorized and still refuse to invoke — so only the real
`InvokeModel` decides. Failures come with the fix, and the distinctions matter:
an IAM policy gap, a missing inference-profile prefix, and an account that never
completed the First Time Use form all look similar and have nothing in common.

Opus and Haiku are required (Claude Code reaches for Haiku on nearly every turn);
Sonnet only warns.

The same check runs **inside each workspace task**, because the supervisor is the
only component holding the sandbox task role — the credential sessions actually
use. A check run anywhere else would test a different principal (§12.3). Turn it
on with `-bedrock-preflight` on the control plane; the config reaches tasks as
environment, which is what both drivers pass through.

A blocking verdict refuses session creation with the fix attached, rather than
letting Claude Code start and then die on the first prompt:

```
$ curl -X POST localhost:9000/v1/orgs/forty-crimson-windmill/workspaces/w1/sessions
HTTP 424
bedrock is not usable in this workspace: us.anthropic.claude-opus-5 (opus) is not
invocable. the account does not have model access… Submit the Anthropic First Time
Use form… a denial has NO self-service recovery and needs an AWS support case
```

`GET /v1/orgs/{org}/workspaces/{wid}/preflight` serves the structured report. The console
no longer renders it — on the gateway path every workspace reports `skipped`,
so the panel was a permanent nothing — but the endpoint stays, because the
report is what makes a Bedrock failure diagnosable and `curl` is a fine reader
of it. It blocks on *evidence* of breakage, never on the absence of one: a
report that never arrives fails open with a loud log, and throttling is reported
without blocking, since it means the check reached no verdict.

## Session lifecycle

Compute, process and client presence are three independent axes (§2.4). These
move the **process** axis only — stopping a session leaves the workspace task up,
because a sibling session may still be working in it.

```
POST   /v1/orgs/{org}/sessions/{sid}/stop     Ctrl-C/Ctrl-D; ?force=1 is SIGKILL
POST   /v1/orgs/{org}/sessions/{sid}/resume   starts the process with NO client attached
DELETE /v1/orgs/{org}/sessions/{sid}          ends it and drops the conversation
```

Stop keeps the record, because the conversation outlives the process. Resume is a
verb rather than a side effect of attaching, since a console has to be able to
put an agent back to work without becoming its terminal. Delete refuses a running
session without `?force=1` — it is the one unrecoverable verb.

Whether a resumed session gets `--resume` or `--session-id` is decided **by the
supervisor, from the workspace volume**, never by a control-plane flag: the two
flags fail in each other's case, and a replacement task's disk is empty while any
flag we stored would still say "started". §12.7 has the measurements.

## Starting the machine

**Creating a workspace starts its task, and does not wait for it.** `202`, with
the record already saying `starting`:

```
POST /v1/orgs/{org}/workspaces          202 {"id":"w1","status":"starting"}
GET  /v1/orgs/{org}/workspaces/{wid}/events   SSE: a snapshot, then changes
```

A workspace's whole reason to exist is the machine, so asking for one starts it.
That matters more than it sounds on a **shared** workspace: placement used to
happen at the first session, so the first member to want one paid the cold start
for a machine the whole team then used.

**A failed placement lands on the record.** While placement happened inside a
request, a failure *was* the response body. Nobody is waiting now, so it goes to
`workspace.last_error` — and it describes exactly **one attempt**: every status
write clears it and only a failure sets it, because a message that outlived its
attempt reads as the reason for the current state and is not.

**The status answers one question — may a task exist?**

| Where it failed | Status | Reference |
|---|---|---|
| Before dispatch, nothing placed before | `stopped` | none |
| Before dispatch, an older task on the record | unchanged | kept — it is the only handle for stopping it |
| Dispatched, never dialed in | `starting` | kept — it may be slow rather than dead, and this earns it the reconnect grace instead of a second task |

**The event stream is what removed the blank terminal.** `lem` used to post a
session, wait 25 s while the control plane blocked on a task dialing in, and
print nothing — indistinguishable from a hang, and the user's next move is
Ctrl-C, which abandons a placement that was about to succeed. It now tails
`/events` for the duration and says so. Three details are deliberate:

- **It opens with a snapshot**, or a client watching a workspace that is already
  up waits for a transition that has already happened.
- **The snapshot's `last_error` is never reported.** It belongs to the previous
  attempt, the one this command is about to replace — announcing it would report
  a failure that has not happened, on the run most likely to succeed.
- **It polls the record** rather than fanning out in process. A workspace's
  control tunnel is held by exactly one control-plane replica, so an in-memory
  broadcast would deliver nothing to the replica a console happens to be talking
  to, and would do it silently.

None of this is safe on its own — it is safe because of the next section. A
workspace created on a Friday afternoon with nothing to stop it bills until
Monday, which is why placement on create was built *after* the auto-stop
cascade rather than before it.

## Stopping what nobody is using

A Fargate task's memory is fixed at launch, so an OOM kills the **task** — and a
task holds every member's sessions, so one developer's runaway build ends a
colleague's overnight run. That is why these are prerequisites rather than
billing hygiene.

**The supervisor states facts; the control plane decides.** Every 30 s a task
reports its headroom and, per session, how long each of §2.4's four signals has
been quiet. Policy — timeouts, the opt-out, the admission rule — lives in the
Management API, which owns it, acts on it, and outlives the task it would
otherwise be timing.

**Idle means all four conditions, never any three:**

```
no client attached · no PTY output · no model requests · NO TOOL EXECUTING
```

The fourth is the one that matters. A one-hour `make`, a long test run, anything
blocked on the network produces **no output and no model calls**, because Claude
Code is sitting blocked waiting on the tool — so without it the other three go
quiet precisely when the workspace is busiest, and the build is reaped an hour
in. **A running tool is any live descendant of the session** — no start-time
window, because nothing is left for one to exclude: no MCP server can run, and a
real session spawns zero long-lived children of its own. Both are gated by
tests, and an earlier 15 s window was retired after the end-to-end run showed it
missed a tool started in a session's first seconds.

> ⚠️ **That rule holds only while the image ships no MCP servers**, and it is
> gated rather than assumed. Measured against 2.1.220 in the real image on
> 2026-08-02: **MCP servers start lazily**, not with the session — a configured
> stdio server was never spawned by an interactive session sitting at its prompt
> for 40 s, while `claude mcp list` spawned it immediately. A lazily-started
> server starts outside the window and lives forever, so it would read as a tool
> that never finishes and switch idle detection off silently.
> `TestMCPServersWouldBreakToolDetection` fails the moment `image/mcp.json`
> gains one; the fix at that point is to exclude MCP descendants by their
> configured command rather than by age.

Default 2 h, per workspace, overridable per session — `0` is the explicit
opt-out for a session that is *meant* to sit there.

```
PATCH /v1/orgs/{org}/workspaces/{wid}   {"policy": {"idle_timeout_secs": 3600, …}}
PATCH /v1/orgs/{org}/sessions/{sid}     {"idle_timeout_secs": 0}
```

**A session pinned by a background process is kept alive, and reported.** The
conditions split three ways: *busy*, *pinned* (quiet except for a running tool)
and *idle*. Only idle is stopped — pinned covers both the overnight build and
the dev server somebody left up, which look identical from here and where
reaping either destroys work somebody chose to leave running. When a workspace
would be stopping and only a background process holds it open, the console shows
a warning on its row saying how long. The bound is visibility, not enforcement.

**Warm hold** keeps the task up **1 h** after the last session stops, so somebody
who quit and reconnected does not pay a cold start. The deadline is
a **column**, not a `time.AfterFunc`: a timer dies with the process, and a
redeploy mid-hold would leave the task running with nobody timing it — the
failure warm hold exists to prevent, made permanent.

It was 5 min until 2026-08-03, and the change is Phase 6 catching up with the
default. Five minutes sizes a workspace as **one developer's sandbox**; a shared
machine spends the hold on the whole team rather than on whoever quit last, and
anyone stepping away from their desk came back to a cold start. The measurements
carry it: warm attach is **1.7 s** against **22.6 s** cold, and a cold start that
restores an EBS snapshot is nearer **50 s**.

> It is the most expensive default here — an idle 2 vCPU / 8 GiB task is roughly
> **$0.09/h**, so an hour of hold costs about what five minutes cost twelve times
> over. Two things bound it: it is per-workspace overridable, and it only starts
> when the *last* session stops. Note the two axes **stack**: a forgotten session
> is reaped at `idle_timeout_secs` (2 h) and the task then holds for another
> hour, so one abandoned session can bill ~3 h.
>
> **Existing workspaces keep whatever they have.** The migration moves the
> column default, not the rows — nothing can tell "the customer chose 300" apart
> from "the customer got the old default", and silently overriding the first to
> fix the second is the worse mistake.

**Admission control** refuses a session the task cannot hold, naming the policy
and the numbers. Default `min_free_memory_pct` at 20%, **with a constant
768 MB floor** — a percentage adapts across instance sizes, but a Claude Code
session's footprint does not scale with the task, so 20% of a 2 GiB task is
~410 MB, less than one session. It runs *before* placement, so a refusal has not
first cost a cold start.

**Four rules about what we cannot see**, each with a test that fails when it is
inverted:

| Rule | Because |
|---|---|
| Unknown headroom **admits** | `memoryHeadroomMB()` reports a zero limit on darwin and on an unlimited cgroup; refusing fails closed against our own operators |

**Where the limit comes from, and why it is not just the cgroup.** Inside a
Fargate task `/sys/fs/cgroup/memory/memory.limit_in_bytes` is
`9223372036854771712` — the unlimited sentinel — because the 8 GiB cap is
enforced at a cgroup the container cannot see. Reading only that made every rule
above correct and *inert*: headroom was permanently "unknown", and unknown
admits. So `taskMemoryLimitBytes()` takes the cgroup where it is genuinely set
(docker `--memory`, local runs) and otherwise asks
`$ECS_CONTAINER_METADATA_URI_V4/task` → `"Limits":{"Memory":8192}`. Found and
fixed on 2026-08-03; before it, `limit_mb` read `0` and `total_mb` reported the
host's ~15.7 GB as the task's memory.
| A **stale** report against a **known** limit falls back to `max_sessions` | The numbers exist and are merely old; trusting them oversubscribes a task whose supervisor is wedged |
| **Silence is not idle** | A wedged tunnel must never read as an idle workspace. Three missed intervals and the sweeper does nothing |
| An idle stop **never advances the generation** | It rotates the credential out from under a task that is alive and about to reconnect, and the `ecs` driver then adopts that same task and reports success (§2.8) |

The first two and the last two are the same principle pointing opposite ways:
*unknown is not a value*. For an activity signal, "unknown" and "busy" have the
same safe reading; for headroom they have opposite ones, which is the only
reason they look like different rules.

One property that fell out rather than being built: the sweeper acts only on
workspaces whose report is in **its own** memory, and a task holds its control
tunnel with exactly one control-plane process — so replicas partition instead of
racing, with no lease and no leader election.

## Authentication

**Mandatory.** `-auth-issuer` and `-auth-audience` are required, and the control
plane refuses to start without them — the same posture as `AssertNoBypassRLS`,
and for the same reason: this process is multi-tenant, so a deployment that
cannot tell two customers apart has nothing left to enforce.

There was an unauthenticated mode until Phase 4. It existed to spare the test
suite an identity provider, it was reachable by *omitting* a flag, and it
attributed every request — console and CLI alike — to one fixed user. The tests
now sign their own tokens against a local key set (`internal/authtest`), which
costs an RSA key and validates through exactly the path a real deployment uses.

Set up the local identity provider — Logto + Mailpit, config fully scripted —
with [`auth-stack/`](auth-stack/README.md):

```bash
docker compose -f auth-stack/docker-compose.yml up -d
bun auth-stack/seed.ts > auth-stack/.env.generated
set -a; . auth-stack/.env.generated; set +a
```

The control plane then validates bearer tokens on the management API
(`internal/auth`: signature via JWKS, issuer, audience, expiry). All four flags
default to the environment above, so sourcing that file is enough:

```bash
bin/controlplane -auth-issuer "$LEMUL_AUTH_ISSUER" -auth-audience "$LEMUL_AUTH_AUDIENCE" \
                 -auth-cli-client-id "$LEMUL_CLI_CLIENT_ID" \
                 -auth-console-client-id "$LEMUL_CONSOLE_CLIENT_ID" …
lem login          # OAuth device flow (RFC 8628); approve in a browser
```

`lem login` also relays its **ID token** to `PUT /v1/identity`. An access token
minted for an API resource carries a subject and no identity claims, so a signed
assertion is the only trustworthy source of an email — and the email is what
turns `forty-crimson-windmill` into "Dario's Org". The control plane checks the
ID token's subject against the access token's, so a caller can name only itself.
The two client ids are both configured because an ID token is audienced to
whichever client asked for it, and the console and CLI are different clients.

**The CLI is told nothing.** `lem login` reads `GET /v1/auth/config` from the
control plane at `-server` and learns the issuer, the audience and its own client
id from there, then caches them with the token so no later command pays a round
trip. Those are properties of the deployment, not of a laptop: a client that has
to be handed them cannot be pointed at two control planes without two sets of
environment variables, and nothing catches the mismatch — the token gets minted
by the wrong identity provider and the only symptom is a 401. It sharpens
further once each organization brings its own identity provider, which only the
control plane knows about.

The endpoint is unauthenticated by necessity — it is what a client reads *before*
it has a token — and carries no secret: `lem` is a public OAuth client, so its
client id already travels in every device-flow request. `LEMUL_AUTH_ISSUER`,
`LEMUL_CLI_CLIENT_ID` and `LEMUL_AUTH_AUDIENCE` still override discovery, all
three or none; a partial set is refused rather than merged, because the hybrid
fails much later at token exchange with an error that points at neither half.

Three endpoints stay unauthenticated on purpose: this one, `/v1/tunnel/*`, where the runner
and workspace tasks present their own derived credentials and have no user to
be, and `/v1/sessions/{sid}/attach`, which carries a single-use attach
credential — a browser cannot set an `Authorization` header on a WebSocket
handshake, so requiring one would make the console's viewer impossible. Attach
is also the one endpoint not nested under an organization, for the same reason:
its credential is the only authority it has, so the organization travels inside
the signature rather than in the path.

**Authorisation now exists, and it is organization-shaped.** Every request
resolves to a user and an organization; a caller who is not a member of the
`{org}` in the URL gets a 404, and the database refuses independently through
row-level security. An organization owner sees every workspace in it; a plain
member sees what they own or were added to.

**§2.5's owner-scoped attach is closed.** A session's keyboard belongs to
whoever started it: endpoint negotiation gives a control credential to that user
alone. A workspace or organization owner may ask for a **viewer** credential and
gets one — refused, not silently downgraded, if they asked to drive — and
anybody else gets a 404. Watching is conspicuous by construction, because an
attach bumps the attacher count the session's owner sees.

**Authorisation is workspace-shaped as well as organization-shaped.** Belonging
to an organization is not belonging to every workspace in it: a caller with no
membership gets 404 on every `{wid}` route, the explorer trio included — those
read file trees, process lists and command lines, and until Phase 4 had no check
at all.

## Operator console

```bash
cd console && bun install && bun run dev     # http://127.0.0.1:3000
```

TanStack Start (SSR) over the control plane's JSON API. A workspace opens as a
tabbed page — **overview** (CPU, RAM, disk and network, each with the saturation
signal that matters for a sandbox), **sessions** with their lifecycle buttons,
**members**, and behind a flag **filesystem** and **processes**. See
[`console/README.md`](console/README.md).

The organization page is the **workspace table**, a **create form** and an
**organization settings** page. Three things about them are decisions rather
than layout:

- **The table has a STATE column again**, having once been dropped as noise. It
  was noise while the only thing that placed a task was somebody waiting for
  one; placement happens at create now, so a workspace spends its first 20–60 s
  coming up with nobody having asked for a session. It separates `starting`,
  **could not start** (`last_error` — the row did not merely stop, it tried),
  **no task** (recorded `active` with no tunnel) and **warm hold**.
- **Creating is a form, not a name prompt**, because a workspace has an access
  scope and it is the one field where the safe answer and the useful answer
  differ. A prompt that asks only for a name creates every workspace at `owner`,
  which on a product whose premise is a shared machine is the wrong default to
  make invisible. The ACCESS column renders the same choice as a sentence.
- **Settings is gated on the org-owner role in two places, for two reasons.**
  Here it decides what to render, which is UX; the control plane refuses the
  PATCH independently, which is the enforcement — a route guard in the console
  protects nothing, because the server functions behind it are RPC endpoints
  reachable by direct POST whatever the router is showing.

**It binds to loopback deliberately.** That is a control in `vite.config.ts`,
not a default — one fewer listener while the deployment story is still forming.

The header carries an **organization switcher** when there is more than one to
switch to. It reads the current organization from the URL rather than from
client state, which is the reason the organization is a path segment at all: a
switcher backed by state can disagree with the page under it, and that
disagreement acts on the wrong customer's workspace.

Behind `FF_VIEW_SESSION=1` (off by default), `view` on a running session opens a
**read-only** xterm.js terminal on the existing `?mode=viewer` attach path — no
capability the CLI did not already have, with input dropped by the relay *and*
the supervisor. It never resizes the session, since the PTY has one size and
honouring the browser window would reflow the controller's Claude Code. The flag
gates the route as well as the button, so it is a control rather than decoration.
Driving still means `lem`.

It is served by its own process, not the Go binary. Backing endpoints:

```
GET /v1/status                                  what this deployment is
GET /v1/orgs                                    organizations the caller belongs to
PUT /v1/identity                                the caller's ID token, for owner columns
GET    /v1/orgs/{org}/settings                  members_can_create_workspaces
PATCH  /v1/orgs/{org}/settings                  owner only for write
GET /v1/orgs/{org}/workspaces                   records, plus whether a task is connected
POST /v1/orgs/{org}/workspaces                  202; name + access_scope, placed in the background
GET /v1/orgs/{org}/workspaces/{wid}
GET /v1/orgs/{org}/workspaces/{wid}/fs          directory listing (metadata, never contents)
GET /v1/orgs/{org}/workspaces/{wid}/processes   what is running, attributed to sessions
GET /v1/orgs/{org}/workspaces/{wid}/resources   CPU / memory / disk / network, with history
```

The last three back the **workspace explorer**, behind `FF_WORKSPACE_EXPLORER`
(off by default). They are reads that never place a task, and there is no write
counterpart on the supervisor to call — read-only is structural rather than
enforced. The path jail lives in the supervisor (`internal/supervisor/fsjail.go`)
because that process runs as root: `/proc/self/environ` there holds the gateway
key, so containment is policy and this is the only copy of it.

## Not yet built

`--fork`, which would let
somebody branch a colleague's conversation instead of watching it · end-to-end
encryption, which the Relay split is the precondition for (§2.7) · asymmetric
credential signing, so the relay holds a public key rather than the key that
mints attach credentials (§12.9). Tracked as the §8 checklist.

The `ecs` driver and `terraform/` have been applied and verified against a real
account several times, most recently on **2026-08-03 for the workspace volume**:
the volume attaches, the auto-stop cascade snapshots it, and a restored
workspace comes back with a member's home and their conversation. That round
also found and fixed the restore being broken outright, the volume never being
released, and admission control being inert on Fargate.

**The vendor half now has a stack, and has not yet been hosted.**
[`deploy/`](deploy) builds images for the control plane, the relay and the
console and runs all three behind Caddy with Logto, Postgres and Mailpit —
verified end to end locally on 2026-08-03. It is deliberately separate from
`terraform/`: that module is what a **customer** applies in **their** account and
nothing in it listens inbound, while everything in `deploy/` does, because it is
the endpoint their runner dials out to (§2.2). `image/` still builds only the
workspace sandbox and the runner, which are the two things that belong on the
customer's side of that line.

Every AWS round so far has instead published a developer's machine through
temporary tunnels. What is still missing is the host: an instance, an address
and the module to create them.

**The model path is proven** (2026-08-01), though not on Fargate. `e2e/gateway_test.go`
places the real sandbox image with the `docker` driver, attaches through our own
tunnel and relay, and completes a real Claude Code turn through the supervisor's
loopback proxy to LiteLLM. It asks `7919 × 13` and waits for `102947` —
deliberately an answer the test never types, because the first version asked for
a word and passed in 3.3 s by matching **its own keystrokes echoing back** from
the input box, before any model had answered. Spend is then read from
`/spend/logs` and asserted to carry `workspace:` and `session:` tags the session
cannot forge. What is still unexercised is that path on **Fargate**, where the
gateway has to be genuinely reachable from the task.
