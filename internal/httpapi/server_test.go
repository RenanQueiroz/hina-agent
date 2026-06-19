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
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// TestChangePasswordRevokesSessions proves a session minted under the old
// (bootstrap) password is dead after the password changes, while the caller's
// reissued session keeps working. This closes the bootstrap-credential-survives
// hole behind the LAN gate.
func TestChangePasswordRevokesSessions(t *testing.T) {
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
		llm.NewMockProvider(), logbuf.New(50),
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
	postJSON(t, client, ts.URL+"/api/v1/auth/login",
		map[string]string{"username": "admin", "password": boot.Password}, nil)

	// Capture the pre-change session cookie before it gets replaced.
	u, _ := url.Parse(ts.URL)
	var oldCookie *http.Cookie
	for _, c := range jar.Cookies(u) {
		if c.Name == "hina_session" {
			oldCookie = c
		}
	}
	if oldCookie == nil {
		t.Fatal("no session cookie after login")
	}

	// Change the password; the jar transparently picks up the reissued cookie.
	postJSON(t, client, ts.URL+"/api/v1/auth/change-password",
		map[string]string{"current_password": boot.Password, "new_password": "newpassword123"}, nil)

	// The OLD cookie must now be rejected on an authenticated route.
	bare := &http.Client{}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/auth/me", nil)
	req.AddCookie(oldCookie)
	resp, err := bare.Do(req)
	if err != nil {
		t.Fatalf("me with old cookie: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old cookie after password change = %d, want 401", resp.StatusCode)
	}

	// The reissued session (in the jar) still works.
	if got := getStatus(t, client, ts.URL+"/api/v1/auth/me"); got != http.StatusOK {
		t.Fatalf("reissued session /me = %d, want 200", got)
	}
}

// TestErroredTurnReplayParity proves that on a mid-stream failure the durable
// ErrorEvent carries the same partial text as the assistant turn's canonical
// text, so a reload (which replays from events) reconstructs exactly what
// BuildContext feeds the model — no hidden, un-inspectable context.
func TestErroredTurnReplayParity(t *testing.T) {
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

	body, _ := json.Marshal(map[string]string{"text": "hi"})
	resp, err := client.Post(ts.URL+"/api/v1/conversations/"+conv.ID+"/messages",
		"application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("errored message = %d, want 502", resp.StatusCode)
	}

	// Canonical text fed to BuildContext for the failed assistant turn.
	turns, _ := st.ListTurns(ctx, conv.ID)
	var canonical string
	var sawAssistant bool
	for _, tn := range turns {
		if tn.Role == "assistant" {
			canonical = tn.CanonicalText
			sawAssistant = true
		}
	}
	if !sawAssistant {
		t.Fatal("expected a persisted (partial) assistant turn")
	}

	// The durable ErrorEvent must carry the same text, so replay matches context.
	evs, _ := st.ListEventsSince(ctx, conv.ID, 0)
	var errText string
	var foundErr bool
	for _, e := range evs {
		if e.Type == events.TypeError {
			var p struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal([]byte(e.Payload), &p); err != nil {
				t.Fatalf("decode error payload: %v", err)
			}
			errText, foundErr = p.Text, true
		}
	}
	if !foundErr {
		t.Fatal("expected a durable ErrorEvent")
	}
	if errText != canonical || canonical == "" {
		t.Fatalf("ErrorEvent text %q != canonical %q — replay would diverge from model context", errText, canonical)
	}
}

// blockingProvider streams nothing until release is closed, so a turn can be
// held in flight while the test issues a second concurrent POST.
type blockingProvider struct {
	started chan struct{}
	release chan struct{}
}

func (b *blockingProvider) Name() string { return "blocking" }

func (b *blockingProvider) Stream(ctx context.Context, _ llm.Request) (<-chan llm.Delta, error) {
	ch := make(chan llm.Delta)
	go func() {
		defer close(ch)
		close(b.started)
		select {
		case <-b.release:
		case <-ctx.Done():
			return
		}
		ch <- llm.Delta{Text: "ok"}
		ch <- llm.Delta{Done: true}
	}()
	return ch, nil
}

// TestConcurrentPostReturns409 proves turns are serialized per conversation: a
// second POST while one is in flight is rejected with 409, so interleaved turns
// can't corrupt the durable order or the model context.
func TestConcurrentPostReturns409(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	prov := &blockingProvider{started: make(chan struct{}), release: make(chan struct{})}
	srv := New(
		config.Default(), st, events.NewBus(st), auth.NewManager(st, false),
		prov, logbuf.New(50),
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

	msgURL := ts.URL + "/api/v1/conversations/" + conv.ID + "/messages"

	// POST #1 blocks in the provider, holding the conversation's turn.
	done := make(chan int, 1)
	go func() {
		b, _ := json.Marshal(map[string]string{"text": "first"})
		resp, err := client.Post(msgURL, "application/json", strings.NewReader(string(b)))
		if err != nil {
			done <- -1
			return
		}
		_ = resp.Body.Close()
		done <- resp.StatusCode
	}()

	select {
	case <-prov.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first turn never started")
	}

	// POST #2 to the same conversation, concurrent with #1, must be rejected.
	b2, _ := json.Marshal(map[string]string{"text": "second"})
	resp2, err := client.Post(msgURL, "application/json", strings.NewReader(string(b2)))
	if err != nil {
		t.Fatalf("second post: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Fatalf("concurrent POST = %d, want 409", resp2.StatusCode)
	}

	// Release #1; it completes successfully and frees the turn.
	close(prov.release)
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("first POST = %d, want 200", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first POST never completed")
	}

	// Exactly one user turn ("first") was persisted — the rejected POST added none.
	turns, _ := st.ListTurns(ctx, conv.ID)
	users := 0
	for _, tn := range turns {
		if tn.Role == "user" {
			users++
		}
	}
	if users != 1 {
		t.Fatalf("user turns = %d, want 1 (rejected POST must not persist a turn)", users)
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
