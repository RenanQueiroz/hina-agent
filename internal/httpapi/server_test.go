package httpapi

import (
	"context"
	"encoding/json"
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
