//go:build onnx

package onnx

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestResolveLibPathPrecedence locks the resolution contract: an exact LibFile is
// used verbatim; under a lib dir the manifest's lib/ subdir is searched FIRST (so
// an unverified same-named file placed directly under the dir can't be selected
// ahead of the verified one); a configured-but-empty lib dir fails closed; and an
// empty lib dir uses the env fallback.
func TestResolveLibPathPrecedence(t *testing.T) {
	dir := t.TempDir()
	libDirLib := filepath.Join(dir, "lib")
	if err := os.MkdirAll(libDirLib, 0o755); err != nil {
		t.Fatal(err)
	}
	name := libCandidates()[0]
	verified := filepath.Join(libDirLib, name) // the manifest install location
	if err := os.WriteFile(verified, []byte("verified"), 0o644); err != nil {
		t.Fatal(err)
	}
	// An UNVERIFIED same-named file directly under the lib dir must NOT win.
	parent := filepath.Join(dir, name)
	if err := os.WriteFile(parent, []byte("unverified"), 0o644); err != nil {
		t.Fatal(err)
	}
	envLib := filepath.Join(dir, "env-other-lib")
	if err := os.WriteFile(envLib, []byte("env"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ONNXRUNTIME_SHARED_LIBRARY_PATH", envLib)

	// Exact LibFile -> used verbatim, ignores everything else.
	if got := resolveLibPath(verified, dir); got != verified {
		t.Fatalf("resolveLibPath(libFile) = %q, want %q", got, verified)
	}
	// LibDir set -> lib/ searched first, so the verified file wins over the parent.
	if got := resolveLibPath("", dir); got != verified {
		t.Fatalf("resolveLibPath(dir) = %q, want the verified lib/ file %q (not the parent)", got, verified)
	}
	// LibDir set but no lib anywhere -> fail closed (no fallback to env).
	if got := resolveLibPath("", filepath.Join(dir, "nope")); got != "" {
		t.Fatalf("resolveLibPath(missing dir) = %q, want \"\" (fail closed)", got)
	}
	// Empty LibFile + LibDir -> env fallback.
	if got := resolveLibPath("", ""); got != envLib {
		t.Fatalf("resolveLibPath(empty) = %q, want env %q", got, envLib)
	}
}

// TestORTLoadsAndRunsModel is Phase 4 exit criterion #1: with `-tags onnx` and an
// installed ONNX Runtime 1.26.0 shared library, the Backend loads the runtime
// from the app-managed library path and runs a trivial ONNX model end to end
// through our Tensor/Session abstraction. It validates input marshalling
// (float32 [2,10]), the dynamic output shape ([2]), and exact numerical results
// (each row summed: rows 0..9 and 10..19 -> 45 and 145).
//
// It SKIPS only when no ORT library was requested at all
// (ONNXRUNTIME_SHARED_LIBRARY_PATH unset), so the onnx-tagged build still passes
// on a host without the runtime. When the env var IS set (as CI does), an
// unavailable runtime is a FAILURE, not a skip — otherwise a bad path, wrong
// architecture, version mismatch, or init regression would silently turn this
// into a no-op while the job stays green.
func TestORTLoadsAndRunsModel(t *testing.T) {
	b, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer b.Close()
	if !b.Info().Available {
		if os.Getenv("ONNXRUNTIME_SHARED_LIBRARY_PATH") != "" {
			t.Fatalf("ONNXRUNTIME_SHARED_LIBRARY_PATH is set but the runtime is unavailable: %s", b.Info().Reason)
		}
		t.Skipf("no ONNX Runtime library provided: %s", b.Info().Reason)
	}
	if b.Info().Version == "" || b.Info().LibPath == "" {
		t.Fatalf("available backend missing version/lib path: %+v", b.Info())
	}
	t.Logf("ORT %s (%s) from %s", b.Info().Version, b.Info().Provider, b.Info().LibPath)

	sess, err := b.Open("testdata/example_dynamic_axes.onnx",
		[]string{"input_vectors"}, []string{"output_scalars"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sess.Close()

	in := make([]float32, 20)
	for i := range in {
		in[i] = float32(i)
	}
	out, err := sess.Run(context.Background(), map[string]Tensor{
		"input_vectors": NewFloat32([]int64{2, 10}, in),
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got, ok := out["output_scalars"]
	if !ok {
		t.Fatalf("missing output, got keys %v", keys(out))
	}
	if len(got.Shape) != 1 || got.Shape[0] != 2 {
		t.Fatalf("output shape = %v, want [2]", got.Shape)
	}
	want := []float32{45, 145}
	if len(got.Float32) != len(want) || got.Float32[0] != want[0] || got.Float32[1] != want[1] {
		t.Fatalf("output = %v, want %v", got.Float32, want)
	}

	// A missing required input is a clean error, not a panic.
	if _, err := sess.Run(context.Background(), map[string]Tensor{}); err == nil {
		t.Fatal("expected error running with a missing input")
	}

	// A pre-cancelled context is honored before the run dispatches.
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sess.Run(cctx, map[string]Tensor{"input_vectors": NewFloat32([]int64{2, 10}, in)}); err == nil {
		t.Fatal("expected error running with a cancelled context")
	}
}

func keys(m map[string]Tensor) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
