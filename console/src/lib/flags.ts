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
   * Off unless explicitly enabled, and the reason CHANGED on 2026-08-03 without
   * the comment noticing -- which is the same failure as the "localhost only"
   * banner in __root.tsx, so it is worth stating precisely.
   *
   * The reason it used to give is gone: §2.5's authorisation landed, and
   * mgmtapi's authz_test now proves a non-member of the organization AND a
   * member of the organization who is not a member of the workspace both get
   * 404 on /fs, /processes and /resources.
   *
   * What it actually guards now is INSIDE a workspace. The explorer is served
   * by the supervisor, which runs as ROOT, and fsjail.go confines paths to the
   * workspace root -- it stops an escape outward, not a read sideways. So a
   * member of a shared workspace can list and read every other member's home,
   * which is exactly the 0700 boundary §2.3 says is the isolation. Fine for a
   * workspace with one member; not fine as a default.
   *
   * It should stay off until the explorer reads as the REQUESTING member rather
   * than as root. §13 carries the row.
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
