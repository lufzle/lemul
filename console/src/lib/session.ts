import { createServerFn } from '@tanstack/react-start'
import type { Session } from './logto.server'

/**
 * Who is signed in, for the route guard and the header.
 *
 * Deliberately NOT protected: it is the function that answers "are you signed
 * in", so requiring a session to call it would make the answer unreachable.
 * It returns no capability — only whether a cookie is valid and the email
 * attached to it.
 */
export const getSession = createServerFn({ method: 'GET' }).handler(
  async (): Promise<Session> => {
    const { readSession } = await import('./logto.server')
    return readSession()
  },
)

/** Whether the console has an identity provider configured at all. */
export const authConfigured = createServerFn({ method: 'GET' }).handler(
  async (): Promise<boolean> => {
    const { isConfigured } = await import('./logto.server')
    return isConfigured()
  },
)
