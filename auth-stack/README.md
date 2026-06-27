# auth-stack

Local identity provider for the console and `lem`: **Logto** + Postgres +
**Mailpit**, so passwordless email sign-in works with no real mailbox and
nothing leaving the machine.

```bash
docker compose -f auth-stack/docker-compose.yml up -d
bun auth-stack/seed.ts > auth-stack/.env.generated     # config is scripted, no clicking
```

| | |
|---|---|
| Logto OIDC | `:3001` — issuer, JWKS, token, device authorization |
| Logto admin console | `:3002` |
| Mailpit UI | `:8025` — the sign-in codes land here |

`.env.generated` holds the console's client secret and is gitignored. Re-running
the seed is safe: it converges what it can and reports what it did.

## The bootstrap has no manual step

Logto's own `db seed` creates an M2M application `m-default` holding the
`machine:mapi:default` role, so the credential that manages the default tenant
already exists on a fresh volume. The seed script reads its secret straight from
Postgres. Two things about it cost real time to work out:

- **`m-default` belongs to the `admin` tenant**, so its token comes from the
  admin endpoint (`:3002`), while the Management API it opens is served from
  `:3001`. Asking `:3001` for the token answers `invalid_client`, which reads
  like a wrong secret rather than a wrong port.
- **The client secret is not on the application object.** `applications.secret`
  holds an `#internal:` placeholder and the create response carries no usable
  value, so reading either yields `undefined` — which is written into an env
  file as the literal string and then fails at the token exchange as
  `oidc.invalid_client`, long after the sign-in page has appeared and worked.
  Secrets live at `GET /applications/{id}/secrets`.

## What the seed configures

- **API resource** `https://api.lemul.local` — the `aud` the Go control plane
  validates.
- **`lemul console`** — Traditional Web app, redirect to
  `http://localhost:3000/api/auth/callback`.
- **`ourcli`** — Native app with `customClientMetadata.isDeviceFlow` for RFC 8628.
  The CLI binary was renamed to `lem`; the Logto **application** keeps the old
  name, because renaming it mints a new client id and orphans the old app.
- **SMTP connector** → `mailpit:1025`.
- **Sign-in experience** → email verification code, no password anywhere.

First-party apps skip the consent screen in Logto by default, so there is
nothing to switch off for that.

### Two constraints on the `ourcli` app worth knowing

- **Device flow is fixed at creation.** A PATCH answers
  `application.device_flow_not_changeable`, so the seed *deletes and recreates*
  the app when the flag is wrong rather than pretending to converge. Safe here:
  it is a public client and nobody holds its secret.
- **It still needs a redirect URI.** Device flow never uses one — that is the
  point of it — but Logto validates the application shape anyway and refuses
  `/device/auth` with `redirect_uris must contain members`.

## Wiring it up

```bash
set -a; . auth-stack/.env.generated; set +a

bin/controlplane …               # picks up all three LEMUL_* as flag defaults
cd console && bun run dev        # reads LOGTO_* from the same file
```

Only the **server** side reads this file. `lem` needs nothing exported:

```bash
lem login                        # asks the control plane, then device flow
```

It fetches `GET /v1/auth/config` from whatever `-server` points at and learns the
issuer, the audience and its own client id from there. The three `LEMUL_*`
variables still override it, all three or none, which is for pointing the CLI at
an identity provider the control plane does not know about — not for normal use.

**This stack is not optional for a local run.** `-auth-issuer` and
`-auth-audience` are required and the control plane refuses to start without
them; leaving them unset used to fall back to a fixed dev principal, which meant
an unauthenticated multi-tenant control plane was one omitted flag away.

The **test suite** still needs nothing from here. It signs its own tokens against
a local key set (`internal/authtest`) — real RS256, validated through
`internal/auth`'s ordinary path — so `go test ./... -race` never waits on Logto.

## Sharp edges

- **Logto signs with ES384.** `go-oidc` accepts RS256 only unless told
  otherwise, and the mismatch surfaces as `unexpected signature algorithm`
  against a token that is otherwise perfectly valid — it reads as a broken token
  rather than a missing setting. `internal/auth` lists the asymmetric algorithms
  explicitly, and deliberately excludes the HMAC family.
- **The `resource` parameter must be repeated on the token request**, not only
  on the authorization request. Without it Logto issues an *opaque* token
  instead of a JWT, so sign-in appears to succeed and the API rejects the result
  with nothing connecting the two symptoms.
- **Mailpit is reached as `mailpit:1025`**, not localhost: Logto dials it from
  inside its own container.

## Custom JWT claims do not work on self-hosted Logto

Adding user claims (an email, say) to an **access token** via
`PUT /api/configs/jwt-customizer/access-token` is a **Logto Cloud** feature. The
OSS build accepts the request, answers `201`, stores the script, and serves it
back on `GET` — and then never runs it. The only evidence anywhere is one line
in the container log:

```
warn Early terminate `deployJwtCustomizerScript` since we do not provide
     dedicated computing resource for OSS version.
```

This matters because an access token minted for an API resource carries
`aud, client_id, exp, iat, iss, jti, scope, sub` and nothing else — no email,
no name. That is OAuth working as designed: identity claims belong to the ID
token and userinfo, and the API is meant to know a caller as a stable opaque
subject. So the control plane records `sub` as a workspace's owner, and the
console shows `gihv2soom1by` where a person expects `dario@lemul.local`.

Resolving a subject to a name needs Management API credentials — the lookup is
for *arbitrary* users, so a caller's own token cannot do it — which is a real
decision about what to hand a component, not a formatting fix.

## Two client ids, and the one that was missing

`.env.generated` names the console's Logto application **twice**, and they are
not interchangeable:

| Variable | Read by | For |
|---|---|---|
| `LOGTO_APP_ID` | the console | signing a person in |
| `LEMUL_CONSOLE_CLIENT_ID` | the control plane | deciding whose ID tokens `PUT /v1/identity` accepts |

The second was missing until 2026-08-02, and its absence is worth recording
because of **how it presented**. An ID token audienced to a client the control
plane has never been told about is rejected as `id token is not audienced to a
known client`, so the console's identity relay failed on every refresh. What an
operator saw was owner columns full of raw uuids and an organization still
named after its own slug — neither of which points at a missing environment
variable, and both of which look exactly like the "an access token carries no
identity claims" limitation described above, which is real and had already been
written up. A known limitation is a very good hiding place for a
misconfiguration that produces the same symptom.

The CLI hid it from the other side: `LEMUL_CLI_CLIENT_ID` *was* emitted, so
`lem login` set an email perfectly well, and the path anyone exercising the
stack by hand used most was the one that worked.
