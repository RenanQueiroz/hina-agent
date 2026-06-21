package vault

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenanQueiroz/hina-agent/internal/id"
	"github.com/RenanQueiroz/hina-agent/internal/platform"
	"github.com/RenanQueiroz/hina-agent/internal/store"
)

func newTestVault(t *testing.T) (*Vault, *store.Store, string, []byte, string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "vault.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	u := store.User{ID: id.New("usr"), Username: "alice", Role: "user", PasswordHash: "x"}
	if err := st.CreateUser(ctx, u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	key := make([]byte, platform.MasterKeyLen)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	root := filepath.Join(t.TempDir(), "vaultblobs")
	v, err := New(key, root, st)
	if err != nil {
		t.Fatalf("new vault: %v", err)
	}
	return v, st, u.ID, key, root
}

func TestVaultPutGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	v, _, uid, _, root := newTestVault(t)

	meta, err := v.Put(ctx, uid, "OPENAI_API_KEY", "cloud key", "sk-supersecret-123")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if meta.Name != "OPENAI_API_KEY" || meta.ID == "" {
		t.Fatalf("meta = %+v", meta)
	}

	got, err := v.Get(ctx, uid, meta.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != "sk-supersecret-123" {
		t.Fatalf("get = %q, want the original value", got)
	}

	list, err := v.List(ctx, uid)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v err=%v", list, err)
	}

	// The encrypted blob exists on disk and does NOT contain the plaintext.
	blob, err := os.ReadFile(filepath.Join(root, uid, meta.ID+".enc"))
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if strings.Contains(string(blob), "sk-supersecret-123") {
		t.Fatal("plaintext leaked into the on-disk blob")
	}
}

func TestVaultValueNotInDatabase(t *testing.T) {
	ctx := context.Background()
	v, st, uid, _, _ := newTestVault(t)
	if _, err := v.Put(ctx, uid, "TOKEN", "", "plaintext-needle"); err != nil {
		t.Fatalf("put: %v", err)
	}
	// Dump every text column of secrets_meta and assert the value never appears.
	rows, err := st.DB().QueryContext(ctx, `SELECT id, user_id, name, description FROM secrets_meta`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var a, b, c, d string
		if err := rows.Scan(&a, &b, &c, &d); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if strings.Contains(a+b+c+d, "plaintext-needle") {
			t.Fatal("secret value found in the database metadata row")
		}
	}
}

func TestVaultDuplicateName(t *testing.T) {
	ctx := context.Background()
	v, _, uid, _, _ := newTestVault(t)
	if _, err := v.Put(ctx, uid, "DUP", "", "a"); err != nil {
		t.Fatalf("first put: %v", err)
	}
	_, err := v.Put(ctx, uid, "DUP", "", "b")
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("duplicate name err = %v, want ErrConflict", err)
	}
}

func TestVaultCrossUserIsolation(t *testing.T) {
	ctx := context.Background()
	v, st, uid, _, _ := newTestVault(t)
	other := store.User{ID: id.New("usr"), Username: "bob", Role: "user", PasswordHash: "x"}
	if err := st.CreateUser(ctx, other); err != nil {
		t.Fatalf("create other: %v", err)
	}
	meta, err := v.Put(ctx, uid, "MINE", "", "secret")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	// Bob, addressing Alice's secret id, must not be able to read it.
	if _, err := v.Get(ctx, other.ID, meta.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-user get err = %v, want ErrNotFound", err)
	}
	if err := v.Delete(ctx, other.ID, meta.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-user delete err = %v, want ErrNotFound", err)
	}
}

func TestVaultAgentStateVersionWriteUnique(t *testing.T) {
	v, _, uid, _, _ := newTestVault(t)
	_ = v.PutAgentState(uid, "codex", []byte("same-value"))
	got, v1, err := v.GetAgentStateVersioned(uid, "codex")
	if err != nil || string(got) != "same-value" || v1 == "" {
		t.Fatalf("versioned get: data=%q ver=%q err=%v", got, v1, err)
	}
	// Delete + re-add the SAME plaintext: the write-unique version MUST change (a fresh
	// envelope nonce), so a same-value re-auth during a run is detectable.
	_ = v.DeleteAgentState(uid, "codex")
	_ = v.PutAgentState(uid, "codex", []byte("same-value"))
	if v2 := v.AgentStateVersion(uid, "codex"); v2 == "" || v2 == v1 {
		t.Fatalf("write-unique version unchanged after same-value recreate: %q -> %q", v1, v2)
	}
}

func TestVaultDelete(t *testing.T) {
	ctx := context.Background()
	v, _, uid, _, root := newTestVault(t)
	meta, err := v.Put(ctx, uid, "GONE", "", "x")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := v.Delete(ctx, uid, meta.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, uid, meta.ID+".enc")); !os.IsNotExist(err) {
		t.Fatalf("blob still present after delete: %v", err)
	}
	if _, err := v.Get(ctx, uid, meta.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get after delete err = %v, want ErrNotFound", err)
	}
	if list, _ := v.List(ctx, uid); len(list) != 0 {
		t.Fatalf("list after delete = %+v", list)
	}
}

func TestVaultDeleteTolerantOfMissingBlob(t *testing.T) {
	ctx := context.Background()
	v, _, uid, _, root := newTestVault(t)
	meta, err := v.Put(ctx, uid, "X", "", "v")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	// Simulate a prior partial delete: the blob is gone but metadata remains. Delete
	// must still succeed and clear the metadata (recoverable, no orphan).
	_ = os.Remove(filepath.Join(root, uid, meta.ID+".enc"))
	if err := v.Delete(ctx, uid, meta.ID); err != nil {
		t.Fatalf("delete should tolerate a missing blob: %v", err)
	}
	if _, err := v.Get(ctx, uid, meta.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("metadata should be gone after delete: %v", err)
	}
}

func TestVaultWrongMasterKeyFails(t *testing.T) {
	ctx := context.Background()
	v, st, uid, _, root := newTestVault(t)
	meta, err := v.Put(ctx, uid, "K", "", "value")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	// A vault with a different master key over the same store + blobs must fail to
	// decrypt — the wrapped DEK can't be unwrapped.
	other := make([]byte, platform.MasterKeyLen)
	other[0] = 0xAA
	v2, err := New(other, root, st)
	if err != nil {
		t.Fatalf("new v2: %v", err)
	}
	if _, err := v2.Get(ctx, uid, meta.ID); err == nil {
		t.Fatal("decrypt with the wrong master key must fail")
	}
}

func TestVaultRejectsShortMasterKey(t *testing.T) {
	if _, err := New(make([]byte, 16), t.TempDir(), nil); err == nil {
		t.Fatal("expected error for a 16-byte master key")
	}
}

func TestMaterializeAndRedactor(t *testing.T) {
	ctx := context.Background()
	v, _, uid, _, _ := newTestVault(t)
	a, err := v.Put(ctx, uid, "API", "", "tok-AAAA")
	if err != nil {
		t.Fatalf("put a: %v", err)
	}
	b, err := v.Put(ctx, uid, "DB", "", "pw-BBBB")
	if err != nil {
		t.Fatalf("put b: %v", err)
	}
	inj, err := v.Materialize(ctx, uid, []EnvGrant{
		{SecretID: a.ID, EnvName: "API_KEY"},
		{SecretID: b.ID, EnvName: "DB_PASS"},
	})
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	pairs := inj.EnvPairs()
	want := []string{"API_KEY=tok-AAAA", "DB_PASS=pw-BBBB"}
	if strings.Join(pairs, ",") != strings.Join(want, ",") {
		t.Fatalf("env pairs = %v, want %v", pairs, want)
	}
	red := inj.Redactor().Redact("leaked tok-AAAA and pw-BBBB here")
	if strings.Contains(red, "tok-AAAA") || strings.Contains(red, "pw-BBBB") {
		t.Fatalf("redactor left a secret: %q", red)
	}
}

func TestMaterializeSkipsDeletedSecret(t *testing.T) {
	ctx := context.Background()
	v, _, uid, _, _ := newTestVault(t)
	a, _ := v.Put(ctx, uid, "LIVE", "", "live")
	b, _ := v.Put(ctx, uid, "DEAD", "", "dead")
	if err := v.Delete(ctx, uid, b.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	inj, err := v.Materialize(ctx, uid, []EnvGrant{
		{SecretID: a.ID, EnvName: "LIVE"},
		{SecretID: b.ID, EnvName: "DEAD"}, // points at the deleted secret -> skipped
	})
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if pairs := inj.EnvPairs(); len(pairs) != 1 || pairs[0] != "LIVE=live" {
		t.Fatalf("env pairs = %v, want [LIVE=live]", pairs)
	}
}

func TestMaterializeInvalidEnvName(t *testing.T) {
	ctx := context.Background()
	v, _, uid, _, _ := newTestVault(t)
	a, _ := v.Put(ctx, uid, "S", "", "v")
	if _, err := v.Materialize(ctx, uid, []EnvGrant{{SecretID: a.ID, EnvName: "1bad name"}}); err == nil {
		t.Fatal("expected error for an invalid env name")
	}
}

func TestRedactorLongestFirst(t *testing.T) {
	// A shorter value that is a prefix of a longer one must not leave a fragment.
	r := NewRedactor([]string{"abc", "abcdef"})
	if got := r.Redact("xx abcdef yy"); strings.Contains(got, "abc") {
		t.Fatalf("redact left a fragment: %q", got)
	}
}

func TestRedactorJSONTraversal(t *testing.T) {
	r := NewRedactor([]string{"topsecret/value-12345"})
	// The same secret encoded three valid ways inside JSON: canonical, escaped solidus,
	// and a \uXXXX escape of the leading char. Decoding normalizes all to the plaintext.
	cases := [][]byte{
		[]byte(`{"v":"topsecret/value-12345"}`),
		[]byte(`{"v":"topsecret\/value-12345"}`),
		[]byte(`{"v":"\u0074opsecret/value-12345"}`), // \uXXXX escape of 't'
		[]byte(`{"topsecret/value-12345":"x"}`),      // as a KEY
		[]byte(`["a","topsecret\/value-12345"]`),
	}
	for _, data := range cases {
		if !r.JSONContainsSecret(data) {
			t.Errorf("JSONContainsSecret missed an encoded secret: %s", data)
		}
		if got := r.RedactJSON(data); strings.Contains(string(got), "value-12345") {
			t.Errorf("RedactJSON left an encoded secret: %s -> %s", data, got)
		}
	}
	// Non-JSON falls back to a raw substring check / redaction.
	if !r.JSONContainsSecret([]byte("plain topsecret/value-12345 here")) {
		t.Fatal("non-JSON fallback ContainsSecret failed")
	}
	if got := r.RedactJSON([]byte("plain topsecret/value-12345 here")); strings.Contains(string(got), "value-12345") {
		t.Fatalf("non-JSON fallback redaction failed: %q", got)
	}
}

func TestRedactorJSONNumericSecret(t *testing.T) {
	r := NewRedactor([]string{"31337"})
	data := []byte(`{"const":31337,"nested":[31337],"s":"x"}`)
	if !r.JSONContainsSecret(data) {
		t.Fatal("a numeric secret embedded as a JSON number was not detected")
	}
	if got := r.RedactJSON(data); strings.Contains(string(got), "31337") {
		t.Fatalf("a numeric secret survived RedactJSON: %s", got)
	}
	// A non-secret large integer keeps its exact value (UseNumber avoids float64 loss).
	r2 := NewRedactor([]string{"99999"})
	if got := r2.RedactJSON([]byte(`{"n":12345678901234567890}`)); !strings.Contains(string(got), "12345678901234567890") {
		t.Fatalf("a non-secret number lost precision: %s", got)
	}
}

func TestRedactorNilSafe(t *testing.T) {
	var r *Redactor
	if got := r.Redact("hello"); got != "hello" {
		t.Fatalf("nil redactor changed input: %q", got)
	}
}
