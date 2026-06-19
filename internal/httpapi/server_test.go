package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenanQueiroz/hina-agent/internal/auth"
	"github.com/RenanQueiroz/hina-agent/internal/config"
	"github.com/RenanQueiroz/hina-agent/internal/events"
	"github.com/RenanQueiroz/hina-agent/internal/llm"
	"github.com/RenanQueiroz/hina-agent/internal/logbuf"
	"github.com/RenanQueiroz/hina-agent/internal/store"
)

// TestServerIntegration runs migrations, the readiness check, the auth/session
// flow, a streamed (mock) message turn, persistence, and the CSRF guard against
// the real handler. It runs in CI on every OS in the matrix via `go test`, which
// is the portable cross-platform server smoke.
func TestServerIntegration(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	srv := New(
		config.Default(), st, events.NewBus(st), auth.NewManager(st, false),
		llm.NewMockProvider(), logbuf.New(100),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	srv.SetReady(true)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	boot, err := auth.EnsureAdmin(ctx, st)
	if err != nil || !boot.Created {
		t.Fatalf("bootstrap admin: %v (%+v)", err, boot)
	}

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	// readiness
	if got := getStatus(t, client, ts.URL+"/readyz"); got != http.StatusOK {
		t.Fatalf("/readyz = %d", got)
	}

	// login (no Origin header -> treated as a non-browser client, allowed)
	postJSON(t, client, ts.URL+"/api/v1/auth/login",
		map[string]string{"username": "admin", "password": boot.Password}, nil)

	// create a conversation
	var conv struct {
		ID string `json:"id"`
	}
	postJSONInto(t, client, ts.URL+"/api/v1/conversations", map[string]string{"title": "t"}, &conv)
	if conv.ID == "" {
		t.Fatal("no conversation id")
	}

	// post a message; the mock provider streams an echoed reply
	var msg struct {
		Text string `json:"text"`
	}
	postJSONInto(t, client, ts.URL+"/api/v1/conversations/"+conv.ID+"/messages",
		map[string]string{"text": "hi"}, &msg)
	if !strings.Contains(msg.Text, "You said: hi") {
		t.Fatalf("assistant reply = %q", msg.Text)
	}

	// turns persisted (user + assistant)
	var turns struct {
		Turns []struct {
			Role string `json:"role"`
			Text string `json:"text"`
		} `json:"turns"`
	}
	getInto(t, client, ts.URL+"/api/v1/conversations/"+conv.ID+"/turns", &turns)
	if len(turns.Turns) != 2 {
		t.Fatalf("turns = %d, want 2", len(turns.Turns))
	}

	// CSRF: a cross-origin POST riding the cookie is rejected.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/conversations", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://evil.example")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("csrf request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin POST = %d, want 403", resp.StatusCode)
	}
}

// errAfterDeltaProvider streams one delta then fails, simulating a real
// mid-stream backend error (not a client interrupt).
type errAfterDeltaProvider struct{}

func (errAfterDeltaProvider) Name() string { return "err" }

func (errAfterDeltaProvider) Stream(_ context.Context, _ llm.Request) (<-chan llm.Delta, error) {
	ch := make(chan llm.Delta)
	go func() {
		defer close(ch)
		ch <- llm.Delta{Text: "partial "}
		ch <- llm.Delta{Err: errors.New("backend boom")}
	}()
	return ch, nil
}

func TestStreamErrorReturns502(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	srv := New(
		config.Default(), st, events.NewBus(st), auth.NewManager(st, false),
		errAfterDeltaProvider{}, logbuf.New(50),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	srv.SetReady(true)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	boot, _ := auth.EnsureAdmin(ctx, st)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	postJSON(t, client, ts.URL+"/api/v1/auth/login",
		map[string]string{"username": "admin", "password": boot.Password}, nil)
	var conv struct {
		ID string `json:"id"`
	}
	postJSON(t, client, ts.URL+"/api/v1/conversations", map[string]string{}, &conv)

	// The message stream errors mid-way -> 502, not a 200 "completed" turn.
	body, _ := json.Marshal(map[string]string{"text": "hi"})
	resp, err := client.Post(ts.URL+"/api/v1/conversations/"+conv.ID+"/messages",
		"application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("message on stream error = %d, want 502", resp.StatusCode)
	}

	evs, err := st.ListEventsSince(ctx, conv.ID, 0)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var hasError, hasCompleted bool
	for _, e := range evs {
		switch e.Type {
		case events.TypeError:
			hasError = true
		case events.TypeAgentTextCompleted:
			hasCompleted = true
		}
	}
	if !hasError {
		t.Error("expected an ErrorEvent")
	}
	if hasCompleted {
		t.Error("must not emit AgentTextCompleted on a stream error")
	}
}

func TestSameOrigin(t *testing.T) {
	mk := func(origin, host string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "http://"+host+"/api/v1/x", nil)
		r.Host = host
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		return r
	}
	cases := []struct {
		origin, host string
		want         bool
	}{
		{"http://127.0.0.1:5173", "127.0.0.1:5173", true}, // dev proxy (Host preserved)
		{"http://hina.example", "hina.example", true},     // prod same-origin
		{"http://evil.example", "hina.example", false},    // cross-site forged POST
		{"", "hina.example", true},                        // non-browser client (no cookie CSRF)
	}
	for _, c := range cases {
		if got := sameOrigin(mk(c.origin, c.host)); got != c.want {
			t.Errorf("sameOrigin(origin=%q host=%q) = %v, want %v", c.origin, c.host, got, c.want)
		}
	}
}

func getStatus(t *testing.T, c *http.Client, url string) int {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func getInto(t *testing.T, c *http.Client, url string, v any) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

func postJSON(t *testing.T, c *http.Client, url string, body any, into any) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := c.Post(url, "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.Fatalf("POST %s = %d", url, resp.StatusCode)
	}
	if into != nil {
		if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
			t.Fatalf("decode %s: %v", url, err)
		}
	}
}

func postJSONInto(t *testing.T, c *http.Client, url string, body, into any) {
	t.Helper()
	postJSON(t, c, url, body, into)
}
