package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/RenanQueiroz/hina-agent/internal/id"
)

func agentTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "agents.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	u := User{ID: id.New("usr"), Username: "alice", Role: "user", PasswordHash: "x"}
	if err := st.CreateUser(ctx, u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return st, u.ID
}

func TestAgentProfileUpsertGetDelete(t *testing.T) {
	ctx := context.Background()
	st, uid := agentTestStore(t)

	if _, err := st.GetAgentProfile(ctx, uid, "codex"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	p := AgentProfile{ID: id.New("agp"), UserID: uid, Provider: "codex", AuthType: "browser_state", Status: "authenticated", Label: "configured"}
	if err := st.UpsertAgentProfile(ctx, p); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := st.GetAgentProfile(ctx, uid, "codex")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AuthType != "browser_state" || got.Status != "authenticated" || got.CreatedAt.IsZero() {
		t.Fatalf("profile = %+v", got)
	}

	// Upsert again with a new id replaces (one profile per provider) and keeps a row.
	p2 := AgentProfile{ID: id.New("agp"), UserID: uid, Provider: "codex", AuthType: "api_key", Status: "authenticated"}
	if err := st.UpsertAgentProfile(ctx, p2); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	got2, _ := st.GetAgentProfile(ctx, uid, "codex")
	if got2.AuthType != "api_key" {
		t.Fatalf("upsert did not update auth_type: %+v", got2)
	}
	if got2.ID != got.ID {
		t.Fatalf("upsert should keep the original row id, got %s want %s", got2.ID, got.ID)
	}

	list, err := st.ListAgentProfilesByUser(ctx, uid)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v (err %v), want 1", list, err)
	}

	if err := st.DeleteAgentProfile(ctx, uid, "codex"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := st.DeleteAgentProfile(ctx, uid, "codex"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete should be ErrNotFound, got %v", err)
	}
}

func TestAgentProfileScopedToUser(t *testing.T) {
	ctx := context.Background()
	st, uid := agentTestStore(t)
	other := User{ID: id.New("usr"), Username: "bob", Role: "user", PasswordHash: "x"}
	if err := st.CreateUser(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAgentProfile(ctx, AgentProfile{ID: id.New("agp"), UserID: uid, Provider: "claude", AuthType: "api_key"}); err != nil {
		t.Fatal(err)
	}
	// Bob can't see Alice's profile.
	if _, err := st.GetAgentProfile(ctx, other.ID, "claude"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user read should be ErrNotFound, got %v", err)
	}
	all, err := st.ListAllAgentProfiles(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("ListAll = %v (err %v)", all, err)
	}
}
