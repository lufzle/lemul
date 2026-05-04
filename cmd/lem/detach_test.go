package main

import "testing"

func TestDetachIndexRecognisesEveryEncoding(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want int
	}{
		// The encoding a terminal uses depends on what the application
		// negotiated. Claude Code negotiates both of the modern ones, which is
		// why matching only the legacy byte silently stopped working.
		{"legacy control code", []byte{0x1d}, 0},
		{"kitty csi-u", []byte("\x1b[93;5u"), 0},
		{"kitty csi-u with event subparam", []byte("\x1b[93;5:1u"), 0},
		{"modifyOtherKeys", []byte("\x1b[27;5;93~"), 0},
		{"ctrl+shift+]", []byte("\x1b[93;6u"), 0}, // 6 = 1 + shift(1) + ctrl(4)

		{"after typed text", append([]byte("hello"), 0x1d), 5},
		{"csi-u after typed text", []byte("hi\x1b[93;5u"), 2},

		// Must not fire on anything else, or the client detaches mid-typing.
		{"plain text", []byte("hello world"), -1},
		{"bare escape", []byte{0x1b}, -1},
		{"escape then bracket only", []byte("\x1b["), -1},
		{"arrow key", []byte("\x1b[A"), -1},
		{"unmodified ] as csi-u", []byte("\x1b[93u"), -1},
		{"] with shift only", []byte("\x1b[93;2u"), -1},
		{"different key with ctrl", []byte("\x1b[97;5u"), -1},
		{"modifyOtherKeys, different key", []byte("\x1b[27;5;97~"), -1},
		{"modifyOtherKeys without ctrl", []byte("\x1b[27;2;93~"), -1},
		{"bracketed paste start", []byte("\x1b[200~"), -1},
		{"truecolor sgr", []byte("\x1b[38;2;255;100;0m"), -1},
		{"literal ] character", []byte("]"), -1},
		{"empty", []byte{}, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := detachIndex(tc.in); got != tc.want {
				t.Errorf("detachIndex(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// A pasted block can contain "]" and escape sequences in quantity. Detaching in
// the middle of a paste would be both surprising and data-losing.
func TestDetachIndexIgnoresPastedContent(t *testing.T) {
	paste := []byte("\x1b[200~arr[93] := x; if (a[5]) { }\x1b[201~")
	if got := detachIndex(paste); got != -1 {
		t.Errorf("detached inside a paste at offset %d: %q", got, paste)
	}
}

// A CSI sequence must be skipped whole, so its parameters cannot be rescanned
// as if they were fresh input.
func TestDetachIndexSkipsWholeSequences(t *testing.T) {
	in := []byte("\x1b[27;5;93~")
	if got := detachIndex(in[1:]); got != -1 {
		// Sanity: without the leading ESC this is just text, and 0x1d is absent.
		t.Errorf("matched a headless sequence at %d", got)
	}
	in2 := []byte("\x1b[38;2;29;93;5m") // contains "93;5" but is an SGR colour
	if got := detachIndex(in2); got != -1 {
		t.Errorf("matched inside an SGR sequence at %d: %q", got, in2)
	}
}

func TestHasCtrl(t *testing.T) {
	for mod, want := range map[int]bool{
		0: false, // absent
		1: false, // no modifiers
		2: false, // shift
		3: false, // alt
		5: true,  // ctrl
		6: true,  // shift+ctrl
		7: true,  // alt+ctrl
		8: true,  // shift+alt+ctrl
	} {
		if got := hasCtrl(mod); got != want {
			t.Errorf("hasCtrl(%d) = %v, want %v", mod, got, want)
		}
	}
}
