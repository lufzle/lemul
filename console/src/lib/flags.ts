import { createServerFn } from '@tanstack/react-start'

/**
 * Feature flags, read from the console process's environment.
 *
 * Server-side on purpose. A non-`VITE_` variable read at module scope can be
 * pulled into the client bundle, and a flag whose value ships to the browser is
 * a flag an operator can flip in devtools -- which for anything gating a
 * capability is worse than no flag at all.
 */
export type Flags = {
  /**
   * The read-only session viewer (`FF_VIEW_SESSION`).
   *
   * Off unless explicitly enabled. A terminal in the browser is Phase 4
   * territory (§11) and, unlike the rest of the console, it is the one surface
   * that opens a session's byte stream to something other than `ourcli` -- so it
   * ships dark and gets turned on deliberately.
   */
  viewSession: boolean

  /**
   * The workspace explorer (`FF_WORKSPACE_EXPLORER`): directory listings,
   * running processes and resource usage for a workspace task.
   *
   * Off unless explicitly enabled, for an authorisation reason rather than a
   * maturity one. §2.5's owner-scoped attach is still outstanding, so any
   * authenticated operator reaches every workspace -- and where attaching to
   * someone's session is visible and means co-driving it, the explorer reads
   * their file paths and command lines silently. Loopback binding plus this
   * flag is what bounds that until §2.5 lands.
   */
  explorer: boolean
}

function on(v: string | undefined): boolean {
  // Anything unrecognised is off. A flag that gates a capability should fail
  // closed on a typo rather than guess that the operator meant yes.
  return v === '1' || v?.toLowerCase() === 'true'
}

export const getFlags = createServerFn({ method: 'GET' }).handler(
  async (): Promise<Flags> => ({
    viewSession: on(process.env.FF_VIEW_SESSION),
    explorer: on(process.env.FF_WORKSPACE_EXPLORER),
  }),
)
