// Command keyprobe shows the bytes your terminal emits for a keypress, with the
// same terminal modes Claude Code sets.
//
// It exists because of a real bug: Ctrl-] stopped detaching once sessions ran
// real Claude Code. CC pushes the kitty keyboard protocol and modifyOtherKeys at
// startup, and those escapes reach the user's own emulator through the
// byte-transparent relay -- so the emulator stops sending legacy control codes
// and sends CSI sequences instead. The detach key was never pressed as 0x1d.
//
// Which encoding you get depends on the emulator, so guessing is not good
// enough. Run this in the terminal you actually use.
//
// Usage:
//
//	keyprobe            # legacy mode, no negotiation
//	keyprobe -cc        # with the modes Claude Code sets
package main

import (
	"flag"
	"fmt"
	"os"

	"golang.org/x/term"
)

func main() {
	cc := flag.Bool("cc", false, "enable the terminal modes Claude Code sets (kitty keyboard + modifyOtherKeys)")
	flag.Parse()

	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		fmt.Fprintln(os.Stderr, "keyprobe: stdin is not a terminal")
		os.Exit(1)
	}

	old, err := term.MakeRaw(fd)
	if err != nil {
		fmt.Fprintln(os.Stderr, "keyprobe:", err)
		os.Exit(1)
	}
	restore := func() {
		if *cc {
			// Pop the keyboard mode before restoring, or the shell inherits it.
			fmt.Print("\x1b[<u\x1b[>4;0m")
		}
		_ = term.Restore(fd, old)
	}
	defer restore()

	if *cc {
		fmt.Print("\x1b[>1u\x1b[>4;2m")
		fmt.Print("modes: kitty keyboard (CSI >1u) + modifyOtherKeys 2 (CSI >4;2m)\r\n")
	} else {
		fmt.Print("modes: none (legacy encoding)\r\n")
	}
	fmt.Print("press keys to see their bytes; Ctrl-C three times to quit\r\n\r\n")

	buf := make([]byte, 256)
	ctrlC := 0
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return
		}
		if n == 0 {
			continue
		}
		p := buf[:n]
		fmt.Printf("% -28x %q\r\n", p, string(p))

		ctrlC++
		for _, b := range p {
			if b != 0x03 {
				ctrlC = 0
				break
			}
		}
		if ctrlC >= 3 {
			return
		}
	}
}
