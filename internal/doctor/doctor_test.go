package doctor_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/RenanQueiroz/hina-agent/internal/assets"
	"github.com/RenanQueiroz/hina-agent/internal/config"
	"github.com/RenanQueiroz/hina-agent/internal/doctor"
	"github.com/RenanQueiroz/hina-agent/internal/platform"
)

func check(t *testing.T, rep doctor.Report, name string) doctor.Check {
	t.Helper()
	for _, c := range rep.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %q check in report; have %+v", name, rep.Checks)
	return doctor.Check{}
}

// doctor must verify the ORT library's checksum BEFORE constructing the backend,
// so a stale/corrupted lib (size matches, checksum doesn't) is reported as
// unavailable rather than dlopen'd. (Verification is pure Go, so this holds in
// the default build too.)
func TestDoctorVerifiesORTBeforeLoading(t *testing.T) {
	a, ok := assets.ORTAsset(runtime.GOOS, runtime.GOARCH)
	if !ok {
		t.Skipf("no ORT asset for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	tmp := t.TempDir()
	root := filepath.Join(tmp, "assets")
	dest := filepath.Join(root, a.Dest)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	// Right size, wrong bytes -> checksum mismatch.
	if err := os.WriteFile(dest, bytes.Repeat([]byte("x"), int(a.MemberSize)), 0o644); err != nil {
		t.Fatal(err)
	}

	paths := platform.Paths{
		Config: filepath.Join(tmp, "config"), Cache: filepath.Join(tmp, "cache"),
		Data: filepath.Join(tmp, "data"), Runtime: filepath.Join(tmp, "run"), Log: filepath.Join(tmp, "log"),
	}
	cfg := config.Default()
	cfg.TTS.Enabled = true
	cfg.TTS.AssetsDir = root

	rep := doctor.Run(context.Background(), cfg, paths)
	c := check(t, rep, "onnx runtime")
	if c.Status != "unavailable" {
		t.Fatalf("onnx runtime status = %q, want unavailable", c.Status)
	}
	if !strings.Contains(c.Detail, "checksum") && !strings.Contains(c.Detail, "size") {
		t.Fatalf("expected a verification reason, got %q", c.Detail)
	}
}
