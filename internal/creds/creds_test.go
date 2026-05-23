package creds

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func testSigner(t *testing.T) *Signer {
	t.Helper()
	s, err := NewSigner(bytes.Repeat([]byte{0xA5}, MinKeyBytes))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return s
}

func TestNewSignerRejectsShortKeys(t *testing.T) {
	for _, n := range []int{0, 1, MinKeyBytes - 1} {
		if _, err := NewSigner(bytes.Repeat([]byte{1}, n)); err == nil {
			t.Errorf("NewSigner accepted a %d-byte key; HMAC would not fail loudly, "+
				"it would just be weak forever", n)
		}
	}
	if _, err := NewSigner(bytes.Repeat([]byte{1}, MinKeyBytes)); err != nil {
		t.Errorf("NewSigner rejected a %d-byte key: %v", MinKeyBytes, err)
	}
}

// The signer must not alias the caller's key: a caller that reuses or zeroes its
// buffer would otherwise silently change every credential this signer derives.
func TestNewSignerCopiesTheKey(t *testing.T) {
	key := bytes.Repeat([]byte{7}, MinKeyBytes)
	s, err := NewSigner(key)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	before := s.WorkspaceToken("t1", "w1", 1)
	for i := range key {
		key[i] = 0
	}
	if after := s.WorkspaceToken("t1", "w1", 1); after != before {
		t.Fatal("mutating the caller's key changed derived credentials; key was aliased")
	}
}

// The property the whole package exists for: a restarted control plane, holding
// the same configured key, re-derives the credential a live task is already
// holding.
func TestWorkspaceTokenSurvivesRestart(t *testing.T) {
	key := bytes.Repeat([]byte{0x3C}, MinKeyBytes)
	before, _ := NewSigner(key)
	tok := before.WorkspaceToken("t1", "myproj", 4)

	after, _ := NewSigner(key) // a new process, same configured key
	if !after.VerifyWorkspace("t1", "myproj", 4, tok.Secret()) {
		t.Fatal("a restarted signer did not recognise its predecessor's credential; " +
			"every live workspace task would be orphaned")
	}
}

func TestWorkspaceTokenBindsToGenerationAndWorkspace(t *testing.T) {
	s := testSigner(t)
	tok := s.WorkspaceToken("t1", "w1", 3)

	if !s.VerifyWorkspace("t1", "w1", 3, tok.Secret()) {
		t.Fatal("the credential did not verify for its own workspace and generation")
	}
	// A new placement increments the generation, and that is the ONLY thing that
	// may invalidate a live task's credential.
	if s.VerifyWorkspace("t1", "w1", 4, tok.Secret()) {
		t.Error("a credential verified against a later generation; placing a " +
			"replacement task would not lock out the task it replaces")
	}
	if s.VerifyWorkspace("t1", "w1", 2, tok.Secret()) {
		t.Error("a credential verified against an earlier generation")
	}
	if s.VerifyWorkspace("t1", "w2", 3, tok.Secret()) {
		t.Error("a credential verified for a different workspace")
	}
}

func TestVerifyWorkspaceRejectsJunk(t *testing.T) {
	s := testSigner(t)
	for name, tok := range map[string]string{
		"empty":       "",
		"not base64":  "!!!!",
		"wrong value": base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0}, 32)),
		"truncated":   s.WorkspaceToken("t1", "w1", 1).Secret()[:10],
	} {
		if s.VerifyWorkspace("t1", "w1", 1, tok) {
			t.Errorf("%s verified as a workspace credential", name)
		}
	}
}

// Length-prefixing the MAC input is what stops this. Concatenating the fields
// would make ("a", 12) and ("a1", 2) hash identically, so a credential for one
// workspace would open another. Workspace ids are user-supplied names today, so
// an attacker picks the name.
func TestWorkspaceTokenEncodingIsUnambiguous(t *testing.T) {
	s := testSigner(t)
	if s.WorkspaceToken("t1", "a", 12) == s.WorkspaceToken("t1", "a1", 2) {
		t.Fatal("(a,12) and (a1,2) derived the same credential: the MAC input is " +
			"ambiguous, so one workspace's credential opens another")
	}
	if s.WorkspaceToken("t1", "", 112) == s.WorkspaceToken("t1", "1", 12) {
		t.Fatal("ambiguous MAC input across an empty workspace id")
	}
}

func TestForgeryNeedsTheKey(t *testing.T) {
	mine := testSigner(t)
	theirs, _ := NewSigner(bytes.Repeat([]byte{0x11}, MinKeyBytes))

	if mine.VerifyWorkspace("t1", "w1", 1, theirs.WorkspaceToken("t1", "w1", 1).Secret()) {
		t.Error("a workspace credential minted under a different key verified")
	}

	forged, err := theirs.AttachToken("s1", "t1", "w1", "org", "control", AttachTTL)
	if err != nil {
		t.Fatalf("AttachToken: %v", err)
	}
	if _, err := mine.VerifyAttach(forged.Secret()); !errors.Is(err, ErrSignature) {
		t.Errorf("an attach credential minted under a different key gave %v, want %v",
			err, ErrSignature)
	}
}

func TestAttachRoundTrip(t *testing.T) {
	s := testSigner(t)
	tok, err := s.AttachToken("sess-1", "t1", "ws-1", "org", "viewer", AttachTTL)
	if err != nil {
		t.Fatalf("AttachToken: %v", err)
	}
	got, err := s.VerifyAttach(tok.Secret())
	if err != nil {
		t.Fatalf("VerifyAttach: %v", err)
	}
	if got.SessionID != "sess-1" || got.WorkspaceName != "ws-1" || got.Mode != "viewer" {
		t.Errorf("claims round-tripped wrong: %+v", got)
	}
	if got.Nonce == "" {
		t.Error("no nonce; single-use enforcement has nothing to remember")
	}
	if d := time.Until(got.ExpiresAt); d <= 0 || d > AttachTTL+time.Second {
		t.Errorf("expiry %v is not within the TTL", d)
	}
}

// The relay authorises from the token alone, so every field it routes and gates
// on must survive verbatim -- including values that collide with the encoding's
// own punctuation.
func TestAttachClaimsSurviveAwkwardValues(t *testing.T) {
	s := testSigner(t)
	for _, tc := range []struct{ name, sid, wid, mode string }{
		{"colons", "3:x", "12:ab", "control"},
		{"empty mode", "s", "w", ""},
		{"all empty", "", "", ""},
		{"digits only", "123", "456", "789"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tok, err := s.AttachToken(tc.sid, "t1", tc.wid, "org", tc.mode, AttachTTL)
			if err != nil {
				t.Fatalf("AttachToken: %v", err)
			}
			got, err := s.VerifyAttach(tok.Secret())
			if err != nil {
				t.Fatalf("VerifyAttach: %v", err)
			}
			if got.SessionID != tc.sid || got.WorkspaceName != tc.wid || got.Mode != tc.mode {
				t.Errorf("got %+v, want (%q,%q,%q)", got, tc.sid, tc.wid, tc.mode)
			}
		})
	}
}

func TestAttachExpires(t *testing.T) {
	s := testSigner(t)
	tok, err := s.AttachToken("s1", "t1", "w1", "org", "control", -time.Second)
	if err != nil {
		t.Fatalf("AttachToken: %v", err)
	}
	// A negative TTL falls back to the default rather than minting something
	// already dead, so this must still be valid.
	if _, err := s.VerifyAttach(tok.Secret()); err != nil {
		t.Fatalf("a non-positive TTL should fall back to the default, got %v", err)
	}

	expired, err := s.AttachToken("s1", "t1", "w1", "org", "control", time.Nanosecond)
	if err != nil {
		t.Fatalf("AttachToken: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := s.VerifyAttach(expired.Secret()); !errors.Is(err, ErrExpired) {
		t.Errorf("expired token gave %v, want %v", err, ErrExpired)
	}
}

func TestAttachRejectsTampering(t *testing.T) {
	s := testSigner(t)
	tok, err := s.AttachToken("s1", "t1", "w1", "org", "viewer", AttachTTL)
	if err != nil {
		t.Fatalf("AttachToken: %v", err)
	}
	raw := tok.Secret()
	payload, mac, _ := strings.Cut(raw, ".")

	// Re-signing a rewritten payload is exactly the attack: promote a viewer to
	// a controller, or point the token at another session.
	decoded, _ := base64.RawURLEncoding.DecodeString(payload)
	swapped := strings.Replace(string(decoded), "6:viewer", "7:control", 1)
	rewritten := base64.RawURLEncoding.EncodeToString([]byte(swapped)) + "." + mac
	if _, err := s.VerifyAttach(rewritten); err == nil {
		t.Fatal("a rewritten payload verified: a viewer could promote itself to controller")
	}

	// Flip a bit in the DECODED mac and re-encode, so what comes back is still
	// valid base64 and therefore reaches the signature comparison. Flipping a
	// bit in the base64 text instead sometimes lands on a character outside the
	// alphabet, and the token is then rejected as malformed -- the right outcome
	// for the wrong reason, and only on some runs, because the nonce changes the
	// mac each time.
	macBytes, err := base64.RawURLEncoding.DecodeString(mac)
	if err != nil {
		t.Fatalf("decode mac: %v", err)
	}
	macBytes[0] ^= 0x01
	flipped := payload + "." + base64.RawURLEncoding.EncodeToString(macBytes)
	if _, err := s.VerifyAttach(flipped); !errors.Is(err, ErrSignature) {
		t.Errorf("a flipped signature bit gave %v, want %v", err, ErrSignature)
	}
}

func TestVerifyAttachRejectsMalformed(t *testing.T) {
	s := testSigner(t)
	valid := func() string {
		tok, _ := s.AttachToken("s1", "t1", "w1", "org", "control", AttachTTL)
		return tok.Secret()
	}()
	payload, mac, _ := strings.Cut(valid, ".")

	for name, tok := range map[string]string{
		"empty":            "",
		"no separator":     payload,
		"payload only":     payload + ".",
		"mac only":         "." + mac,
		"bad base64 body":  "!!!." + mac,
		"bad base64 mac":   payload + ".!!!",
		"extra separator":  valid + "." + mac,
		"swapped halves":   mac + "." + payload,
		"empty both sides": ".",
	} {
		if _, err := s.VerifyAttach(tok); err == nil {
			t.Errorf("%s verified", name)
		}
	}
}

// decodeAttach parses attacker-supplied bytes. It only runs after the signature
// checks out, but it is tested directly because "unreachable" is a property of
// today's call order, not of the function.
func TestDecodeAttachRejectsMalformedPayloads(t *testing.T) {
	for name, payload := range map[string]string{
		"no fields":        "",
		"too few":          "2:ab",
		"length overruns":  "9:ab",
		"negative length":  "-1:ab",
		"non-numeric":      "x:ab",
		"missing colon":    "2ab",
		"too many fields":  "1:a1:a1:a1:a1:a1:a",
		"bad expiry":       "1:a1:a1:a3:xyz1:a",
		"trailing garbage": "1:a1:a1:a1:11:a!",
	} {
		if _, err := decodeAttach(payload); err == nil {
			t.Errorf("%s (%q) decoded without error", name, payload)
		}
	}
}

func TestNoncesDiffer(t *testing.T) {
	s := testSigner(t)
	seen := make(map[string]bool)
	for range 100 {
		tok, err := s.AttachToken("s1", "t1", "w1", "org", "control", AttachTTL)
		if err != nil {
			t.Fatalf("AttachToken: %v", err)
		}
		c, err := s.VerifyAttach(tok.Secret())
		if err != nil {
			t.Fatalf("VerifyAttach: %v", err)
		}
		if seen[c.Nonce] {
			t.Fatal("nonce repeated; single-use enforcement would reject a legitimate attach")
		}
		seen[c.Nonce] = true
	}
}

// Domain separation: the two credential types must not be interchangeable even
// though both are HMACs under one key.
func TestCredentialTypesAreNotInterchangeable(t *testing.T) {
	s := testSigner(t)

	ws := s.WorkspaceToken("t1", "w1", 1)
	if _, err := s.VerifyAttach(ws.Secret()); err == nil {
		t.Error("a workspace credential was accepted as an attach credential")
	}

	at, err := s.AttachToken("s1", "t1", "w1", "org", "control", AttachTTL)
	if err != nil {
		t.Fatalf("AttachToken: %v", err)
	}
	if s.VerifyWorkspace("t1", "w1", 1, at.Secret()) {
		t.Error("an attach credential was accepted as a workspace credential")
	}
}

// The redaction is a security property, so it is tested like one. Both routes a
// credential can reach a log line are covered: slog attributes and fmt verbs.
func TestCredentialsRedactThemselves(t *testing.T) {
	s := testSigner(t)
	ws := s.WorkspaceToken("t1", "w1", 1)
	at, err := s.AttachToken("s1", "t1", "w1", "org", "control", AttachTTL)
	if err != nil {
		t.Fatalf("AttachToken: %v", err)
	}

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	log.Info("cred", "workspace", ws, "attach", at)
	// %v and %s are the fmt paths; both must go through String.
	buf.WriteString(strings.Join([]string{
		strings.TrimSpace(ws.String()),
		strings.TrimSpace(at.String()),
	}, " "))

	out := buf.String()
	for name, secret := range map[string]string{"workspace": ws.Secret(), "attach": at.Secret()} {
		if strings.Contains(out, secret) {
			t.Errorf("the %s credential appeared in log output: %s", name, out)
		}
	}
	if !strings.Contains(out, "<redacted>") {
		t.Errorf("no redaction marker in output: %s", out)
	}
}

// The credential must not be transferable between organizations.
//
// A workspace NAME is unique only within one -- `UNIQUE (tenant_id, name)` --
// so two customers can both have a workspace called `api`, at generation 1, at
// the same time. Before the organization was part of the derivation those two
// had the same credential, which means either task could have authenticated as
// the other's and been handed its tunnel.
func TestWorkspaceTokenBindsToTheOrganization(t *testing.T) {
	s := testSigner(t)

	a := s.WorkspaceToken("org-a", "api", 1)
	b := s.WorkspaceToken("org-b", "api", 1)
	if a == b {
		t.Fatal("two organizations' workspaces of the same name share a credential")
	}
	if s.VerifyWorkspace("org-b", "api", 1, a.Secret()) {
		t.Error("org A's workspace credential verified against org B")
	}
	if !s.VerifyWorkspace("org-a", "api", 1, a.Secret()) {
		t.Error("the credential does not verify for its own organization")
	}
}

func TestRunnerTokenBindsToTheOrganization(t *testing.T) {
	s := testSigner(t)

	a := s.RunnerToken("org-a")
	if !s.VerifyRunner("org-a", a.Secret()) {
		t.Fatal("a runner credential does not verify for its own organization")
	}
	// The whole point of replacing the shared agent token: holding one
	// organization's credential must not let a runner register as another and
	// receive its workspace placements.
	if s.VerifyRunner("org-b", a.Secret()) {
		t.Error("org A's runner credential registered as org B")
	}
	if s.VerifyRunner("org-a", "") {
		t.Error("an empty credential verified")
	}
}

// Domain separation, extended to the third credential. Without it a value that
// is a valid runner token could be presented wherever a workspace credential is
// expected, if the field layouts ever lined up.
func TestRunnerTokensAreNotWorkspaceTokens(t *testing.T) {
	s := testSigner(t)

	runner := s.RunnerToken("org-a")
	if s.VerifyWorkspace("org-a", "org-a", 0, runner.Secret()) {
		t.Error("a runner credential verified as a workspace credential")
	}
	if _, err := s.VerifyAttach(runner.Secret()); err == nil {
		t.Error("a runner credential verified as an attach credential")
	}
	ws := s.WorkspaceToken("org-a", "w1", 1)
	if s.VerifyRunner("org-a", ws.Secret()) {
		t.Error("a workspace credential verified as a runner credential")
	}
}

func TestRunnerTokenIsRedactedWhenLogged(t *testing.T) {
	tok := testSigner(t).RunnerToken("org-a")
	if got := tok.String(); got != "<redacted>" {
		t.Errorf("String() = %q, want <redacted>", got)
	}
	if got := fmt.Sprintf("%v", tok); got != "<redacted>" {
		t.Errorf("%%v = %q, want <redacted>", got)
	}
	if tok.Secret() == "<redacted>" {
		t.Error("Secret() must return the real value")
	}
}
