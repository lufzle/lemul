package gateway

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// upstream records what the gateway actually received.
type upstream struct {
	*httptest.Server
	gotAuth   string
	gotAPIKey string
	gotTags   string
	gotUser   string
	gotBody   string
}

func newUpstream(t *testing.T, handler http.HandlerFunc) *upstream {
	t.Helper()
	u := &upstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.gotAuth = r.Header.Get("Authorization")
		u.gotAPIKey = r.Header.Get("x-api-key")
		u.gotTags = r.Header.Get("x-litellm-tags")
		u.gotUser = r.Header.Get("x-litellm-end-user-id")
		b, _ := io.ReadAll(r.Body)
		u.gotBody = string(b)
		if handler != nil {
			handler(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"type":"message","content":[{"type":"text","text":"ok"}]}`)
	}))
	t.Cleanup(u.Close)
	return u
}

func newBroker(t *testing.T, up *upstream, user string) *Broker {
	t.Helper()
	b, err := New(Options{Upstream: up.URL, APIKey: "sk-REAL-SECRET", WorkspaceID: "w1", UserID: user})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(b.CloseAll)
	return b
}

// The whole point of the proxy: the real credential is added on the way out, and
// whatever the session sent is discarded. A session that forged an Authorization
// header must not be able to reach the gateway as anyone else.
func TestProxyReplacesSessionCredentials(t *testing.T) {
	up := newUpstream(t, nil)
	b := newBroker(t, up, "dario")

	sp, err := b.Open("s-1")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	req, _ := http.NewRequest("POST", sp.BaseURL+"/v1/messages", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer sk-FORGED-BY-SESSION")
	req.Header.Set("x-api-key", "sk-ALSO-FORGED")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()

	if up.gotAuth != "Bearer sk-REAL-SECRET" {
		t.Errorf("upstream Authorization = %q, want the real key", up.gotAuth)
	}
	if strings.Contains(up.gotAuth, "FORGED") || strings.Contains(up.gotAPIKey, "FORGED") {
		t.Errorf("a session-supplied credential reached the gateway: auth=%q x-api-key=%q",
			up.gotAuth, up.gotAPIKey)
	}
}

// Attribution must come from the supervisor's port-to-session mapping, not from
// anything the session can set -- otherwise cost partitioning is advisory.
func TestProxyAttributionCannotBeForged(t *testing.T) {
	up := newUpstream(t, nil)
	b := newBroker(t, up, "dario")

	sp, _ := b.Open("s-real")
	req, _ := http.NewRequest("POST", sp.BaseURL+"/v1/messages", strings.NewReader("{}"))
	req.Header.Set("x-litellm-tags", "workspace:someone-elses,session:s-forged")
	req.Header.Set("x-litellm-end-user-id", "not-dario")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()

	if strings.Contains(up.gotTags, "s-forged") || strings.Contains(up.gotTags, "someone-elses") {
		t.Errorf("session-supplied tags survived: %q", up.gotTags)
	}
	for _, want := range []string{"workspace:w1", "session:s-real", "user:dario"} {
		if !strings.Contains(up.gotTags, want) {
			t.Errorf("tags %q missing %q", up.gotTags, want)
		}
	}
	if up.gotUser != "dario" {
		t.Errorf("end-user header = %q, want dario", up.gotUser)
	}
}

// Each session gets its own listener; that separation IS the attribution.
func TestEachSessionGetsItsOwnPortAndTag(t *testing.T) {
	up := newUpstream(t, nil)
	b := newBroker(t, up, "")

	a, _ := b.Open("s-a")
	c, _ := b.Open("s-b")
	if a.BaseURL == c.BaseURL {
		t.Fatalf("both sessions share a listener: %s", a.BaseURL)
	}

	for _, tc := range []struct{ url, want string }{{a.BaseURL, "session:s-a"}, {c.BaseURL, "session:s-b"}} {
		resp, err := http.Post(tc.url+"/v1/messages", "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		_ = resp.Body.Close()
		if !strings.Contains(up.gotTags, tc.want) {
			t.Errorf("via %s tags were %q, want %q", tc.url, up.gotTags, tc.want)
		}
	}
	// No user configured: no end-user header and no user tag, rather than an
	// empty one that would aggregate every session under "".
	if up.gotUser != "" || strings.Contains(up.gotTags, "user:") {
		t.Errorf("empty user id leaked into attribution: user=%q tags=%q", up.gotUser, up.gotTags)
	}
}

// The request body must arrive untouched. Rewriting it to inject metadata was
// rejected precisely because it breaks streaming and puts us inside message
// content; this guards that decision.
func TestProxyDoesNotTouchTheBody(t *testing.T) {
	up := newUpstream(t, nil)
	b := newBroker(t, up, "dario")
	sp, _ := b.Open("s-1")

	body := `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"hi é 😀"}]}`
	resp, err := http.Post(sp.BaseURL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_ = resp.Body.Close()
	if up.gotBody != body {
		t.Errorf("body was modified in transit\n sent: %s\n got:  %s", body, up.gotBody)
	}
}

// Claude Code streams in interactive use. Buffering here would look like the UI
// freezing until a turn completed, so chunks must arrive as they are produced.
func TestProxyStreamsWithoutBuffering(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("upstream cannot flush")
		}
		for i := range 3 {
			fmt.Fprintf(w, "data: chunk-%d\n\n", i)
			fl.Flush()
			time.Sleep(60 * time.Millisecond)
		}
	})
	b := newBroker(t, up, "")
	sp, _ := b.Open("s-1")

	resp, err := http.Post(sp.BaseURL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	start := time.Now()
	sc := bufio.NewScanner(resp.Body)
	var firstAt time.Duration
	got := 0
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: chunk-") {
			if got == 0 {
				firstAt = time.Since(start)
			}
			got++
		}
	}
	if got != 3 {
		t.Fatalf("received %d chunks, want 3", got)
	}
	// All three take ~180ms upstream. Arriving as a batch at the end would put
	// the first chunk near that total; streaming puts it near zero.
	if firstAt > 120*time.Millisecond {
		t.Errorf("first chunk arrived after %v; the proxy is buffering the stream", firstAt)
	}
}

// An unreachable gateway must produce something Claude Code can render, not a
// bare connection error it will choke on.
func TestUnreachableGatewayReturnsAnAnthropicShapedError(t *testing.T) {
	b, err := New(Options{Upstream: "http://127.0.0.1:1", APIKey: "k", WorkspaceID: "w1"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer b.CloseAll()
	sp, _ := b.Open("s-1")

	resp, err := http.Post(sp.BaseURL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	if !strings.Contains(string(body), `"type":"error"`) {
		t.Errorf("error body is not Anthropic-shaped: %s", body)
	}
}

func TestNewRejectsBadUpstream(t *testing.T) {
	for _, bad := range []string{"", "litellm.internal:4000", "/v1/messages"} {
		if _, err := New(Options{Upstream: bad, APIKey: "k"}); err == nil {
			t.Errorf("New(%q) succeeded, want an error", bad)
		}
	}
}

func TestCloseStopsTheListener(t *testing.T) {
	up := newUpstream(t, nil)
	b := newBroker(t, up, "")
	sp, _ := b.Open("s-1")

	b.Close("s-1")
	time.Sleep(50 * time.Millisecond)
	if _, err := http.Post(sp.BaseURL+"/v1/messages", "application/json", strings.NewReader("{}")); err == nil {
		t.Error("listener still accepting after Close")
	}
}
