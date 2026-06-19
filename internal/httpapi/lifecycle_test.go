package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/auth"
	"github.com/RenanQueiroz/hina-agent/internal/config"
	"github.com/RenanQueiroz/hina-agent/internal/events"
	"github.com/RenanQueiroz/hina-agent/internal/llm"
	"github.com/RenanQueiroz/hina-agent/internal/logbuf"
	"github.com/RenanQueiroz/hina-agent/internal/store"
)

// TestServerListenReadyAndShutdown exercises the real server lifecycle over a
// live TCP socket on every OS in the CI matrix (it runs under `go test ./...`):
// serve, observe /readyz=200, then shut down gracefully and confirm Serve
// returns. This is the portable form of the Phase 1 "start the server, hit
// /readyz, shut it down cleanly" exit criterion; process-tree/orphan cleanup is
// covered separately by the platform KillTree tests on each OS.
func TestServerListenReadyAndShutdown(t *testing.T) {
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

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	httpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(ln) }()

	base := "http://" + ln.Addr().String()
	ready := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/readyz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				ready = true
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		_ = httpSrv.Close()
		t.Fatal("server never reported /readyz=200")
	}

	sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(sc); err != nil {
		t.Fatalf("graceful shutdown: %v", err)
	}
	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("serve returned %v, want ErrServerClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after graceful Shutdown")
	}
}
