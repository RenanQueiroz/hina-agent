package httpapi

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/RenanQueiroz/hina-agent/internal/auth"
	"github.com/RenanQueiroz/hina-agent/internal/config"
	"github.com/RenanQueiroz/hina-agent/internal/events"
	"github.com/RenanQueiroz/hina-agent/internal/llm"
	"github.com/RenanQueiroz/hina-agent/internal/logbuf"
	"github.com/RenanQueiroz/hina-agent/internal/store"
)

// TestDeliverEventRecoversGap proves the SSE consumer never silently loses a
// persisted event when its buffer overflows: receiving seq=4 after only seq=1
// was delivered triggers a store replay of the missing 2 and 3, in order, with
// no duplicate of 4.
func TestDeliverEventRecoversGap(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// A conversation owner + conversation so the events FK is satisfied.
	if err := st.CreateUser(ctx, store.User{ID: "usr_1", Username: "u", Role: "user", PasswordHash: "x"}); err != nil {
		t.Fatalf("user: %v", err)
	}
	convID := "cnv_1"
	if err := st.CreateConversation(ctx, store.Conversation{ID: convID, OwnerUserID: "usr_1"}); err != nil {
		t.Fatalf("conversation: %v", err)
	}

	bus := events.NewBus(st)
	srv := New(
		config.Default(), st, bus, auth.NewManager(st, false),
		llm.NewMockProvider(), logbuf.New(50),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	// Persist four events (seq 1..4) so a replay has something to recover.
	for i := 0; i < 4; i++ {
		e, err := events.New(events.SourceServer, events.TypeTurnStarted, convID, "", "", nil)
		if err != nil {
			t.Fatalf("new event: %v", err)
		}
		if _, err := bus.Publish(ctx, e); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	rec := httptest.NewRecorder()
	var lastSeq int64

	// Simulate the consumer receiving seq=1 normally, then jumping to seq=4
	// because seq 2 and 3 were dropped from a full buffer.
	first, _ := events.New(events.SourceServer, events.TypeTurnStarted, convID, "", "", nil)
	first.Seq = 1
	lastSeq = srv.deliverEvent(ctx, rec, convID, first, lastSeq)
	if lastSeq != 1 {
		t.Fatalf("lastSeq after first = %d, want 1", lastSeq)
	}

	gap, _ := events.New(events.SourceServer, events.TypeTurnStarted, convID, "", "", nil)
	gap.Seq = 4
	lastSeq = srv.deliverEvent(ctx, rec, convID, gap, lastSeq)
	if lastSeq != 4 {
		t.Fatalf("lastSeq after gap = %d, want 4", lastSeq)
	}

	got := sseSeqs(t, rec.Body.String())
	want := []int64{1, 2, 3, 4}
	if len(got) != len(want) {
		t.Fatalf("delivered seqs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delivered seqs = %v, want %v", got, want)
		}
	}
}

// sseSeqs extracts the ordered list of seq values from an SSE body's `id:` lines.
func sseSeqs(t *testing.T, body string) []int64 {
	t.Helper()
	var out []int64
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "id:") {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "id:")), 10, 64)
		if err != nil {
			t.Fatalf("parse id line %q: %v", line, err)
		}
		out = append(out, n)
	}
	return out
}
