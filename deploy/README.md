# deploy — the vendor side

The control plane, the relay, the console, and the identity provider they all
depend on.

**This is the other half of §2.2's line, and it is a separate directory for that
reason.** [`terraform/`](../terraform) is the module a **customer** applies in
**their** account, and nothing in it listens inbound. Everything here listens,
because this is the endpoint their runner dials out to. A customer reading
`terraform/` should find nothing of ours running in their VPC, and mixing the
two would quietly destroy the one thing that module demonstrates.

```bash
cp .env.example .env      # set BASE and the two passwords
./build.sh                # three images
./up.sh                   # identity first, seed, then the rest
```

## One hostname per service, and no DNS to configure

`BASE` is `<public-ip>.sslip.io`. sslip.io is a public wildcard resolver that
reads the address out of the hostname — `1.2.3.4.sslip.io` resolves to `1.2.3.4`,
and so does `anything.1.2.3.4.sslip.io`. There is no zone to create, no record to
publish and nothing to renew, which is the whole reason it is here rather than a
domain. Caddy then gets a real Let's Encrypt certificate over HTTP-01, so this
costs no TLS.

| | |
|---|---|
| `console.$BASE` | the operator console |
| `cp.$BASE` | Management API — what a runner and every workspace task dials |
| `relay.$BASE` | the session data path |
| `gw.$BASE` | the inference gateway — **inference routes only**, see below |
| `gwadmin.$BASE` | LiteLLM's console and management API |
| `auth.$BASE` | Logto's OIDC endpoint |
| `admin.$BASE` | Logto's admin console — **guarded** |
| `mail.$BASE` | Mailpit — **guarded**; only used when SES is not configured |

## Who is guarded, and why it is not uniform

`admin.` and `mail.` sit behind HTTP basic auth. `gw.` and `gwadmin.` do not,
and the difference is the point rather than an inconsistency:

- **Logto's admin console is CLAIMABLE on a fresh deployment.** Verified, not
  assumed: a new stack answers `/` with a welcome page and an empty users table,
  so the first visitor becomes the administrator of the identity provider
  everything else trusts. It has no authentication of its own until somebody
  claims it, so the guard *is* its authentication.
- **Mailpit is an open inbox** that happens to hold every sign-in code, and
  sign-in is passwordless email — so reading it is signing in as anyone.
- **LiteLLM has its own**: a master key for the API and `UI_USERNAME` /
  `UI_PASSWORD` for the console. A second layer there bought little and cost the
  whole UI, because the console is a single-page app and a basic-auth challenge
  on every XHR re-prompts the browser until it is unusable.

`gw.` is separate from `gwadmin.` for a different reason again. It has to be
reachable from the **customer's** VPC — a supervisor inside a workspace task
dials it — so it is reachable from the internet, and LiteLLM serves `/ui` and
`/key/*` on the same port. `gw.` therefore allowlists inference
(`/v1/messages`, `/v1/messages/count_tokens`, `/v1/models`) and 404s everything
else, mirroring `internal/gateway/proxy.go`'s own allowlist. Those two answer
different questions: the proxy inside the task bounds what a **session** may ask
for, this bounds what **the internet** may.

> ⚠️ **The master key is the inference credential AND the admin credential.**
> Every workspace task holds it, and the runner can read it from
> `ecs:DescribeTasks` (§13). Anyone holding it can mint keys and edit models.
> The fix is a LiteLLM **virtual key** for the supervisor, not another layer in
> front of Caddy.

Subdomains rather than one host with paths, because the control plane and the
relay both serve `/v1/…`: path routing would need a rule per endpoint and would
break the moment either grew one. It also keeps the two-service split visible in
the URL, which is the thing a customer is being asked to believe.

## The bootstrap is a cycle, which is why `up.sh` exists

The console and the control plane cannot start without Logto application ids and
secrets. Those do not exist until the seed runs. The seed cannot run until Logto
is up **and reachable at its public name**, because the ids it mints are bound to
the URLs it is told about. So `up.sh` starts identity and the proxy, waits, seeds,
merges the result into `.env`, and only then starts everything that depends on it.

Re-running is safe: the seed converges what it can, and the merge **replaces**
the keys it owns rather than appending — two values for one key would let the
stale one win depending on who read the file.

## Things that cost time here, and will again

- **Compose interpolates the whole file even when starting a subset.** The five
  seed-owned variables therefore cannot be `${VAR:?...}`; a guard on them refuses
  to start the very identity provider that mints them. `up.sh` checks instead —
  after re-reading `.env`, because the merge wrote to the *file*, not to the
  running shell.
- **`LEMUL_CLI_CLIENT_ID` and `LEMUL_CONSOLE_CLIENT_ID` are not `LEMUL_AUTH_*`,**
  unlike the issuer and audience beside them. Getting them wrong is silent: the
  control plane starts and `GET /v1/auth/config` simply omits `client_id`, so
  `lem login` has no client to present.
- **Distroless runs as uid 65532 and a named volume is created root-owned**, so
  the control plane cannot create `signing.key` and the relay then reports it
  missing. Neither image has a shell, hence the one-shot `state-init` container.
- **`SEED_TLS_INSECURE` is the inverse of `NODE_TLS_REJECT_UNAUTHORIZED`.**
  Passing one into the other reads correctly and does the opposite.
- **A public hostname does not resolve to the proxy from inside a container** —
  fatally when `BASE` is a loopback address, where the seeder dials its own
  loopback. Hence the seeder's `extra_hosts: host-gateway`.
- **LiteLLM needs a DATABASE** for its admin UI and `/spend/logs`. Without one
  it serves inference perfectly well and answers *"Not connected to DB!"* at the
  login form, which reads as a wrong password. `/spend/logs` is also where
  decision #12's per-workspace cost attribution comes from, so this is not only
  a console convenience. Its UI credentials are `UI_USERNAME`/`UI_PASSWORD`,
  **not** the master key.
- **`aws s3 cp` replaces a file; Docker bind-mounts the INODE.** Updating a
  mounted config that way leaves the container serving the old one, and
  `caddy reload` cheerfully reports *"config is unchanged"*. Recreate the
  container, do not reload it.
- **Validate a Caddyfile before deploying it.** An invalid one does not degrade,
  it refuses to start — and Caddy is the front door, so the console, control
  plane, relay and gateway all go down together:
  `docker run --rm -v $PWD/Caddyfile:/etc/caddy/Caddyfile:ro -v <stub creds>:/etc/caddy/auth.creds:ro -e BASE=x.sslip.io caddy:2-alpine caddy validate --config /etc/caddy/Caddyfile`
- **Values with spaces must be quoted in `.env`** (`CADDY_TLS="tls internal"`).
  `up.sh` sources it with the shell, which otherwise tries to run the second
  word. This bit twice — the second time in the Terraform template, on the one
  line that *assembles* a value (`CADDY_GLOBAL="email …"`) rather than copying it.
- **The basic-auth hash never goes through compose.** A bcrypt hash is full of
  `$`; compose interpolates `environment:` always and `env_file` depending on its
  version, so the same file produced a working guard locally and one that
  rejected the correct password on the deployed host. Caddy `import`s the
  credentials from a file instead.
- **A bind-mounted file that does not exist yet becomes an empty one.** Docker
  creates the mount target at container-create time, so a container built before
  `caddy-auth.creds` existed holds a 0-byte file forever — `import` then yields
  no users and the guard rejects everybody. `up.sh` writes the file before the
  first `compose up`, but a container that predates it needs
  `docker compose up -d --force-recreate caddy`.

## Running it locally

`BASE=127.0.0.1.sslip.io` and `CADDY_TLS="tls internal"`. Let's Encrypt cannot
validate a loopback name because it cannot reach your laptop to do it, so Caddy
issues from its own CA and `SEED_TLS_INSECURE=1` lets the seed accept it. Browsers
will warn; `curl -k` will not care. **Set both back for anything public** — with a
real certificate there is nothing to excuse, and leaving the seed insecure would
let it talk to anything claiming to be Logto.

## The one volume to back up

`state`, which holds `signing.key`. Every workspace, attach and runner credential
in every customer account is derived from it, so losing it orphans every running
task — and the control plane will say so by generating a fresh one at boot.
