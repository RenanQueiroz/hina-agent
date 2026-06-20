// Package onnx is Hina's shared local-inference runtime layer: a thin, model-
// agnostic abstraction over ONNX Runtime that the TTS engine (Phase 4) and the
// streaming ASR (Phase 5) both build on.
//
// The actual ORT binding (github.com/yalue/onnxruntime_go) is CGo and is
// isolated behind the `onnx` build tag (backend_onnx.go). The default build
// compiles backend_stub.go instead, so the control plane stays CGO_ENABLED=0 and
// cross-compiles to every Tier-1 target with no native toolchain. Everything in
// this file is build-tag-free: the interfaces, the tensor value type, and the
// lazy-load/idle-unload Lifecycle are pure Go and unit-testable with fake
// sessions — no models, no ORT, no CGo.
//
// Library/version facts (research-findings B1): the yalue binding pinned at
// v1.31.0 speaks ONNX Runtime C API version 26, so it must be paired with an
// ORT **1.26.0** shared library loaded from the app-managed runtime dir via
// SetSharedLibraryPath — never the system path.
package onnx

import (
	"context"
	"errors"
	"fmt"
)

// ErrUnavailable is returned by every Backend operation when no real ORT runtime
// is linked (the default, CGo-free build) or the shared library could not be
// loaded. Callers treat it as "local inference is off" and fall back / report
// unavailable rather than failing hard.
var ErrUnavailable = errors.New("onnx: runtime unavailable (build with -tags onnx and install the ONNX Runtime 1.26.0 shared library)")

// Tensor is a dense tensor crossing the backend boundary. Exactly one of the
// typed data slices is populated; Dtype reports which. Data is laid out
// row-major (C order) matching the Shape. The slice is owned by the caller on
// input and by the caller on output (the backend copies at the boundary so no
// Go slice aliases ORT-owned C memory after Run returns).
//
// Int32 exists alongside Int64 because some graphs distinguish them: the
// Nemotron RNNT decoder_joint takes int32 `targets`/`target_length` and emits an
// int32 `prednet_lengths`, while the encoder uses int64 lengths/`prompt_index`.
type Tensor struct {
	Shape   []int64
	Float32 []float32 // set iff Dtype()==Float32
	Int64   []int64   // set iff Dtype()==Int64
	Int32   []int32   // set iff Dtype()==Int32
}

// Dtype identifies a tensor's element type.
type Dtype int

const (
	DtypeUnknown Dtype = iota
	DtypeFloat32
	DtypeInt64
	DtypeInt32
)

// Dtype reports the populated element type.
func (t Tensor) Dtype() Dtype {
	switch {
	case t.Float32 != nil:
		return DtypeFloat32
	case t.Int64 != nil:
		return DtypeInt64
	case t.Int32 != nil:
		return DtypeInt32
	default:
		return DtypeUnknown
	}
}

// NewFloat32 builds a float32 tensor. len(data) must equal the product of shape.
func NewFloat32(shape []int64, data []float32) Tensor {
	return Tensor{Shape: shape, Float32: data}
}

// NewInt64 builds an int64 tensor. len(data) must equal the product of shape.
func NewInt64(shape []int64, data []int64) Tensor {
	return Tensor{Shape: shape, Int64: data}
}

// NewInt32 builds an int32 tensor. len(data) must equal the product of shape.
func NewInt32(shape []int64, data []int32) Tensor {
	return Tensor{Shape: shape, Int32: data}
}

// Elements is the flattened element count implied by Shape (product of dims, 1
// for a scalar/empty shape).
func (t Tensor) Elements() int64 {
	n := int64(1)
	for _, d := range t.Shape {
		n *= d
	}
	return n
}

// Validate checks the tensor is internally consistent: a single populated dtype
// whose length matches the shape's flattened element count.
func (t Tensor) Validate() error {
	n := t.Elements()
	switch t.Dtype() {
	case DtypeFloat32:
		if int64(len(t.Float32)) != n {
			return fmt.Errorf("onnx: float32 tensor has %d elements, shape %v implies %d", len(t.Float32), t.Shape, n)
		}
	case DtypeInt64:
		if int64(len(t.Int64)) != n {
			return fmt.Errorf("onnx: int64 tensor has %d elements, shape %v implies %d", len(t.Int64), t.Shape, n)
		}
	case DtypeInt32:
		if int64(len(t.Int32)) != n {
			return fmt.Errorf("onnx: int32 tensor has %d elements, shape %v implies %d", len(t.Int32), t.Shape, n)
		}
	default:
		return errors.New("onnx: tensor has no populated data (Float32, Int64, or Int32)")
	}
	return nil
}

// Session is a loaded ONNX graph. Run executes one forward pass: inputs are keyed
// by the input names the session was opened with (all must be present); the
// result is keyed by the output names. ctx cancels the run — including an
// already-dispatched ORT call, which is terminated mid-flight — so a barge-in /
// supersede stops expensive inference promptly instead of running to completion
// while holding the serialized session. The ORT-backed session serializes Run
// internally; callers should otherwise treat a Session as single-flight.
type Session interface {
	Run(ctx context.Context, inputs map[string]Tensor) (map[string]Tensor, error)
	Close() error
}

// Backend is a native ONNX Runtime handle. Open loads a model from a file path;
// OpenBytes loads it from an in-memory buffer (so a caller that has already
// checksum-verified the bytes can load EXACTLY those, with no reopen-by-path that
// a concurrent writer could swap). Both declare the model's input/output tensor
// names (ORT needs them up front for a dynamic session). Info reports availability
// + version/provider/lib path for `hina doctor` and the admin UI. Close releases
// the runtime environment.
type Backend interface {
	Open(modelPath string, inputNames, outputNames []string) (Session, error)
	OpenBytes(model []byte, inputNames, outputNames []string) (Session, error)
	Info() Info
	Close() error
}

// Config configures a Backend. LibFile, when set, is the EXACT path of the ORT
// shared library to load — callers that have already checksum-verified the
// library pass it so the loaded path is exactly the verified one (no directory
// search that could pick an unverified same-named file). Otherwise LibDir is the
// app-managed directory the library is found under (research-findings B1: never
// the system path), and an empty LibDir falls back to the
// ONNXRUNTIME_SHARED_LIBRARY_PATH env var. IntraOpThreads bounds ORT's per-op CPU
// threads (0 = ORT default).
type Config struct {
	LibFile        string
	LibDir         string
	IntraOpThreads int
}

// Info describes the linked runtime for observability. In the stub build
// Available is false and Reason explains why; in the onnx build it carries the
// resolved ORT version, execution provider, and shared-library path.
type Info struct {
	Available bool   `json:"available"`
	Version   string `json:"version,omitempty"`  // ORT library version, e.g. "1.26.0"
	Provider  string `json:"provider,omitempty"` // execution provider, e.g. "CPU"
	LibPath   string `json:"lib_path,omitempty"` // resolved shared-library path
	Reason    string `json:"reason,omitempty"`   // why unavailable (stub build / missing lib)
}
