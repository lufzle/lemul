# auth-stack

Local identity provider for the console and `ourcli`: **Logto** + Postgres +
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

Only the **server** side reads this file. `ourcli` needs nothing exported:

```bash
ourcli login                     # asks the control plane, then device flow
```

It fetches `GET /v1/auth/config` from whatever `-server` points at and learns the
issuer, the audience and its own client id from there. The three `LEMUL_*`
variables still override it, all three or none, which is for pointing the CLI at
an identity provider the control plane does not know about — not for normal use.

Leave `-auth-issuer` unset and everything behaves as it did before there was any
authentication — which is what keeps `go test ./... -race` and a bare local run
working without this stack standing next to them.

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
