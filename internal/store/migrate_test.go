package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/RenanQueiroz/hina-agent/internal/id"
)

// TestMigrateUpDownRoundTrip is the Phase 1 "migrate up/down on every CI OS"
// exit criterion: up creates the schema and round-trips a row, down all drops
// it cleanly, and up again rebuilds it. Runs on each OS via `go test ./...`.
func TestMigrateUpDownRoundTrip(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Up: applies at least one migration and the schema works.
	n, err := st.Migrate(ctx)
	if err != nil || n < 1 {
		t.Fatalf("migrate up = %d, err %v", n, err)
	}
	if err := st.CreateUser(ctx, User{ID: id.New("usr"), Username: "u", Role: "user", PasswordHash: "x"}); err != nil {
		t.Fatalf("insert after up: %v", err)
	}

	// Down all: the schema is gone (users no longer queryable) and no versions
	// remain recorded.
	reverted, err := st.MigrateDown(ctx, 0)
	if err != nil || reverted < 1 {
		t.Fatalf("migrate down = %d, err %v", reverted, err)
	}
	if _, err := st.CountByRole(ctx, "admin"); err == nil {
		t.Fatal("users table should not exist after down all")
	}
	var remaining int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM schema_migrations`).Scan(&remaining); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("schema_migrations rows after down all = %d, want 0", remaining)
	}

	// Up again: schema rebuilds and is usable.
	n2, err := st.Migrate(ctx)
	if err != nil || n2 != n {
		t.Fatalf("re-migrate up = %d, err %v (want %d)", n2, err, n)
	}
	if c, err := st.CountByRole(ctx, "admin"); err != nil || c != 0 {
		t.Fatalf("after re-up: count=%d err=%v", c, err)
	}
}

// TestMigrateDownSteps proves a bounded rollback reverts only the requested
// number of migrations: stepping down one at a time reverts exactly one each
// time, and once every applied migration is rolled back nothing remains. Written
// to survive new migrations being added (no hard-coded migration count).
func TestMigrateDownSteps(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	applied, err := st.Migrate(ctx)
	if err != nil || applied < 1 {
		t.Fatalf("migrate up = %d, err %v", applied, err)
	}
	for i := 0; i < applied; i++ {
		reverted, err := st.MigrateDown(ctx, 1)
		if err != nil || reverted != 1 {
			t.Fatalf("migrate down 1 (step %d) = %d, err %v", i, reverted, err)
		}
	}
	// Idempotent: nothing left to revert.
	if again, err := st.MigrateDown(ctx, 1); err != nil || again != 0 {
		t.Fatalf("down after full rollback = %d, err %v (want 0)", again, err)
	}
}
