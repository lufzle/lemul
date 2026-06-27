import LogtoClient, { CookieStorage } from '@logto/node'
import { getCookie, setCookie } from '@tanstack/react-start/server'

/**
 * Server-only Logto wiring. Never import this at module scope from a route or
 * from anything a route imports: it pulls in `@tanstack/react-start/server`,
 * which the Vite plugin denies in the client environment and which fails the
 * build rather than degrading. Import it with `await import()` inside a handler.
 *
 * Everything here reads its configuration per call, not at module scope. A
 * non-`VITE_` value read at module load can be pulled into the client bundle,
 * and `LOGTO_APP_SECRET` is precisely the value that must not be.
 */

const SESSION_COOKIE = 'lemul_console_session'

function config() {
  const endpoint = process.env.LOGTO_ENDPOINT
  const appId = process.env.LOGTO_APP_ID
  const appSecret = process.env.LOGTO_APP_SECRET
  const resource = process.env.LOGTO_API_RESOURCE
  const cookieSecret = process.env.LOGTO_COOKIE_SECRET
  if (!endpoint || !appId || !appSecret || !resource || !cookieSecret) {
    throw new Error(
      'console auth is not configured. Generate it with:\n' +
        '  bun auth-stack/seed.ts > auth-stack/.env.generated\n' +
        'then start the console with those variables in the environment.',
    )
  }
  return { endpoint, appId, appSecret, resource, cookieSecret }
}

export function isConfigured(): boolean {
  return Boolean(process.env.LOGTO_ENDPOINT && process.env.LOGTO_APP_ID)
}

/**
 * Builds a client bound to this request's cookies.
 *
 * `navigate` is where the redirect lands: the SDK is written for a browser, so
 * `signIn()` calls the adapter with a URL instead of returning one. We capture
 * it and turn it into an HTTP redirect, which is what a server-rendered app
 * needs.
 */
async function client() {
  const cfg = config()
  let navigateTo: string | undefined

  const storage = new CookieStorage({
    cookieKey: SESSION_COOKIE,
    // Secure cookies require HTTPS. The console is loopback-only http in
    // development, so this follows the origin rather than being hardcoded --
    // hardcoding `true` silently drops the cookie and presents as an endless
    // redirect loop back to sign-in.
    isSecure: (process.env.CONSOLE_ORIGIN ?? '').startsWith('https://'),
    encryptionKey: cfg.cookieSecret,
    getCookie: (name) => getCookie(name),
    setCookie: (name, value, options) => {
      setCookie(name, value, { ...options, httpOnly: true, sameSite: 'lax' })
    },
  })
  await storage.init()

  const logto = new LogtoClient(
    {
      endpoint: cfg.endpoint,
      appId: cfg.appId,
      appSecret: cfg.appSecret,
      // Asking for the API resource up front is what makes the eventual
      // getAccessToken(resource) return a token the Go control plane will
      // accept: its audience is this indicator.
      resources: [cfg.resource],
      // Without `email` the ID token carries only the opaque subject, and the
      // header ends up identifying the operator by a random string. It is the
      // one claim this console actually shows.
      scopes: ['email'],
    },
    { navigate: (url: string) => void (navigateTo = url), storage },
  )

  return { logto, resource: cfg.resource, target: () => navigateTo }
}

/** Redirect URL that starts the sign-in flow. */
export async function signInRedirect(redirectUri: string): Promise<string> {
  const { logto, target } = await client()
  await logto.signIn(redirectUri)
  const url = target()
  if (!url) throw new Error('logto did not produce a sign-in URL')
  return url
}

export async function completeSignIn(callbackUrl: string): Promise<void> {
  const { logto } = await client()
  await logto.handleSignInCallback(callbackUrl)
}

export async function signOutRedirect(postLogoutUri: string): Promise<string> {
  const { logto, target } = await client()
  await logto.signOut(postLogoutUri)
  return target() ?? postLogoutUri
}

export type Session = {
  authenticated: boolean
  email?: string
  subject?: string
}

export async function readSession(): Promise<Session> {
  if (!isConfigured()) {
    // Auth off: the console behaves as it did before there was any. Reported
    // honestly so the UI can say so rather than implying a signed-in user.
    return { authenticated: true }
  }
  try {
    const { logto } = await client()
    if (!(await logto.isAuthenticated())) return { authenticated: false }
    const claims = await logto.getIdTokenClaims()
    return { authenticated: true, email: claims.email ?? undefined, subject: claims.sub }
  } catch {
    // A cookie that cannot be decrypted or a session that cannot be refreshed
    // is not an error to show -- it is simply not signed in.
    return { authenticated: false }
  }
}

/**
 * The access token for the control plane.
 *
 * This is the load-bearing piece of the console's authorisation story. Every
 * call to the Go API goes through it, so a server function cannot reach the
 * control plane without a valid session -- the check is structural rather than
 * a guard someone has to remember to attach to each function. That matters
 * because server functions are RPC endpoints reachable by direct POST no matter
 * which route renders the UI, so a route guard would protect nothing here.
 *
 * With auth switched off it returns undefined and callers send no header, which
 * is the configuration the e2e suite and a bare local run use.
 */
export async function accessToken(): Promise<string | undefined> {
  if (!isConfigured()) return undefined
  const { logto, resource } = await client()
  if (!(await logto.isAuthenticated())) {
    throw new Error('not signed in')
  }
  return logto.getAccessToken(resource)
}

/**
 * The raw ID token, for relaying to PUT /v1/identity.
 *
 * The client has held one all along -- readSession reads its claims -- but
 * nothing ever asked for the token itself, because the console used to send a
 * self-asserted label instead. The token is what makes the email an assertion
 * the control plane can verify rather than a string it has to take on trust.
 *
 * Undefined with auth off: there is no identity provider to have signed
 * anything, and the control plane refuses an unverifiable email rather than
 * accepting one.
 */
export async function idToken(): Promise<string | undefined> {
  if (!isConfigured()) return undefined
  const { logto } = await client()
  if (!(await logto.isAuthenticated())) return undefined
  return (await logto.getIdToken()) ?? undefined
}
