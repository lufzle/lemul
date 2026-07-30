# winch-probe — does Claude Code repaint fully on SIGWINCH?

**Verdict: yes. Decision #4 stands, with one addition.**
Measured 2026-07-29 against Claude Code **2.1.220**, darwin/arm64, Go 1.24.5.

This is the check [`../CC_REMOTE_ANALYSIS.md`](../CC_REMOTE_ANALYSIS.md) §12 decision #4
called a gate: the Phase 1 detach/reattach plan (keep the child alive, bounded
replay ring, SIGWINCH nudge) only works if Claude Code redraws its *whole* frame
on resize rather than patching part of it. Manual matrix item #3 passing was
encouraging but not conclusive.

## Method

Eyeballing a resize cannot answer this, because an attached terminal already
holds the correct screen — a partial patch looks identical to a full repaint.
So the probe reconstructs what a *reattaching* client would see:

1. Fork the child in a PTY and let the output go quiet.
2. Record the byte offset. Everything before it is what the attached client has
   already seen.
3. Nudge: `cols-1`, wait, `cols`. Capture **only** the bytes this produced.
4. Render three screens with a `vt10x` emulator:
   - **A** — the whole stream. Ground truth: what a continuously attached client shows.
   - **B** — the nudge output alone, into a *fresh* emulator. A reattaching
     client whose replay ring is empty.
   - **C** — a 256 KB ring plus the nudge output.
5. Diff B and C against A, line by line.

If B matches A, the nudge alone reconstructs the screen and the replay ring is
not load-bearing for correctness.

## Results

All five scenarios: **B matched A exactly.**

| Scenario | Terminal | Nudge bytes | B vs A |
|---|---|---|---|
| Welcome screen | 100×40 | 7,263 | 0/41 lines differ |
| `/help` panel, fits the viewport | 100×40 | 8,858 | 0/41 differ |
| Banner scrolled off the top | 100×12 | 3,736 | 0/13 differ |
| `/help` overflowing the viewport | 100×14 | 2,334 | 0/15 differ |
| Shell-mode `seq 1 120` + agent reply | 100×30 | 3,570 | 0/31 differ |

Sequence census of the repaint:

| Sequence | Count | Meaning |
|---|---|---|
| `ESC[2K` | 60 (2 per row, 30 rows) | erase entire line before rewriting it |
| `ESC[H` | 4 | cursor home |
| `ESC[J` / `ESC[2J` / `ESC[3J` | 0 | no erase-display — it does not need one |
| `ESC[?1049` | 0 | no alternate screen; Claude Code is an inline TUI |

**Claude Code repaints the entire visible viewport**, homing the cursor and
erasing each line before rewriting it. Because every line is erased first, the
repaint is correct even onto a *dirty* screen — which is the case that would
have sunk the trick.

**Consequence for Phase 1:** the replay ring is optional polish. It restores
scrollback above the viewport; it is not what makes the reattached screen
correct.

## The finding that changed a Phase 1 item

The repaint restores the grid but **not the modes**. Claude Code emits its
terminal-mode negotiation exactly once, at startup:

```
ESC[?2004h    bracketed paste
ESC[>1u       kitty keyboard protocol push
ESC[>4;2m     modifyOtherKeys level 2
```

The nudge output contains **none** of them. A client reattaching into a fresh
terminal therefore gets a perfect-looking screen with **broken Shift+Enter and
broken paste** — working display, broken input, precisely the failure §9 item 2
predicts for the Phase 2 VT model.

The fix does not need a screen model: `internal/ptysession/modes.go` scans the
output stream for the handful of DECSET/DECRST, `CSI > … u` and `CSI > 4 ; … m`
shapes, keeps the latest value of each, and replays that **mode prelude** ahead
of the ring on attach. A few dozen bytes and an incremental CSI scanner.

One mode is deliberately excluded: **2026** (synchronized output). It brackets a
single frame, so replaying a stale "begin" leaves the client waiting for an end
marker that already went past — a frozen display.

## Harness notes

- **Validated against a control first.** `vim` produces 8,174 bytes with
  erase-display and cursor-home. Without that, a zero-byte result from Claude
  Code would have been indistinguishable from broken plumbing.
- **A bug that produced exactly that false negative:** the first version's
  quiescence check compared against a last-write timestamp that was already
  stale by the time the nudge fired, so it returned immediately and reported
  0 bytes. `-grace` now forces a minimum wait after every action. If this probe
  ever reports 0 bytes again, suspect the harness before the result.
- Ground truth is `settled + nudge`, not `settled` alone — the attached client
  receives the nudge bytes too, so comparing against the pre-nudge screen flags
  any legitimate state change during the nudge as a mismatch.

## Run

```bash
go build ./...

./winch-probe -cmd claude -rows 40 -cols 100
./winch-probe -cmd claude -rows 14 -cols 100 -prime '/help\r'
./winch-probe -cmd claude -rows 30 -cols 100 -prime '!' -prime2 'seq 1 120\r' -grace 3s
./winch-probe -cmd vim                          # control: a known full-repaint TUI
./winch-probe -cmd claude -dump                 # escaped byte dumps
```

Run it from a directory Claude Code already trusts, or the trust dialog is all
you will measure.
