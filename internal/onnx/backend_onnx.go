//go:build onnx

// This file is compiled only with `-tags onnx` (and CGO_ENABLED=1). It links the
// real ONNX Runtime through the github.com/yalue/onnxruntime_go binding, which is
// CGo and dlopen's the ORT shared library at runtime via SetSharedLibraryPath —
// so building this tag needs a C compiler but NOT the ORT library itself (that is
// loaded at run time from the app-managed runtime dir). The binding pinned at
// v1.31.0 speaks ORT C API v26 and must be paired with an ORT 1.26.0 shared lib.

package onnx

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
)

// expectedORTPrefix pins the ONNX Runtime version the yalue v1.31.0 binding's C
// API (v26) requires. A loaded library outside 1.26.x is rejected (fail closed)
// rather than risking an ABI mismatch.
const expectedORTPrefix = "1.26."

// ORT's environment is process-global. ensureEnv initializes it exactly once for
// the whole process (TTS and, later, ASR share one env) and records the result.
var (
	initMu   sync.Mutex
	initDone bool
	initErr  error
)

func ensureEnv(libPath string) error {
	initMu.Lock()
	defer initMu.Unlock()
	if initDone {
		return initErr
	}
	initDone = true
	if libPath != "" {
		ort.SetSharedLibraryPath(libPath)
	}
	initErr = ort.InitializeEnvironment()
	return initErr
}

// libCandidates lists the shared-library filenames to look for under the runtime
// dir, most-specific first. The path passed to the binding is dlopen'd directly,
// so the exact filename only has to exist on disk.
func libCandidates() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{"libonnxruntime.1.26.0.dylib", "libonnxruntime.dylib", "onnxruntime.dylib"}
	case "windows":
		return []string{"onnxruntime.dll"}
	default: // linux and other unix
		return []string{"libonnxruntime.so.1.26.0", "libonnxruntime.so", "onnxruntime.so"}
	}
}

// resolveLibPath finds the ORT shared library. An exact libFile (a caller-
// verified path) is used verbatim. Otherwise the app-managed lib dir takes
// precedence over the env override: the manifest installs the library under
// libDir/lib, which is searched FIRST so an unverified same-named file placed
// directly under libDir can't be selected ahead of the verified one. A
// configured-but-missing lib dir fails closed (returns "") rather than silently
// falling back to a possibly-unverified env library; only an EMPTY lib dir uses
// ONNXRUNTIME_SHARED_LIBRARY_PATH (tests / ad-hoc).
func resolveLibPath(libFile, libDir string) string {
	if libFile != "" {
		if fileExists(libFile) {
			return libFile
		}
		return ""
	}
	if libDir != "" {
		for _, d := range []string{filepath.Join(libDir, "lib"), libDir} {
			for _, name := range libCandidates() {
				p := filepath.Join(d, name)
				if fileExists(p) {
					return p
				}
			}
		}
		return ""
	}
	if env := os.Getenv("ONNXRUNTIME_SHARED_LIBRARY_PATH"); env != "" && fileExists(env) {
		return env
	}
	return ""
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// New resolves + initializes the ORT runtime. A missing shared library is not a
// hard error: it returns an unavailable backend (with a Reason) so `hina doctor`
// and the admin UI report it cleanly and the server runs with TTS disabled.
func New(cfg Config) (Backend, error) {
	libPath := resolveLibPath(cfg.LibFile, cfg.LibDir)
	if libPath == "" {
		return ortBackend{info: Info{
			Available: false,
			Reason:    "ONNX Runtime 1.26.0 shared library not found (looked in the runtime dir and ONNXRUNTIME_SHARED_LIBRARY_PATH)",
		}}, nil
	}
	if err := ensureEnv(libPath); err != nil {
		return ortBackend{info: Info{
			Available: false,
			LibPath:   libPath,
			Reason:    "failed to initialize ONNX Runtime: " + err.Error(),
		}}, nil
	}
	// Fail closed on a version the binding's ABI doesn't match (e.g. a stray env
	// override pointing at a different ORT), even though the lib loaded.
	ver := ort.GetVersion()
	if !strings.HasPrefix(ver, expectedORTPrefix) {
		return ortBackend{info: Info{
			Available: false,
			Version:   ver,
			LibPath:   libPath,
			Reason:    fmt.Sprintf("loaded ONNX Runtime %q is not the required %sx (yalue v1.31.0 needs ORT C API v26)", ver, expectedORTPrefix),
		}}, nil
	}
	return ortBackend{
		info:    Info{Available: true, Version: ver, Provider: "CPU", LibPath: libPath},
		threads: cfg.IntraOpThreads,
	}, nil
}

type ortBackend struct {
	info    Info
	threads int
}

func (b ortBackend) Info() Info { return b.info }

func (b ortBackend) Close() error { return nil } // the global ORT env outlives any one backend

func (b ortBackend) Open(modelPath string, inputNames, outputNames []string) (Session, error) {
	return b.open(modelPath, nil, inputNames, outputNames)
}

func (b ortBackend) OpenBytes(model []byte, inputNames, outputNames []string) (Session, error) {
	return b.open("", model, inputNames, outputNames)
}

// open builds a dynamic session from either a file path or an in-memory model
// (exactly one of path/data is set).
func (b ortBackend) open(path string, data []byte, inputNames, outputNames []string) (Session, error) {
	if !b.info.Available {
		return nil, ErrUnavailable
	}
	var opts *ort.SessionOptions
	if b.threads > 0 {
		o, err := ort.NewSessionOptions()
		if err != nil {
			return nil, fmt.Errorf("onnx: session options: %w", err)
		}
		if err := o.SetIntraOpNumThreads(b.threads); err != nil {
			_ = o.Destroy()
			return nil, fmt.Errorf("onnx: set threads: %w", err)
		}
		opts = o
	}
	var (
		sess *ort.DynamicAdvancedSession
		err  error
	)
	if data != nil {
		sess, err = ort.NewDynamicAdvancedSessionWithONNXData(data, inputNames, outputNames, opts)
	} else {
		sess, err = ort.NewDynamicAdvancedSession(path, inputNames, outputNames, opts)
	}
	if opts != nil {
		_ = opts.Destroy() // options are copied into the session; safe to free now
	}
	if err != nil {
		if path != "" {
			return nil, fmt.Errorf("onnx: open %s: %w", path, err)
		}
		return nil, fmt.Errorf("onnx: open from bytes: %w", err)
	}
	return &ortSession{
		sess:     sess,
		inNames:  append([]string(nil), inputNames...),
		outNames: append([]string(nil), outputNames...),
	}, nil
}

type ortSession struct {
	mu       sync.Mutex // ORT dynamic sessions are not safe for concurrent Run
	sess     *ort.DynamicAdvancedSession
	inNames  []string
	outNames []string
}

func (s *ortSession) Run(ctx context.Context, inputs map[string]Tensor) (map[string]Tensor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	inVals := make([]ort.Value, 0, len(s.inNames))
	defer func() {
		for _, v := range inVals {
			if v != nil {
				_ = v.Destroy()
			}
		}
	}()
	for _, name := range s.inNames {
		t, ok := inputs[name]
		if !ok {
			return nil, fmt.Errorf("onnx: missing input %q", name)
		}
		v, err := toOrtValue(t)
		if err != nil {
			return nil, fmt.Errorf("onnx: input %q: %w", name, err)
		}
		inVals = append(inVals, v)
	}

	outVals := make([]ort.Value, len(s.outNames)) // nil entries -> ORT auto-allocates
	defer func() {
		for _, v := range outVals {
			if v != nil {
				_ = v.Destroy()
			}
		}
	}()

	// Context-aware run: a watchdog Terminates the in-flight ORT call when ctx is
	// cancelled, so a barge-in / supersede aborts an expensive vector-estimator or
	// vocoder pass mid-flight instead of running to completion while holding the
	// serialized session.
	runOpts, err := ort.NewRunOptions()
	if err != nil {
		return nil, fmt.Errorf("onnx: run options: %w", err)
	}
	defer runOpts.Destroy()
	stop := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-ctx.Done():
			_ = runOpts.Terminate()
		case <-stop:
		}
	}()
	runErr := s.sess.RunWithOptions(inVals, outVals, runOpts)
	close(stop)
	<-watchDone
	if runErr != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err() // the run was terminated by our watchdog
		}
		return nil, fmt.Errorf("onnx: run: %w", runErr)
	}

	out := make(map[string]Tensor, len(s.outNames))
	for i, name := range s.outNames {
		t, err := fromOrtValue(outVals[i])
		if err != nil {
			return nil, fmt.Errorf("onnx: output %q: %w", name, err)
		}
		out[name] = t
	}
	return out, nil
}

func (s *ortSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sess == nil {
		return nil
	}
	err := s.sess.Destroy()
	s.sess = nil
	return err
}

// toOrtValue builds an ORT tensor from a boundary Tensor. The input slice is
// copied so the ORT tensor (which aliases the Go slice) owns independent storage
// that outlives the caller's buffer for the Run's duration.
func toOrtValue(t Tensor) (ort.Value, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	shape := ort.NewShape(t.Shape...)
	switch t.Dtype() {
	case DtypeFloat32:
		return ort.NewTensor(shape, append([]float32(nil), t.Float32...))
	case DtypeInt64:
		return ort.NewTensor(shape, append([]int64(nil), t.Int64...))
	default:
		return nil, ErrUnavailable
	}
}

// fromOrtValue copies an ORT output tensor into a boundary Tensor. The copy is
// required because the ORT value's C memory is destroyed right after Run.
func fromOrtValue(v ort.Value) (Tensor, error) {
	shape := []int64(v.GetShape())
	switch tv := v.(type) {
	case *ort.Tensor[float32]:
		return NewFloat32(shape, append([]float32(nil), tv.GetData()...)), nil
	case *ort.Tensor[int64]:
		return NewInt64(shape, append([]int64(nil), tv.GetData()...)), nil
	default:
		return Tensor{}, fmt.Errorf("onnx: unsupported output tensor type %T", v)
	}
}
