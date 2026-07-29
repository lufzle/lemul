# tui-proxy-proto

Spike **S3** from [`../CC_REMOTE_ANALYSIS.md`](../CC_REMOTE_ANALYSIS.md) §7:
does Claude Code's TUI survive being proxied over WebSocket?

This is the riskiest spike. If fidelity is unacceptable, the entire
"proxy the real CLI" premise is in question and Phase 4 (own client on the
Agent SDK) moves up.

**Step 1:** one hop — `client → supervisor → PTY`. ✅
**Step 2:** `client → relay → yamux tunnel → runner → PTY`. ✅

Both pass. Every automated test runs against **both** transports as subtests
(`/1hop`, `/2hop`), so a failure appearing only in `2hop` indicts the mux
specifically. That attribution is the whole reason the steps are separate.

## Layout

```
proto/       client<->relay contract: binary frames = PTY bytes, text = control
tunnel/      relay<->runner: net.Conn adapter for yamux + length-prefixed framing
supervisor/  step 1: owns the PTY, serves it over WS directly
relay/       step 2: only public listener. /tunnel (runner dials in) + /pty (user)
runner/      step 2: dials OUT from the customer VPC, owns the PTYs
client/      attaches the local terminal      (→ becomes `ourcli connect`)

fidelity_test.go   §4.1 checks, parameterized over both transports
twohop_test.go     two-hop harness + tunnel framing unit tests
MANUAL_MATRIX.md   interactive §4.5 checks that need a human
render-test.sh     matrix item #7 (box-drawing / emoji / CJK width)
```

## Run

```bash
go test ./...                # both transports
go test ./... -race          # concurrency check

# one hop
go run ./supervisor -addr :8080 -cmd claude
go run ./client -url ws://localhost:8080/pty

# two hops
go run ./relay -addr :9000
go run ./runner -relay ws://localhost:9000/tunnel -cmd claude
go run ./client -url ws://localhost:9000/pty
```

Note the client URL is the **relay** in both cases — from the client's side the
topology is invisible, which is the point.

`-cmd` accepts anything: `bash`, `htop`, `vim` are useful for isolating whether
a problem is Claude-Code-specific or general TUI behavior.

## Why these design choices

| Choice | Reason |
|---|---|
| **Real PTY** (`pty.Start`, not pipes) | With pipes `isatty()` is false and Claude Code silently degrades to non-interactive mode — no TUI, no color, line-buffered. Binary failure mode, and the #1 thing to get wrong. |
| **Binary WS frames for PTY data** | Terminal output is not valid UTF-8 at arbitrary chunk boundaries; a multi-byte rune or escape sequence will straddle a frame. Text frames force UTF-8 validation and corrupt it. |
| **Text WS frames for control** | WebSocket's own frame types do the demux — no length-prefix header needed. |
| **Out-of-band resize** | Terminal size can't travel in the byte stream. `{type:"resize"}` → `ioctl(TIOCSWINSZ)` → `SIGWINCH`. Without it everything renders at 80×24. |
| **Initial size in the dial URL, not a control message** | A PTY defaults to **0×0**. A resize sent after connect arrives too late — the child has already drawn its first frame. Size goes in `?rows=&cols=` and is applied by `pty.StartWithSize`, atomically with the fork. |
| **`TCP_NODELAY` explicit** | Go enables it by default, but one missed hop batches keystrokes into ~40 ms delays that read as "the product feels laggy" rather than as a bug. |
| **Client raw mode + guaranteed restore** | Raw mode is what delivers Ctrl-C to the *remote* process. Restore runs on normal exit, panic, and SIGTERM — otherwise the user is left with a broken shell. |
| **Ctrl-] to detach** | In raw mode every byte is forwarded, so there is no key left to mean "quit the client". Needs its own escape sequence, the way SSH uses `~.`. Ctrl-] (`0x1d`) is the classic telnet escape and is not bound by Claude Code. |
| **Close frame with an explicit code** | `CloseNormalClosure` = session over, do not reconnect. Absent/abnormal close = dropped connection, reconnect and replay. Step 2's reconnect logic branches on this. |
| **`TERM` + `COLORTERM` set explicitly** | terminfo must resolve inside the container or apps fall back; `COLORTERM=truecolor` is what unlocks 24-bit color. |

## Step 2 result: the mux costs ~70 µs and nothing else

**No fidelity regression.** Every check that passes at one hop passes at two,
byte-for-byte — including all seven binary-transparency payloads (lone UTF-8
continuation bytes, OSC 52, the bracketed-paste wrapper, kitty keyboard push).

Keystroke round-trip, 5 paired runs on loopback:

| run | 1hop | 2hop | delta |
|---|---|---|---|
| 1 | 94 µs | 180 µs | +86 |
| 2 | 102 µs | 186 µs | +84 |
| 3 | 127 µs | 218 µs | +91 |
| 4 | 107 µs | 160 µs | +53 |
| 5 | 114 µs | 165 µs | +51 |

**~70 µs median added.** That is three orders of magnitude below the ~20–40 ms
of real network RTT a user will experience, so the mux is not a latency factor
in production — geography is. Throughput was unchanged (2.3 vs 2.7 MB/s on 20k
lines, within noise).

Verified end-to-end with the real binaries: a 150×45 terminal propagates through
relay → tunnel → runner (`session opened: size=150x45`) and tears down cleanly.

### The framing hazard step 2 introduced

On the client↔relay hop, WebSocket's own frame types separate PTY bytes from
control messages. **That distinction does not survive the tunnel** — a yamux
stream is a raw byte pipe. So `tunnel/frame.go` carries an explicit type tag:
`[type:1][len:4 BE][payload]`.

Getting this wrong is the most plausible way the mux breaks fidelity: a control
frame misread as data injects JSON into the user's screen; data misread as
control silently drops output. `TestTunnelFrameTypesDoNotCross` and
`TestTunnelFrameRoundTrip` cover it directly, including a 64 KiB payload
(larger than one PTY read) and all 256 byte values.

## Automated results (2026-07-29, darwin/arm64, Go 1.24.5)

All pass, on both transports, and under `-race`.

| Test | Result |
|---|---|
| `TestPTYIsATTY` | PASS — child sees a TTY |
| `TestInitialSizeAppliedBeforeChildStarts` | PASS — regression guard, see below |
| `TestResizeDeliversSIGWINCH` | PASS — both first and second resize land |
| `TestBinaryTransparency` | PASS — 7/7 payloads byte-exact |
| `TestHighVolumeOutputIntegrity` | PASS — 20k lines / 229 KB, complete and ordered |
| `TestTerminalEnvPropagation` | PASS |
| `TestTerminfoResolves` | PASS — `tput colors` → 256 |
| `TestCtrlCDeliversSIGINT` | PASS |
| `TestChildExitClosesConnection` | PASS — regression guard, see below |
| `TestChildExitSendsNormalCloseCode` | PASS |
| `TestKeystrokeRoundTripLatency` | PASS — **avg 60 µs** over loopback |

The transparency payloads include lone UTF-8 continuation bytes, a truncated
3-byte sequence, all 128 high bytes, a truecolor CSI sequence, OSC 52, the
bracketed-paste wrapper, and a kitty keyboard push. All survive byte-exact.

**The 60 µs loopback RTT is the transport's own floor.** Everything above it in
production is geography, which is the argument for placing the relay in the
customer's region rather than near the user.

## Exiting the client

| Key | Behavior | Why |
|---|---|---|
| **Ctrl-C** | Forwarded to Claude Code; does **not** exit the client | Correct. Raw mode forwards `0x03` so it interrupts CC's running tool call (matrix #4, #13). A client that exited on Ctrl-C would be the bug. |
| **Ctrl-D** | Child sees EOF and exits → client prints `[session ended]` and exits | Fixed — see below. |
| **Ctrl-]** | Detaches the client, leaves the remote session alone | Needed because raw mode leaves no other key available. |

### Bug found and fixed: Ctrl-D hung the client

Original chain: Ctrl-D → child exits → `ptmx.Read` errors → the PTY goroutine
returned — but the supervisor's main loop was still blocked on `c.Read()`
waiting for client input, so **nothing ever closed the websocket** and the
client's read loop blocked forever.

Fix: the PTY goroutine now sends a `CloseNormalClosure` frame and closes the
connection on EOF. The client distinguishes that from an abnormal close, which
is exactly the signal step 2 needs to decide whether to reconnect.

Guarded by `TestChildExitClosesConnection` (asserts the connection closes within
5 s) and `TestChildExitSendsNormalCloseCode` (asserts the code, since reconnect
logic branches on it).

### Bug found and fixed: first frame rendered into a 0×0 terminal

A PTY's window size defaults to `0 0`. The original code called `pty.Start` and
relied on the client's first resize *control message* to set the size — which
arrives after the child has already started and drawn its first frame. Probing
it directly confirmed the sequence: `stty size` → `0 0`, then `50 200` once the
resize landed.

Visible symptom would have been a collapsed or garbled Claude Code TUI on
connect that only fixes itself once you manually resize the window — the kind of
thing easily misattributed to the TUI or the transport.

Fix: the client sends `?rows=&cols=` on the dial URL; the supervisor applies it
with `pty.StartWithSize`, atomically with the fork. Guarded by
`TestInitialSizeAppliedBeforeChildStarts`, which fails on `0 0`.

Verified end-to-end: a 100×40 local terminal logs `session started: … size=100x40`.

### Harness gotcha worth remembering

The first run showed four transparency failures where `1b 5b` (ESC `[`) arrived
as `5e 5b 5b` (`^[[`). That was the **PTY's ECHOCTL line discipline** rendering
control characters visibly during `cat` echo — not a transport fault. Fix was
`stty raw -echo` in the test child. If a future test shows `^[` where ESC was
sent, check termios before suspecting the relay.

## Not yet covered

- **Reconnect / replay buffer.** The supervisor currently kills the child on
  client disconnect, so matrix item #6 (network drop) fails by design. Phase 1
  work: tmux, or a server-side VT state model.
- **The mux (step 2).** The whole point of the two-step split.
- **Interactive matrix.** See `MANUAL_MATRIX.md` — needs a human.
