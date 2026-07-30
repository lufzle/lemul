package supervisor

import (
	"os"
	"strings"
	"testing"
)

// Claude Code's Bash tool is a child process and inherits the environment
// wholesale -- verified against 2.1.220, where AWS_ACCESS_KEY_ID,
// AWS_SESSION_TOKEN, AWS_CONTAINER_CREDENTIALS_RELATIVE_URI and
// ANTHROPIC_AUTH_TOKEN all reached a Bash command intact.
//
// So anything left in a session's environment is handed to every command the
// agent runs, including commands the model wrote. This is the test that says the
// gateway credential is not one of them.
func TestChildEnvStripsCredentials(t *testing.T) {
	t.Setenv("LEMUL_GATEWAY_KEY", "sk-REAL-SECRET")
	t.Setenv("LEMUL_TOKEN", "workspace-tunnel-credential")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "stale-inherited-token")
	t.Setenv("ANTHROPIC_API_KEY", "stale-api-key")
	t.Setenv("ANTHROPIC_BASE_URL", "http://stale-base-url")
	t.Setenv("PATH_MARKER_KEEP", "this-must-survive")

	env := childEnv("xterm-256color")
	joined := strings.Join(env, "\n")

	for _, secret := range []string{"sk-REAL-SECRET", "workspace-tunnel-credential",
		"stale-inherited-token", "stale-api-key", "http://stale-base-url"} {
		if strings.Contains(joined, secret) {
			t.Errorf("secret reached the session environment: %q", secret)
		}
	}
	if !strings.Contains(joined, "PATH_MARKER_KEEP=this-must-survive") {
		t.Error("childEnv dropped an ordinary variable; sessions need a working environment")
	}
	if !strings.Contains(joined, "TERM=xterm-256color") || !strings.Contains(joined, "COLORTERM=truecolor") {
		t.Error("terminal environment missing; rendering would degrade (section 4.1)")
	}
}

// In gateway mode the session is pointed at its own loopback proxy and given a
// placeholder, so the values it holds are worthless if exfiltrated.
func TestSessionEnvPointsAtTheLoopbackProxy(t *testing.T) {
	t.Setenv("LEMUL_GATEWAY_KEY", "sk-REAL-SECRET")

	s := New(Options{
		WorkspaceID: "w1",
		GatewayURL:  "http://127.0.0.1:65535",
		GatewayKey:  "sk-REAL-SECRET",
	})
	if s.broker == nil {
		t.Fatal("gateway mode did not create a broker")
	}
	t.Cleanup(func() { s.broker.CloseAll() })

	env, err := s.sessionEnv("s-1", "xterm-256color")
	if err != nil {
		t.Fatalf("sessionEnv: %v", err)
	}
	joined := strings.Join(env, "\n")

	if strings.Contains(joined, "sk-REAL-SECRET") {
		t.Fatal("the gateway credential reached the session environment")
	}
	if !strings.Contains(joined, "ANTHROPIC_BASE_URL=http://127.0.0.1:") {
		t.Errorf("session is not pointed at a loopback proxy:\n%s", joined)
	}
	if !strings.Contains(joined, "ANTHROPIC_AUTH_TOKEN=sk-lemul-loopback-not-a-secret") {
		t.Error("session was not given the placeholder token")
	}
}

// Without a gateway configured nothing should be pointed anywhere.
func TestSessionEnvIsPlainWithoutAGateway(t *testing.T) {
	s := New(Options{WorkspaceID: "w1"})
	env, err := s.sessionEnv("s-1", "xterm-256color")
	if err != nil {
		t.Fatalf("sessionEnv: %v", err)
	}
	if strings.Contains(strings.Join(env, "\n"), "ANTHROPIC_BASE_URL=") {
		t.Error("a base URL was injected with no gateway configured")
	}
}

func TestRedactURLDropsUserinfo(t *testing.T) {
	if got := redactURL("https://user:pass@litellm.internal:4000"); strings.Contains(got, "pass") {
		t.Errorf("redactURL leaked credentials: %s", got)
	}
	_ = os.Environ()
}
