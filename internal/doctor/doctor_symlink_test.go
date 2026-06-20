//go:build !windows

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

// doctor must not dlopen an ORT library that is a SYMLINK (a link's target could be
// swapped after the checksum), so a symlinked library is reported unavailable.
func TestDoctorRejectsSymlinkedORT(t *testing.T) {
	a, ok := assets.ORTAsset(runtime.GOOS, runtime.GOARCH)
	if !ok {
		t.Skipf("no ORT asset for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	tmp := t.TempDir()
	root := filepath.Join(tmp, "assets")
	target := filepath.Join(tmp, "target.so")
	if err := os.WriteFile(target, bytes.Repeat([]byte("x"), int(a.MemberSize)), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, a.Dest)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, dest); err != nil {
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
		t.Fatalf("onnx runtime status = %q, want unavailable for a symlinked lib", c.Status)
	}
	if !strings.Contains(c.Detail, "symlink") {
		t.Fatalf("expected a symlink-rejection reason, got %q", c.Detail)
	}
}
