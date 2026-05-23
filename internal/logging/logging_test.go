package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/lufzle/lemul/internal/creds"
)

func TestJSONFormatIsQueryable(t *testing.T) {
	var buf bytes.Buffer
	build(&buf, "controlplane", "json", "").Info("workspace dispatched",
		"workspace", "w1", "generation", 4)

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("json format did not emit valid JSON: %v (%q)", err, buf.String())
	}
	// These are the fields a CloudWatch Insights query selects on; a message
	// that folded them into the text would be back to needing a regex.
	for k, want := range map[string]any{
		"service":   "controlplane",
		"workspace": "w1",
		"msg":       "workspace dispatched",
	} {
		if got[k] != want {
			t.Errorf("field %q = %v, want %v", k, got[k], want)
		}
	}
	if got["generation"] != float64(4) {
		t.Errorf("generation = %v, want 4", got["generation"])
	}
}

// Text is the default because the format that is wrong in production is merely
// ugly, while the one that is wrong locally makes development worse.
func TestTextIsTheDefault(t *testing.T) {
	for _, format := range []string{"", "text", "TEXT", "nonsense"} {
		var buf bytes.Buffer
		build(&buf, "runner", format, "").Info("hello")
		if json.Valid(buf.Bytes()) {
			t.Errorf("format %q produced JSON; text is the default", format)
		}
		if !strings.Contains(buf.String(), "service=runner") {
			t.Errorf("format %q lost the service attribute: %q", format, buf.String())
		}
	}
}

func TestLevelFiltersOutput(t *testing.T) {
	for _, tc := range []struct {
		level               string
		wantDebug, wantWarn bool
	}{
		{"", false, true}, // info by default
		{"debug", true, true},
		{"warn", false, true},
		{"error", false, false},
	} {
		var buf bytes.Buffer
		l := build(&buf, "s", "text", tc.level)
		l.Debug("dbg")
		l.Warn("wrn")
		out := buf.String()
		if strings.Contains(out, "dbg") != tc.wantDebug {
			t.Errorf("level %q: debug visible = %v, want %v", tc.level, !tc.wantDebug, tc.wantDebug)
		}
		if strings.Contains(out, "wrn") != tc.wantWarn {
			t.Errorf("level %q: warn visible = %v, want %v", tc.level, !tc.wantWarn, tc.wantWarn)
		}
	}
}

// The reason this migration was worth doing. A credential reaching a log line
// must be redacted by the type rather than by whoever wrote the call, in either
// format -- JSON serialises attributes through a different path than text, so
// both are checked.
func TestCredentialsAreRedactedInBothFormats(t *testing.T) {
	signer, err := creds.NewSigner(bytes.Repeat([]byte{9}, creds.MinKeyBytes))
	if err != nil {
		t.Fatal(err)
	}
	ws := signer.WorkspaceToken("t1", "w1", 1)
	at, err := signer.AttachToken("s1", "t1", "w1", "org", "control", creds.AttachTTL)
	if err != nil {
		t.Fatal(err)
	}

	for _, format := range []string{"text", "json"} {
		var buf bytes.Buffer
		build(&buf, "s", format, "").Info("credentials", "workspace_cred", ws, "attach_cred", at)
		out := buf.String()
		if strings.Contains(out, ws.Secret()) {
			t.Errorf("%s: the workspace credential leaked: %s", format, out)
		}
		if strings.Contains(out, at.Secret()) {
			t.Errorf("%s: the attach credential leaked: %s", format, out)
		}
		if !strings.Contains(out, "<redacted>") {
			t.Errorf("%s: no redaction marker: %s", format, out)
		}
	}
}

// yamux writes pre-formatted lines to an io.Writer. Left on stderr they bypass
// the handler entirely -- unlevelled, unstructured, and untouched by the JSON
// format everything else obeys.
func TestWriterRoutesLibraryOutputThroughTheHandler(t *testing.T) {
	var buf bytes.Buffer
	w := Writer(build(&buf, "s", "json", "debug"), slog.LevelDebug)

	n, err := w.Write([]byte("[ERR] yamux: keepalive failed\n"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len("[ERR] yamux: keepalive failed\n") {
		t.Errorf("Write reported %d bytes; a short count would make yamux retry", n)
	}

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("library output did not go through the handler: %q", buf.String())
	}
	if got["msg"] != "[ERR] yamux: keepalive failed" {
		t.Errorf("msg = %v, want the line without its newline", got["msg"])
	}

	// A bare newline carries nothing; emitting a line for it would turn yamux's
	// spacing into log entries.
	buf.Reset()
	if _, err := w.Write([]byte("\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("an empty line produced output: %q", buf.String())
	}
}
