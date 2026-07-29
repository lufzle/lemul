# S3 Manual Test Matrix

The automated suite (`go test ./...`) covers the mechanical requirements. These
items need a human, a real terminal emulator, and real Claude Code — they are
about *perceived* fidelity, which no assertion captures.

## Setup

```bash
# terminal 1
go run ./supervisor -addr :8080 -cmd claude

# terminal 2 — must be a real terminal (iTerm2, Ghostty, Terminal.app, Alacritty)
go run ./client -url ws://localhost:8080/pty
```

Record which emulator you used; results differ between them. Repeat on at least
two (ideally one of iTerm2/Ghostty plus Terminal.app) — the enhanced keyboard
protocol is where they diverge most.

Notation: **P** pass · **F** fail · **?** inconclusive.

---

## Matrix (from CC_REMOTE_ANALYSIS.md §4.5)

| # | Test | How | Pass criterion | Step 1 (1 hop) | Step 2 (2 hops) |
|---|---|---|---|---|---|
| 1 | **Bracketed paste** | Paste a 30-line code block into the prompt | Arrives as ONE message; does not submit on first newline | | |
| 2 | **Shift+Enter** | Press Shift+Enter mid-prompt | Inserts a newline; does NOT submit | | |
| 3 | **Mid-render resize** | Start a long response, drag the window narrower while it streams | Clean reflow; no garbage, no stale columns | | |
| 4 | **Ctrl-C during tool call** | Ask CC to run a long bash command, Ctrl-C mid-execution | Interrupts the tool; session survives; prompt returns | | |
| 5 | **Large output** | `cat` a 100k-line file, or ask CC to print a big file | Client keeps up; no lockup; scrollback intact | | |
| 6 | **Network drop** | `sudo pfctl` block / disable Wi-Fi 60s, restore | Session survives; screen repaints correctly | | |
| 7 | **Box + emoji render** | Ask CC something that draws a box; include emoji and CJK | No shearing at 80 cols; no shearing at 200 cols | | |

### Additional Claude Code specific checks

| # | Test | Pass criterion | Step 1 | Step 2 |
|---|---|---|---|---|
| 8 | **True color** | `printf '\e[38;2;255;100;0mORANGE\e[0m\n'` shows orange, not approximated | | |
| 9 | **OSC 52 clipboard** | Copy from remote session lands in local clipboard (requires emulator support enabled) | | |
| 10 | **Plan mode** | Shift+Tab cycles permission modes; the mode indicator renders | | |
| 11 | **`/` command menu** | Slash-command autocomplete list renders and navigates with arrows | | |
| 12 | **Image paste** | Expected to FAIL — local-terminal affordance, does not survive the hop. Confirm the failure mode is graceful (no crash) | | |
| 13 | **Ctrl-C at idle prompt** | Does not kill the client process; behaves as CC does locally | | |
| 14 | **Terminal restore** | After exit (and after `kill -TERM` on the client), local shell is not left in raw mode | | |
| 15 | **Ctrl-D / `/exit`** | Claude Code exits → client prints `[session ended]` and returns to your shell | | |
| 16 | **Ctrl-] detach** | Client prints `[detached]` and exits; terminal is sane | | |

---

## Notes on expected results

**#12 image paste is expected to fail.** It is a local-terminal-to-local-process
affordance. Product answers: an upload endpoint in the web client, a CLI
subcommand, or a paste hook that ships the file and rewrites it as a path.

**#2 and #10 are the ones most likely to break.** Both depend on the enhanced
keyboard protocol (kitty keyboard / `modifyOtherKeys`) negotiating end-to-end.
The negotiation escapes must pass through untouched in **both** directions — the
automated suite proves the bytes survive, but not that the negotiation
round-trips under a real emulator's handshake.

**#6 network drop will fail in step 1.** The current supervisor kills the child
on client disconnect. This is deliberate — reconnect handling (tmux or a
server-side VT state model) is a Phase 1 item, not a transport property. Record
the failure and re-test once the replay buffer lands.

---

## Verdict

S3 gates the CLI-proxy architecture. Interpretation:

- **1–5, 7, 8, 11, 13, 14 all P** → transport is sound, proceed to Phase 1.
- **2 or 10 F** → enhanced keyboard protocol needs work; likely fixable in the
  relay (do not filter/rewrite escape sequences).
- **1, 3, 5, or 7 F** → serious. Investigate before committing to CLI proxying;
  the §4.1 requirements are probably not all met.
- **Step 2 regresses anything that passed in step 1** → the mux is at fault.
  That is the specific finding this two-step split exists to produce.

---

## Step 2 setup (two hops)

The automated suite shows no regression through the mux, but the interactive
items still need re-running against the two-hop path.

```bash
go run ./relay  -addr :9000                                # terminal 1
go run ./runner -relay ws://localhost:9000/tunnel -cmd claude   # terminal 2
go run ./client -url ws://localhost:9000/pty               # terminal 3 (real terminal)
```

Fill the "Step 2" column. Anything that passed in step 1 and fails here is the
mux — most plausibly the tunnel framing in `tunnel/frame.go`, since that is the
only new surface between the two runs.
