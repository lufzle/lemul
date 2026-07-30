package supervisor

import (
	"os"
	"path/filepath"
	"strings"
)

// Claude Code's session flags are complementary, and each fails in the other's
// case. Measured against the pinned 2.1.220:
//
//	                     transcript exists          transcript absent
//	--session-id <id>    "Session ID … is           starts a fresh
//	                      already in use"           conversation
//	--resume <id>        resumes it, reusing        "No conversation found
//	                      the same id               with session ID: …"
//
// So the argv depends on a fact only the workspace filesystem holds, and picking
// wrong is not a degraded session -- it is a process that exits immediately with
// a message about session IDs, which reads as our bug.
//
// The control plane deliberately does not decide this. It could remember that a
// session was started once, but that belongs to a task generation: decision #3
// puts the workspace on Fargate ephemeral disk, so a replacement task comes up
// with the transcript gone while the flag still says "started". The supervisor is
// standing on the disk, which makes it the only honest answer -- the same reason
// section 12.3 moved the Bedrock preflight here and handleListSessions asks the
// supervisor for liveness rather than trusting our own bookkeeping.
//
// Why our session ids are UUIDs: --session-id demands one, and adopting the
// format makes our id BE the conversation id. That buys a stable id across
// resumes (verified: --resume reuses it, only --fork-session mints a new one),
// which is the property section 2.4 wants for vertical migration, and it
// collapses the workaround in section 12.4 -- the gateway's native Session ID
// and OTel's session.id now carry our id with nothing to map.

// resumeSupport reports whether cmd is a Claude Code invocation we may rewrite.
//
// Gated deliberately. The same create path runs whatever -session-cmd names, and
// the e2e fidelity suite drives it with cat and sh; appending --resume to those
// would break tests for no reason and, worse, would silently pass an unknown flag
// to whatever a customer configures.
func resumeSupported(cmd []string) bool {
	if len(cmd) == 0 {
		return false
	}
	base := filepath.Base(cmd[0])
	return base == "claude" || base == "claude.js"
}

// sessionArgs returns the command for one session, resuming its conversation if
// one is already on disk.
//
// An explicit session flag already in cmd wins and is left alone: an operator who
// configured -session-cmd 'claude --resume something' means it, and silently
// adding a second --session-id would produce an argv Claude Code rejects.
func sessionArgs(cmd []string, sessionID, configDir string) []string {
	if !resumeSupported(cmd) || sessionID == "" || hasSessionFlag(cmd) {
		return cmd
	}
	out := make([]string, len(cmd), len(cmd)+2)
	copy(out, cmd)
	if transcriptExists(configDir, sessionID) {
		return append(out, "--resume", sessionID)
	}
	return append(out, "--session-id", sessionID)
}

func hasSessionFlag(cmd []string) bool {
	for _, a := range cmd[1:] {
		switch {
		case a == "--session-id" || strings.HasPrefix(a, "--session-id="),
			a == "--resume" || a == "-r" || strings.HasPrefix(a, "--resume="),
			a == "--continue" || a == "-c":
			return true
		}
	}
	return false
}

// transcriptExists reports whether Claude Code already has a conversation for
// this session id.
//
// Globbed across project directories rather than derived, on purpose. Claude Code
// files a transcript under a directory named from the mangled working directory,
// and reproducing that mangling would couple us to an internal scheme with no
// stable contract. Globbing couples us only to "the transcript is <id>.jsonl
// somewhere under projects/", which is far less likely to move -- and the Claude
// Code version is pinned in the image anyway (section 13, version skew).
//
// A false negative here is recoverable in the way that matters: we pass
// --session-id, Claude Code says the id is in use, and the session fails loudly
// at start rather than silently losing the conversation.
func transcriptExists(configDir, sessionID string) bool {
	if configDir == "" {
		configDir = defaultConfigDir()
	}
	if configDir == "" {
		return false
	}
	matches, err := filepath.Glob(filepath.Join(configDir, "projects", "*", sessionID+".jsonl"))
	return err == nil && len(matches) > 0
}

// removeTranscript drops a session's conversation. Used by delete_session, which
// is the difference between stopping a session and forgetting it (section 2.6).
func removeTranscript(configDir, sessionID string) error {
	if configDir == "" {
		configDir = defaultConfigDir()
	}
	if configDir == "" || sessionID == "" {
		return nil
	}
	matches, err := filepath.Glob(filepath.Join(configDir, "projects", "*", sessionID+".jsonl"))
	if err != nil {
		return err
	}
	for _, m := range matches {
		if err := os.Remove(m); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// defaultConfigDir mirrors Claude Code's own resolution. The image sets
// CLAUDE_CONFIG_DIR=/workspace/.claude so the conversation lands on the workspace
// volume rather than in the container's home -- which is what makes a
// conversation outlive its process at all (section 2.4).
func defaultConfigDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}
