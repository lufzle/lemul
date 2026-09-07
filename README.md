# lemul

[![CI](https://github.com/lufzle/lemul/actions/workflows/ci.yml/badge.svg)](https://github.com/lufzle/lemul/actions/workflows/ci.yml)
[![License: AGPL v3](https://img.shields.io/github/license/lufzle/lemul)](LICENSE)

Claude Code, in a sandbox, in **your** AWS account. Inference bills to your
Bedrock (or to a gateway you run). Nothing inbound in that account: tasks dial
out.

> **Alpha.** Try at your own risk. The API, the disk layout, and the ops story
> will change. Do not put work here that you cannot afford to lose.

## What you get

An **organization** is a tenant. You can belong to several. Workspaces and
sessions live under `/v1/orgs/{slug}`.

A **workspace** is a shared machine (one Fargate task), not a private laptop.
Several people can hold sessions on it. Each session is a uid, a home, and its
own Claude Code state. `/workspace/shared` is the common directory.

A cold start is about 23 seconds. Joining a workspace that is already up is
about 2.

## How it is split

The control plane is two processes. They share a signing key and nothing else.

```
customer VPC                         your control plane
┌────────────────────────────┐       ┌─────────────────────┐
│ runner  ── control ────────┼─ WSS ▶│ Management API :9000│
│                            │       │  store, authz       │
│ workspace task             │       └─────────────────────┘
│   supervisor ─ control ────┼─ WSS ───────▲
│              ─ data ───────┼─ WSS ▶┌─────────────────────┐
│     PTY per session        │       │ Relay          :9001│
└────────────────────────────┘       │  bytes only         │
                                     └──────────▲──────────┘
                                                │
                                             lem / console
```

| Binary | Job |
|---|---|
| `lem` | Local client. `Ctrl-]` detaches. |
| `controlplane` | Directory, workspaces, placement, authz. |
| `relay` | PTY bytes. No database. |
| `runner` | Places tasks in the customer VPC. Control path only. |
| `supervisor` | One per workspace. Owns the PTYs. Dials out twice. |
| `preflight` | Can this AWS account actually invoke the pinned models? |

Inference goes through a **customer-hosted gateway** (LiteLLM or similar). The
supervisor holds the credential and proxies each session on loopback, so the
session never sees the real key.

## Quick start (local)

You need Go, Docker, and [Bun](https://bun.sh).

```bash
go build -o bin/ ./cmd/...

docker compose -f auth-stack/docker-compose.yml up -d
bun auth-stack/seed.ts > auth-stack/.env.generated
set -a; . auth-stack/.env.generated; set +a

docker run -d --name lemul-pg -e POSTGRES_PASSWORD=postgres -p 5432:5432 postgres:17-alpine
ADMIN='postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable'
psql "$ADMIN" -c "CREATE ROLE lemul_app LOGIN PASSWORD 'lemul_app_password' NOBYPASSRLS"
psql "$ADMIN" -c "GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO lemul_app"

export LEMUL_DATABASE_URL='postgres://lemul_app:lemul_app_password@localhost:5432/postgres?sslmode=disable'
export LEMUL_SCHEMA_URL="$ADMIN"

# Terminal 1 — Management API. -apply-schema uses LEMUL_SCHEMA_URL (DDL).
# The app role must not have BYPASSRLS.
bin/controlplane -addr :9000 -state-dir ./.state -apply-schema \
  -session-cmd claude -relay-url ws://localhost:9001

# Terminal 2 — same -state-dir. The relay verifies; it will not mint a key.
bin/relay -addr :9001 -state-dir ./.state

# Terminal 3 — approve the device code; the email lands in Mailpit at :8025.
bin/lem login
bin/lem orgs

eval "$(bin/controlplane -state-dir ./.state -print-runner-env <org-slug>)"
bin/runner -control-plane ws://localhost:9000 -supervisor ./bin/supervisor

bin/lem workspace create w1
bin/lem --workspace w1
```

Grant the app role on tables **after** the first `-apply-schema`; the `GRANT`
above only covers tables that already exist.

Point `lem` at a deployed API with `LEMUL_SERVER` (or `-server`). Login tokens
are one file: switching servers without setting that variable looks like a
broken login.

```bash
export LEMUL_SERVER=https://cp.<your-host>
lem login
lem workspace ls
```

## `lem`

```
lem                         new session in your only reachable workspace
lem --workspace w1          new session in w1
lem --session <id>          attach (or resume if the process is stopped)
lem --session <id> --viewer watch, read-only
Ctrl-]                      detach

lem workspace ls | create [name] | rm <name>
lem session ls [--workspace w1]
lem orgs
lem invite                  code that adds someone to this organization
lem join <code>
```

Bare `lem` starts a **new** session. Reattach with `--session`. With several
organizations or workspaces, pass `--org` / `--workspace`; the CLI will not
guess.

## Deploy

Two sides, on purpose:

| Path | Who | What |
|---|---|---|
| [`terraform/`](terraform) | Customer AWS account | Runner + workspace tasks. Nothing listens inbound. |
| [`deploy/`](deploy) | You (the operator) | Control plane, relay, console, identity, gateway. |
| [`docs/onboarding-bedrock.md`](docs/onboarding-bedrock.md) | Customer | Bedrock model access, before any apply. |

```bash
cd terraform && terraform init
terraform apply -var-file=example.tfvars
```

Console: [`console/README.md`](console/README.md). Identity for local work:
[`auth-stack/README.md`](auth-stack/README.md). Telemetry:
[`otel-stack/README.md`](otel-stack/README.md).

## Tests

Docker is required for `internal/store`, `internal/mgmtapi`, and `e2e`. Without
it those packages skip.

```bash
make test    # go test -race -p 1
make lint
```

`-p 1` is not optional: those packages start Postgres containers, and running
them in parallel fails in ways that look unrelated.

## License

[GNU Affero General Public License v3.0](LICENSE). If you run a modified
version as a network service, you must offer the corresponding source to its
users.

Issues and pull requests: [CONTRIBUTING.md](CONTRIBUTING.md).
Security reports: [SECURITY.md](SECURITY.md) — not the public issue tracker.
