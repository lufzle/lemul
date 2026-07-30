#!/usr/bin/env bun
/**
 * Configures the local Logto tenant from nothing to usable.
 *
 * Fully scripted, including the bootstrap. Logto's own `db seed` creates an M2M
 * application `m-default` holding the `machine:mapi:default` role, so the
 * credential that manages the default tenant already exists on a fresh volume --
 * there is no click-through step to get started. Two non-obvious things about
 * it, both of which cost time to discover:
 *
 *   - `m-default` belongs to the **admin** tenant, so its token comes from the
 *     ADMIN endpoint (:3002), not the one the app signs in against (:3001).
 *     Asking :3001 answers `invalid_client`, which reads like a bad secret.
 *   - The Management API itself is served from :3001 under /api, with the token
 *     minted at :3002. The two ports are not interchangeable.
 *
 * Idempotent: every step checks for what it would create first, so this is safe
 * to re-run against a stack that is already configured.
 */
import { $ } from 'bun'

const ENDPOINT = process.env.LOGTO_ENDPOINT ?? 'http://localhost:3001'
const ADMIN_ENDPOINT = process.env.LOGTO_ADMIN_ENDPOINT ?? 'http://localhost:3002'
const MANAGEMENT_RESOURCE = 'https://default.logto.app/api'

/** The audience the Go control plane validates. Arbitrary, but it must match. */
const API_INDICATOR = process.env.LEMUL_API_INDICATOR ?? 'https://api.lemul.local'
const CONSOLE_REDIRECT =
  process.env.CONSOLE_REDIRECT ?? 'http://localhost:3000/api/auth/callback'
const CONSOLE_POST_LOGOUT = process.env.CONSOLE_POST_LOGOUT ?? 'http://localhost:3000/'

type Json = Record<string, unknown>

async function bootstrapSecret(): Promise<string> {
  if (process.env.LOGTO_M2M_SECRET) return process.env.LOGTO_M2M_SECRET
  // Read it from the database rather than asking the operator to paste it. This
  // is a local dev stack; the alternative is a manual step in a script whose
  // whole point is not having one.
  const out =
    await $`docker exec auth-stack-db-1 psql -U postgres -d logto -t -A -c ${"select secret from applications where id='m-default';"}`.text()
  const secret = out.trim()
  if (!secret) {
    throw new Error(
      'could not read the m-default secret; is the auth-stack running? ' +
        'docker compose -f auth-stack/docker-compose.yml up -d',
    )
  }
  return secret
}

async function managementToken(secret: string): Promise<string> {
  const res = await fetch(`${ADMIN_ENDPOINT}/oidc/token`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/x-www-form-urlencoded',
      Authorization: `Basic ${btoa(`m-default:${secret}`)}`,
    },
    body: new URLSearchParams({
      grant_type: 'client_credentials',
      resource: MANAGEMENT_RESOURCE,
      scope: 'all',
    }),
  })
  const body = (await res.json()) as { access_token?: string; error_description?: string }
  if (!res.ok || !body.access_token) {
    throw new Error(`management token: ${res.status} ${body.error_description ?? ''}`)
  }
  return body.access_token
}

function api(token: string) {
  return async function call<T = Json>(
    path: string,
    init?: { method?: string; body?: unknown },
  ): Promise<T> {
    const res = await fetch(`${ENDPOINT}/api${path}`, {
      method: init?.method ?? 'GET',
      headers: {
        Authorization: `Bearer ${token}`,
        ...(init?.body ? { 'Content-Type': 'application/json' } : {}),
      },
      body: init?.body ? JSON.stringify(init.body) : undefined,
    })
    const text = await res.text()
    if (!res.ok) throw new Error(`${init?.method ?? 'GET'} ${path} -> ${res.status}: ${text}`)
    return text ? (JSON.parse(text) as T) : (undefined as T)
  }
}

type Application = {
  id: string
  name: string
  type: string
  oidcClientMetadata: { redirectUris: string[]; postLogoutRedirectUris: string[] }
  customClientMetadata?: { isDeviceFlow?: boolean }
}

/**
 * The client secret, which is NOT on the application object.
 *
 * `applications.secret` holds an `#internal:` placeholder and the create
 * response carries no usable value, so reading either yields `undefined` --
 * which is then happily written into an env file as the literal string
 * "undefined" and fails much later as `oidc.invalid_client` during the token
 * exchange, long after the sign-in page has appeared and worked. Secrets have
 * lived in their own collection since Logto 1.x; ask for them explicitly.
 */
async function appSecret(call: ReturnType<typeof api>, appId: string): Promise<string> {
  const secrets = await call<Array<{ name: string; value: string }>>(
    `/applications/${appId}/secrets`,
  )
  const secret = secrets[0]?.value
  if (!secret) throw new Error(`application ${appId} has no client secret`)
  return secret
}

async function ensureResource(call: ReturnType<typeof api>) {
  const existing = await call<Array<{ id: string; indicator: string }>>('/resources')
  const found = existing.find((r) => r.indicator === API_INDICATOR)
  if (found) return { ...found, created: false }
  const made = await call<{ id: string; indicator: string }>('/resources', {
    method: 'POST',
    body: {
      name: 'lemul control plane',
      indicator: API_INDICATOR,
      // Long enough that an operator is not re-authenticating mid-session,
      // short enough that revocation means something.
      accessTokenTtl: 3600,
    },
  })
  return { ...made, created: true }
}

async function ensureApp(
  call: ReturnType<typeof api>,
  spec: {
    name: string
    type: 'Traditional' | 'Native'
    redirectUris: string[]
    postLogoutRedirectUris: string[]
    /** `isDeviceFlow` lives here; without it Logto refuses the device grant. */
    customClientMetadata?: Record<string, unknown>
  },
) {
  const existing = await call<Application[]>('/applications')
  let found = existing.find((a) => a.name === spec.name)

  // Device flow is fixed at creation: Logto answers a PATCH with
  // `application.device_flow_not_changeable`. So an app seeded before the flag
  // was needed can only be replaced, never corrected -- and leaving it alone
  // would fail later at /device/auth with a message about grant types that
  // points nowhere near here. Safe to recreate: the CLI is a public client with
  // no secret anyone holds.
  const wantDeviceFlow = spec.customClientMetadata?.isDeviceFlow === true
  if (found && Boolean(found.customClientMetadata?.isDeviceFlow) !== wantDeviceFlow) {
    console.error(`  ${spec.name}: device-flow flag differs and cannot be patched; recreating`)
    await call(`/applications/${found.id}`, { method: 'DELETE' })
    found = undefined
  }

  if (found) {
    // Converge the parts that CAN be changed. "Exists" is not "correct": an app
    // left from an earlier run with the wrong redirect URIs fails much later
    // with an error that says nothing about seeding.
    await call(`/applications/${found.id}`, {
      method: 'PATCH',
      body: {
        oidcClientMetadata: {
          redirectUris: spec.redirectUris,
          postLogoutRedirectUris: spec.postLogoutRedirectUris,
        },
      },
    })
    return { ...found, created: false }
  }
  const made = await call<Application>('/applications', {
    method: 'POST',
    body: {
      name: spec.name,
      type: spec.type,
      // First-party by default, which is what skips the consent screen: Logto
      // only asks for consent on third-party apps. Nothing to turn off.
      isThirdParty: false,
      oidcClientMetadata: {
        redirectUris: spec.redirectUris,
        postLogoutRedirectUris: spec.postLogoutRedirectUris,
      },
      ...(spec.customClientMetadata
        ? { customClientMetadata: spec.customClientMetadata }
        : {}),
    },
  })
  return { ...made, created: true }
}

async function ensureSmtpConnector(call: ReturnType<typeof api>) {
  const existing = await call<Array<{ id: string; connectorId: string }>>('/connectors')
  const found = existing.find((c) => c.connectorId === 'simple-mail-transfer-protocol')
  if (found) return { id: found.id, created: false }

  const template = (usageType: string, subject: string, verb: string) => ({
    usageType,
    contentType: 'text/plain',
    subject,
    content: `Your lemul ${verb} code is {{code}}. It expires in 10 minutes.`,
  })

  const made = await call<{ id: string }>('/connectors', {
    method: 'POST',
    body: {
      connectorId: 'simple-mail-transfer-protocol',
      config: {
        // Container-to-container: Logto reaches Mailpit by service name, not
        // localhost. Using localhost here is the classic failure -- it resolves
        // inside the Logto container to Logto itself and the send just hangs.
        host: 'mailpit',
        port: 1025,
        // Mailpit accepts anything (MP_SMTP_AUTH_ACCEPT_ANY), and there is no
        // TLS on the local listener.
        auth: { type: 'login', user: 'lemul', pass: 'lemul' },
        secure: false,
        ignoreTls: true,
        fromEmail: 'lemul@localhost',
        templates: [
          template('SignIn', 'lemul sign-in code', 'sign-in'),
          template('Register', 'lemul sign-up code', 'sign-up'),
          template('ForgotPassword', 'lemul password reset code', 'password reset'),
          template('Generic', 'lemul verification code', 'verification'),
        ],
      },
    },
  })
  return { id: made.id, created: true }
}

/**
 * Passwordless email: no password field anywhere, sign-in and sign-up both by
 * emailed verification code. `verify: true` on sign-up is what makes the code
 * mandatory rather than optional.
 */
async function setSignInExperience(call: ReturnType<typeof api>) {
  await call('/sign-in-exp', {
    method: 'PATCH',
    body: {
      signUp: { identifiers: ['email'], password: false, verify: true },
      signIn: {
        methods: [
          {
            identifier: 'email',
            password: false,
            verificationCode: true,
            isPasswordPrimary: false,
          },
        ],
      },
    },
  })
}

const secret = await bootstrapSecret()
const token = await managementToken(secret)
const call = api(token)

const resource = await ensureResource(call)
const consoleApp = await ensureApp(call, {
  name: 'lemul console',
  type: 'Traditional',
  redirectUris: [CONSOLE_REDIRECT],
  postLogoutRedirectUris: [CONSOLE_POST_LOGOUT],
})
// Native, because that is the application type Logto allows the device
// authorization grant on.
//
// The redirect URI is never used -- the whole point of device flow is that the
// CLI has nowhere to redirect to -- but Logto still validates the application
// shape and refuses `/device/auth` with `redirect_uris must contain members`.
// A custom scheme rather than a localhost port, so nothing is implied about the
// CLI listening on one.
const cliApp = await ensureApp(call, {
  name: 'ourcli',
  type: 'Native',
  redirectUris: ['io.lemul.ourcli://callback'],
  postLogoutRedirectUris: [],
  // The grant is refused outright without this, with a message about the grant
  // type that says nothing about where the switch is.
  customClientMetadata: { isDeviceFlow: true },
})
const connector = await ensureSmtpConnector(call)
await setSignInExperience(call)

const consoleSecret = await appSecret(call, consoleApp.id)

const mark = (created: boolean) => (created ? 'created' : 'exists')
console.error(`resource  ${mark(resource.created)}  ${API_INDICATOR}`)
console.error(`console   ${mark(consoleApp.created)}  ${consoleApp.id}`)
console.error(`ourcli    ${mark(cliApp.created)}  ${cliApp.id}`)
console.error(`smtp      ${mark(connector.created)}  -> mailpit:1025`)
console.error(`sign-in   set to passwordless email`)
console.error('')

// Printed to stdout so it can be redirected straight into an env file, with the
// human-readable summary above going to stderr.
console.log(`# generated by auth-stack/seed.ts
LOGTO_ENDPOINT=${ENDPOINT}
LOGTO_APP_ID=${consoleApp.id}
LOGTO_APP_SECRET=${consoleSecret}
LOGTO_API_RESOURCE=${API_INDICATOR}
LOGTO_COOKIE_SECRET=${crypto.randomUUID()}
CONSOLE_ORIGIN=http://localhost:3000
LEMUL_API=http://127.0.0.1:9000

# ourcli (device flow) and the Go control plane
LEMUL_CLI_CLIENT_ID=${cliApp.id}
LEMUL_AUTH_ISSUER=${ENDPOINT}/oidc
LEMUL_AUTH_AUDIENCE=${API_INDICATOR}`)
