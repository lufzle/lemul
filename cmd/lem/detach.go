package main

import "strconv"

// Detaching needs a key, and in raw mode every byte is forwarded, so it needs
// one the remote application does not use. Ctrl-] is the classic telnet escape
// and Claude Code does not bind it.
//
// The complication is that Claude Code does not have to bind it to break it.
// At startup it pushes the kitty keyboard protocol (CSI >1u) and modifyOtherKeys
// level 2 (CSI >4;2m). Those escapes travel through the byte-transparent relay
// to the USER'S OWN emulator, which then stops sending legacy control codes for
// modified keys and sends CSI sequences instead. So the raw 0x1d simply never
// arrives, and a scan for it silently never matches -- the client looks like it
// ignores the key.
//
// Which encoding appears depends on the emulator, so all three are recognised:
//
//	0x1d                    legacy control code
//	CSI 93 ; <mods> u       kitty keyboard, disambiguated (']' is U+005D = 93)
//	CSI 27 ; <mods> ; 93 ~  xterm modifyOtherKeys level 2
//
// cmd/keyprobe prints which one a given terminal produces.
const detachByte = 0x1d

// bracketKey is ']' as a Unicode code point, the key id both CSI encodings use.
const bracketKey = 93

// detachIndex returns the offset of a detach keypress in p, or -1.
//
// Matching is confined to a single read: a keypress is delivered as one write by
// every terminal in practice, and buffering a partial escape sequence across
// reads would delay a bare ESC -- which Claude Code binds (double-Esc clears the
// input), so the latency would be worse than the edge case it covers.
func detachIndex(p []byte) int {
	for i := 0; i < len(p); i++ {
		if p[i] == detachByte {
			return i
		}
		// gosec reads p[i+1] as unguarded; || short-circuits, so it is only
		// evaluated once i+1 < len(p) has been established on the same line.
		// #nosec G602 -- the bound is the preceding clause
		if p[i] != 0x1b || i+1 >= len(p) || p[i+1] != '[' {
			continue
		}
		params, final, end := parseCSI(p, i+2)
		if end < 0 {
			// Truncated sequence at the end of the chunk: nothing to match, and
			// nothing is held back (see the doc comment).
			return -1
		}
		if isDetachSeq(params, final) {
			return i
		}
		i = end - 1 // skip the sequence we just consumed
	}
	return -1
}

// parseCSI reads the parameter bytes and the final byte of a CSI sequence that
// starts at p[start] (i.e. just past "ESC["). It returns the raw parameter
// string, the final byte, and the offset just past the sequence, or end < 0 if
// the sequence is incomplete.
func parseCSI(p []byte, start int) (params []byte, final byte, end int) {
	for j := start; j < len(p); j++ {
		if p[j] >= 0x40 && p[j] <= 0x7e {
			return p[start:j], p[j], j + 1
		}
	}
	return nil, 0, -1
}

// isDetachSeq reports whether a parsed CSI sequence is Ctrl-].
func isDetachSeq(params []byte, final byte) bool {
	switch final {
	case 'u':
		// CSI <key> ; <mods> u
		f := splitParams(params)
		return len(f) >= 2 && f[0] == bracketKey && hasCtrl(f[1])
	case '~':
		// CSI 27 ; <mods> ; <key> ~
		f := splitParams(params)
		return len(f) >= 3 && f[0] == 27 && f[2] == bracketKey && hasCtrl(f[1])
	}
	return false
}

// splitParams parses a CSI parameter string into numbers. Sub-parameters after
// a colon are dropped: the kitty protocol appends event types there (93;5:1u),
// and only the leading value identifies the key or the modifier set.
func splitParams(params []byte) []int {
	var out []int
	start := 0
	for i := 0; i <= len(params); i++ {
		if i < len(params) && params[i] != ';' {
			continue
		}
		field := params[start:i]
		for k, b := range field {
			if b == ':' {
				field = field[:k]
				break
			}
		}
		n, err := strconv.Atoi(string(field))
		if err != nil {
			n = -1
		}
		out = append(out, n)
		start = i + 1
	}
	return out
}

// hasCtrl reports whether a CSI modifier parameter includes Control. The
// encoding is 1 + a bitmask, where Control is bit 2 (value 4), so plain Ctrl
// is 5.
func hasCtrl(mod int) bool {
	return mod > 0 && (mod-1)&4 != 0
}
