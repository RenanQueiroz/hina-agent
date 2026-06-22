package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/RenanQueiroz/hina-agent/internal/auth"
	"github.com/RenanQueiroz/hina-agent/internal/automation"
	"github.com/RenanQueiroz/hina-agent/internal/autorun"
	"github.com/RenanQueiroz/hina-agent/internal/config"
	"github.com/RenanQueiroz/hina-agent/internal/events"
	"github.com/RenanQueiroz/hina-agent/internal/llm"
	"github.com/RenanQueiroz/hina-agent/internal/logbuf"
	"github.com/RenanQueiroz/hina-agent/internal/store"
	"github.com/RenanQueiroz/hina-agent/internal/wire"
)

// automationTestServer builds a server with an installed automation service backed by
// a real store + mock provider. The scheduler is NOT started (these tests exercise the
// HTTP CRUD/validation glue, not run execution — that's covered in internal/autorun).
func automationTestServer(t *testing.T) (*httptest.Server, *http.Client) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cfg := config.Default()
	cfg.Automations.Enabled = true
	srv := New(cfg, st, events.NewBus(st), auth.NewManager(st, false),
		llm.NewMockProvider(), logbuf.New(50), slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc := autorun.New(autorun.ServiceConfig{
		Store:       st,
		Exec:        autorun.ExecConfig{Provider: llm.NewMockProvider(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))},
		ArtifactDir: t.TempDir(),
		// A permissive eligibility func so an enable can succeed in this glue test.
		Eligibility: func(context.Context, string) (automation.Eligibility, error) {
			return automation.Eligibility{SandboxAvailable: true}, nil
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	srv.SetAutomations(svc)
	srv.SetReady(true)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	boot, err := auth.EnsureAdmin(ctx, st)
	if err != nil || !boot.Created {
		t.Fatalf("bootstrap admin: %v", err)
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	postJSON(t, client, ts.URL+"/api/v1/auth/login",
		map[string]string{"username": "admin", "password": boot.Password}, nil)
	return ts, client
}

func manualDefJSON() json.RawMessage {
	return json.RawMessage(`{"schema_version":"automation.v1","name":"My automation","trigger":{"type":"manual"},
		"sandbox":{"mode":"unrestricted"},
		"steps":[{"id":"s","type":"finish","status":"success","message":"done"}]}`)
}

func TestAutomationCRUDOverHTTP(t *testing.T) {
	ts, c := automationTestServer(t)

	// Create.
	var created wire.AutomationDetail
	postJSON(t, c, ts.URL+"/api/v1/automations", wire.AutomationInput{Definition: manualDefJSON()}, &created)
	if created.ID == "" || created.Enabled {
		t.Fatalf("create = %+v (must be disabled)", created)
	}
	if created.Trigger != "manual" {
		t.Errorf("trigger = %q", created.Trigger)
	}

	// List.
	var list wire.AutomationList
	getInto(t, c, ts.URL+"/api/v1/automations", &list)
	if len(list.Automations) != 1 {
		t.Fatalf("list = %d, want 1", len(list.Automations))
	}

	// Get.
	var got wire.AutomationDetail
	getInto(t, c, ts.URL+"/api/v1/automations/"+created.ID, &got)
	if string(got.Definition) == "" {
		t.Fatal("detail should include the definition")
	}

	// Enable.
	var enabled wire.AutomationDetail
	postJSON(t, c, ts.URL+"/api/v1/automations/"+created.ID+"/enabled", wire.SetAutomationEnabled{Enabled: true}, &enabled)
	if !enabled.Enabled {
		t.Fatal("automation should be enabled")
	}

	// Delete.
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/automations/"+created.ID, nil)
	resp, err := c.Do(req)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %v %d", err, resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestAutomationValidateEndpoint(t *testing.T) {
	ts, c := automationTestServer(t)
	bad := json.RawMessage(`{"schema_version":"automation.v1","name":"","trigger":{"type":"interval","every":"x"},"steps":[]}`)
	var res wire.AutomationValidation
	postJSON(t, c, ts.URL+"/api/v1/automations/validate", wire.AutomationInput{Definition: bad}, &res)
	if res.Valid {
		t.Fatal("a malformed definition must be invalid")
	}
	if len(res.Issues) == 0 {
		t.Fatal("validation issues should be returned for repair")
	}
}

func TestAutomationCreateRejectsInvalid(t *testing.T) {
	ts, c := automationTestServer(t)
	bad := json.RawMessage(`{"schema_version":"automation.v1","name":"x","trigger":{"type":"manual"},"steps":[]}`)
	b, _ := json.Marshal(wire.AutomationInput{Definition: bad})
	resp, err := c.Post(ts.URL+"/api/v1/automations", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	var v wire.AutomationValidation
	_ = json.NewDecoder(resp.Body).Decode(&v)
	if v.Valid || len(v.Issues) == 0 {
		t.Fatalf("expected issues, got %+v", v)
	}
}

func TestAutomationMetaEndpoint(t *testing.T) {
	ts, c := automationTestServer(t)
	var meta wire.AutomationMeta
	getInto(t, c, ts.URL+"/api/v1/automations/meta", &meta)
	if meta.SchemaVersion != automation.SchemaVersion {
		t.Errorf("schema_version = %q", meta.SchemaVersion)
	}
	if len(meta.Tools) == 0 || len(meta.Adapters) == 0 {
		t.Fatal("meta should list tools + adapters for the builder")
	}
}

func TestAutomationOwnerScoping(t *testing.T) {
	ts, c := automationTestServer(t)
	var created wire.AutomationDetail
	postJSON(t, c, ts.URL+"/api/v1/automations", wire.AutomationInput{Definition: manualDefJSON()}, &created)
	// A second user (fresh cookie jar) must not see another user's automation.
	jar2, _ := cookiejar.New(nil)
	c2 := &http.Client{Jar: jar2}
	// There is only one account (admin); a missing-session request is unauthorized.
	resp, err := c2.Get(ts.URL + "/api/v1/automations/" + created.ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("an unauthenticated client must not read an automation")
	}
}
