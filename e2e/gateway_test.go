package e2e_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/lufzle/lemul/internal/driver/docker"
)

// The model path, end to end: a real Claude Code in the real sandbox image,
// placed by the docker driver, completing a turn through the loopback proxy the
// supervisor owns, against a real LiteLLM.
//
// This is the one claim the product rests on that nothing else here proves.
// Earlier lab runs showed Claude Code works through a gateway and that header
// tags partition cost, but those used a hand-run container. The Fargate run of
// 2026-07-31 placed a task and ran Claude Code as uid 1000, but deliberately
// left the gateway on loopback, so a session started and could not complete a
// turn. Everything between those two -- our placement, our tunnel, our proxy,
// our attribution -- has never carried a real model call.
//
// Opt-in, because it needs a LiteLLM with a funded backend and it spends real
// money, a few cents a run:
//
//	# start a LiteLLM (or compatible) gateway on :4000
//	image/build.sh
//	LEMUL_E2E_GATEWAY=1 go test ./e2e -run TestGateway -v
//
// LEMUL_E2E_GATEWAY_URL overrides the gateway, LEMUL_E2E_GATEWAY_KEY its
// credential, and LEMUL_E2E_IMAGE the sandbox image.
//
// MEASURED COST: $0.23 for the FIRST turn and about $0.019 for each one after,
// per test. The gap is prompt caching -- Claude Code's system prompt is most of
// the bill, so the first call pays the cache-write premium on nearly all of it
// and later ones pay the read rate. Both figures are per test function, and
// each one also makes a haiku call of its own (~$0.0006), which Claude Code
// does on nearly every turn. Cheap enough to run by hand, not something to put
// in CI on every push -- and the cold figure is the one to budget with, since a
// cache that has expired looks exactly like a first run.
//
// TWO WAYS THIS TEST CAN LIE, both found by writing it and neither obvious:
//
//   - `Claude Code` matches the terminal TITLE escape (`ESC]0;✳ Claude Code
//     BEL`), which arrives in milliseconds. Waiting for it proves the PTY
//     exists and nothing else. The banner's own "Claude Code" is split by
//     cursor-positioning escapes and never appears as a literal string.
//   - Asking for a word and then waiting for that word matches the KEYSTROKES
//     echoing back into the input box. The first version of this passed in 3.3
//     seconds -- less than the container start -- against a model that had not
//     been called at all.
//
// So the question has an answer this test never types.

const (
	// gatewayFromSandbox is how a CONTAINER reaches a gateway on the host.
	// `localhost` inside a container is the container, which is the same trap
	// the ecs driver hits with -public-url and the docker driver papers over
	// for the control plane but not for this.
	gatewayFromSandbox = "http://host.docker.internal:4000"
	// gatewayFromTest is how THIS PROCESS reaches it, to read spend back.
	gatewayFromTest = "http://127.0.0.1:4000"

	// The pins must name models the gateway actually serves. A pin naming
	// something it does not is the failure deploy/litellm.yaml warns
	// about: the session starts and dies on its first prompt.
	gatewayPins = "opus=claude-opus-4-8,sonnet=claude-sonnet-4-6,haiku=claude-haiku-4-5"
)

func gatewayStack(t *testing.T) (*stack, string) {
	t.Helper()
	if os.Getenv("LEMUL_E2E_GATEWAY") == "" {
		t.Skip("set LEMUL_E2E_GATEWAY=1 to run against a real gateway; this spends money")
	}
	key := envOr("LEMUL_E2E_GATEWAY_KEY", "sk-lemul-spike")
	url := envOr("LEMUL_E2E_GATEWAY_URL", gatewayFromSandbox)
	image := envOr("LEMUL_E2E_IMAGE", "lemul-workspace:dev")

	// Fail rather than skip once opted in: a skip here would read as "the suite
	// passed" for the one property this file exists to check.
	if err := gatewayReachable(); err != nil {
		t.Fatalf("gateway not reachable at %s: %v\n"+
			"start a LiteLLM gateway on :4000 (see deploy/)",
			gatewayFromTest, err)
	}

	s := newStackWith(t, stackOptions{
		SessionCmd: []string{"claude"},
		Driver:     docker.New(image, nil),
		Image:      image,
		GatewayURL: url,
		GatewayKey: key,
		Pins:       gatewayPins,
	})
	return s, image
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func gatewayReachable() error {
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get(gatewayFromTest + "/health/liveliness")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health: %s", resp.Status)
	}
	return nil
}

// THE test. A prompt goes in and an answer comes back, through every hop the
// product actually has.
func TestGatewayCompletesARealTurn(t *testing.T) {
	s, _ := gatewayStack(t)
	s.newWorkspace("gw")
	sid := s.newSession("gw")

	c := s.attach(sid, 40, 120, "")
	awaitBooted(t, c)

	// The PIN is visible in the banner: Claude Code renders the model it will
	// use, derived from ANTHROPIC_DEFAULT_OPUS_MODEL, which the image's managed
	// settings render from -pins. So this asserts the pin travelled all the way
	// from a control-plane flag into the client's own idea of what it is
	// talking to.
	if got := c.snapshot(); !strings.Contains(string(got), "Opus") {
		t.Errorf("the banner does not name the pinned model; screen:\n%q", tail(string(got), 2000))
	}

	askAndAwait(t, c)
	c.detach()
}

// awaitBooted waits for the TUI to finish drawing.
//
// Matched on the status line rather than on anything containing "Claude Code",
// for the reason in the file comment. Version-sensitive by nature -- this test
// drives the real client, so its screen is part of the contract being checked.
func awaitBooted(t *testing.T, c *conn) {
	t.Helper()
	if got := c.await("manual", 150*time.Second); !strings.Contains(got, "manual") {
		t.Fatalf("Claude Code never finished drawing; screen:\n%q", tail(got, 3000))
	}
}

// askAndAwait puts a question whose ANSWER is a string this test never sends.
//
// 7919 x 13 = 102947. Arithmetic rather than a nonsense word because the answer
// has to be absent from the prompt: anything quoted back would match the input
// box echoing, which is exactly how the first version of this passed without
// calling a model.
func askAndAwait(t *testing.T, c *conn) {
	t.Helper()
	c.writeBytes([]byte("What is 7919 multiplied by 13? Reply with only the digits.\r"))
	got := c.await("102947", 180*time.Second)
	if !strings.Contains(got, "102947") {
		t.Fatalf("no model answer came back through the gateway.\n"+
			"A pin naming a model this gateway does not serve looks exactly like this.\n"+
			"screen:\n%q", tail(got, 4000))
	}
}

// Spend must be attributed to the workspace and session that incurred it, from
// the supervisor's own port mapping rather than from anything the session can
// say about itself.
//
// This is the half of decision #12 that makes per-workspace budgets possible at
// all: without attribution the gateway sees one key spending money and nothing
// to divide it by. Earlier lab runs measured it against a hand-run container;
// this measures it against a workspace WE placed, with tags the session never
// supplied.
func TestGatewayAttributesSpendToTheWorkspaceAndSession(t *testing.T) {
	s, _ := gatewayStack(t)
	s.newWorkspace("gw-spend")
	sid := s.newSession("gw-spend")

	c := s.attach(sid, 40, 120, "")
	awaitBooted(t, c)
	askAndAwait(t, c)
	c.detach()

	// Read the cost from /spend/logs, never from a response header -- the
	// header is what the spike found unreliable, and the gateway is the source
	// of record (section 12.4).
	logs := awaitSpend(t, sid, 90*time.Second)
	if len(logs) == 0 {
		t.Fatal("the gateway recorded no spend for this session, so nothing can be billed to it")
	}
	for _, l := range logs {
		tags := requestTags(l)
		if !tags["workspace:gw-spend"] {
			t.Errorf("spend not attributed to the workspace; tags: %v", sortedKeys(tags))
		}
		if !tags["session:"+sid] {
			t.Errorf("spend not attributed to session %s; tags: %v", sid, sortedKeys(tags))
		}
	}
	// Claude Code reaches for haiku alongside its main model on nearly every
	// turn, and BOTH have to be attributable or a workspace budget undercounts.
	t.Logf("%d attributed spend entries for session %s", len(logs), sid)
}

// requestTags reads the attribution the supervisor's proxy injected.
//
// They arrive as LiteLLM `request_tags` -- `workspace:<name>`, `session:<uuid>`
// -- and the session cannot forge them: the supervisor derives both from its
// own port mapping rather than from anything the request carries. Verified from
// inside a sandbox during the spike, where a call with a forged credential AND
// forged tags was still recorded against the correct workspace and session.
func requestTags(l map[string]any) map[string]bool {
	out := map[string]bool{}
	raw, _ := l["request_tags"].([]any)
	for _, t := range raw {
		if s, ok := t.(string); ok {
			out[s] = true
		}
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// awaitSpend polls /spend/logs, because LiteLLM writes them asynchronously
// after answering -- reading once immediately would be a race that fails for
// the wrong reason.
func awaitSpend(t *testing.T, sid string, d time.Duration) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if got := spendLogs(t, sid); len(got) > 0 {
			return got
		}
		time.Sleep(2 * time.Second)
	}
	return nil
}

func spendLogs(t *testing.T, sid string) []map[string]any {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		gatewayFromTest+"/spend/logs", nil)
	if err != nil {
		t.Fatalf("spend logs: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+envOr("LEMUL_E2E_GATEWAY_KEY", "sk-lemul-spike"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("spend logs: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("spend logs: %s: %s", resp.Status, tail(string(body), 500))
	}
	var all []map[string]any
	if err := json.Unmarshal(body, &all); err != nil {
		t.Fatalf("spend logs: %v (%s)", err, tail(string(body), 500))
	}
	var mine []map[string]any
	for _, l := range all {
		if strings.Contains(fmt.Sprint(l), sid) {
			mine = append(mine, l)
		}
	}
	return mine
}
