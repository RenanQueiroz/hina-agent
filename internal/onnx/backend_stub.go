//go:build !onnx

// This is the default build: no CGo, no ONNX Runtime. New returns a backend that
// reports itself unavailable so `hina doctor` and the admin UI can say so
// honestly, and every Open fails with ErrUnavailable. Build with `-tags onnx`
// (and CGO_ENABLED=1, plus an installed ORT 1.26.0 shared library) to link the
// real runtime in backend_onnx.go.

package onnx

// New returns the stub backend. It never errors: "not built with ONNX support"
// is a normal, reportable state, not a failure.
func New(Config) (Backend, error) { return stubBackend{}, nil }

type stubBackend struct{}

func (stubBackend) Open(string, []string, []string) (Session, error) { return nil, ErrUnavailable }

func (stubBackend) OpenBytes([]byte, []string, []string) (Session, error) {
	return nil, ErrUnavailable
}

func (stubBackend) Info() Info {
	return Info{Available: false, Reason: "built without ONNX support (compile with -tags onnx)"}
}

func (stubBackend) Close() error { return nil }
