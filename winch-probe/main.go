// Command winchprobe answers the question that gates decision #4:
//
//	does Claude Code repaint its WHOLE frame on SIGWINCH, or only patch part of it?
//
// Method: fork the child in a PTY, let it settle, record the byte offset, then
// nudge the size (cols-1, then back) and capture only what the nudge produced.
// Feed the settled stream into one VT emulator (ground truth = what an attached
// client sees) and the nudge output alone into a FRESH one (what a reattaching
// client with an empty replay ring sees). If they match, the cheap trick works.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/hinshun/vt10x"
)

var (
	cmdName = flag.String("cmd", "claude", "command to run")
	rows    = flag.Int("rows", 40, "terminal rows")
	cols    = flag.Int("cols", 100, "terminal cols")
	prime   = flag.String("prime", "", "bytes to type into the child before the nudge (\\r for Enter)")
	prime2  = flag.String("prime2", "", "second burst, sent after the first has settled")
	settle  = flag.Duration("settle", 700*time.Millisecond, "quiescence window")
	maxWait = flag.Duration("maxwait", 25*time.Second, "max wait for quiescence")
	dump    = flag.Bool("dump", false, "dump the nudge bytes, escaped")
	grace   = flag.Duration("grace", 1500*time.Millisecond, "minimum wait after an action before checking quiescence")
)

type capture struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	last time.Time
}

func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last = time.Now()
	return c.buf.Write(p)
}

func (c *capture) mark() (int, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Len(), c.last
}

func (c *capture) slice(a, b int) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.buf.Bytes()[a:b]...)
}

// quiesce waits at least grace (so a stale last-write timestamp cannot make it
// return before the child has had a chance to react), then blocks until no
// bytes have arrived for *settle, or maxWait elapses.
func (c *capture) quiesce(grace time.Duration) {
	time.Sleep(grace)
	deadline := time.Now().Add(*maxWait)
	for time.Now().Before(deadline) {
		_, last := c.mark()
		if !last.IsZero() && time.Since(last) > *settle {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func main() {
	flag.Parse()

	c := exec.Command(*cmdName, flag.Args()...)
	c.Dir, _ = os.Getwd()
	// Strip the parent Claude Code's markers so the child behaves like a normal
	// top-level session rather than a nested one.
	for _, e := range os.Environ() {
		k, _, _ := strings.Cut(e, "=")
		if strings.HasPrefix(k, "CLAUDE") || strings.HasPrefix(k, "ANTHROPIC") {
			continue
		}
		c.Env = append(c.Env, e)
	}
	c.Env = append(c.Env, "TERM=xterm-256color", "COLORTERM=truecolor")

	ptmx, err := pty.StartWithSize(c, &pty.Winsize{Rows: uint16(*rows), Cols: uint16(*cols)})
	if err != nil {
		fmt.Fprintf(os.Stderr, "start: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = ptmx.Close() }()

	cap := &capture{}
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				_, _ = cap.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	cap.quiesce(*grace)

	// Primes are sent as separate bursts with a quiescence wait between them,
	// so modal input (e.g. Claude Code's "!" shell mode) has time to switch
	// before the rest of the line arrives.
	for _, p := range []string{*prime, *prime2} {
		if p == "" {
			continue
		}
		s := strings.ReplaceAll(p, `\r`, "\r")
		s = strings.ReplaceAll(s, `\n`, "\n")
		_, _ = ptmx.Write([]byte(s))
		cap.quiesce(*grace)
	}

	// Everything up to here is what an attached client has already seen.
	offSettled, _ := cap.mark()

	// The nudge: cols-1, then back. Each step gets its own quiescence window so
	// a slow re-render is not truncated.
	_ = pty.Setsize(ptmx, &pty.Winsize{Rows: uint16(*rows), Cols: uint16(*cols - 1)})
	cap.quiesce(*grace)
	offMid, _ := cap.mark()
	_ = pty.Setsize(ptmx, &pty.Winsize{Rows: uint16(*rows), Cols: uint16(*cols)})
	cap.quiesce(*grace)
	fmt.Printf("bytes after shrink step: %d\n", offMid-offSettled)

	offNudged, _ := cap.mark()

	settled := cap.slice(0, offSettled)
	nudge := cap.slice(offSettled, offNudged)

	_ = c.Process.Kill()
	_, _ = c.Process.Wait()

	// A: ground truth -- the full stream, i.e. what the attached client shows.
	// B: a reattaching client with an EMPTY replay ring, nudge output only.
	// C: a reattaching client with a full replay ring (ring + nudge).
	// The attached client receives the nudge bytes too, so ground truth is the
	// whole stream -- comparing against the pre-nudge screen would flag any
	// legitimate state change during the nudge as a mismatch.
	screenA := render(append(append([]byte(nil), settled...), nudge...))
	screenB := render(nudge)
	screenC := render(append(append([]byte(nil), ring(settled, 256*1024)...), nudge...))

	fmt.Printf("settled stream: %d bytes\nnudge output:   %d bytes\n\n", len(settled), len(nudge))
	fmt.Printf("nudge contains ED/erase-display (ESC[J / ESC[2J / ESC[3J): %v\n", hasErase(nudge))
	fmt.Printf("nudge contains alt-screen toggle (?1049):                  %v\n", bytes.Contains(nudge, []byte("?1049")))
	fmt.Printf("nudge contains cursor-home (ESC[H / ESC[1;1H):             %v\n\n",
		bytes.Contains(nudge, []byte("\x1b[H")) || bytes.Contains(nudge, []byte("\x1b[1;1H")))

	reportDiff("B (nudge only, empty ring)", screenA, screenB)
	reportDiff("C (256KB ring + nudge)", screenA, screenC)

	fmt.Println("\n=== A: ground truth (full stream) ===")
	fmt.Println(box(screenA))
	fmt.Println("\n=== B: reattach with EMPTY ring (nudge output only) ===")
	fmt.Println(box(screenB))

	if *dump {
		fmt.Printf("\n=== settled bytes (escaped) ===\n%q\n", string(settled))
		fmt.Printf("\n=== nudge bytes (escaped) ===\n%q\n", string(nudge))
	}
}

func render(b []byte) []string {
	t := vt10x.New(vt10x.WithSize(*cols, *rows))
	_, _ = t.Write(b)
	lines := strings.Split(t.String(), "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	return lines
}

func ring(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[len(b)-n:]
}

func hasErase(b []byte) bool {
	for _, s := range []string{"\x1b[J", "\x1b[0J", "\x1b[1J", "\x1b[2J", "\x1b[3J"} {
		if bytes.Contains(b, []byte(s)) {
			return true
		}
	}
	return false
}

func reportDiff(label string, a, b []string) {
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	var diff, aNonEmpty int
	for i := 0; i < n; i++ {
		x, y := at(a, i), at(b, i)
		if x != "" {
			aNonEmpty++
		}
		if x != y {
			diff++
		}
	}
	verdict := "MATCH"
	if diff > 0 {
		verdict = "DIFFERS"
	}
	fmt.Printf("%-28s %s  (%d/%d lines differ; %d non-blank lines in A)\n", label, verdict, diff, n, aNonEmpty)
	if diff > 0 {
		shown := 0
		for i := 0; i < n && shown < 12; i++ {
			if at(a, i) != at(b, i) {
				fmt.Printf("    line %2d  A: %q\n             B: %q\n", i, at(a, i), at(b, i))
				shown++
			}
		}
	}
}

func at(s []string, i int) string {
	if i < len(s) {
		return s[i]
	}
	return ""
}

func box(lines []string) string {
	var sb strings.Builder
	for i, l := range lines {
		fmt.Fprintf(&sb, "%2d |%s\n", i, l)
	}
	return sb.String()
}
