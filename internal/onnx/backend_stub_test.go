//go:build !onnx

package onnx

import (
	"errors"
	"testing"
)

// In the default (CGo-free) build the backend is the stub: it reports itself
// unavailable, and every Open fails with ErrUnavailable. The onnx-tagged build
// replaces this with a real ORT backend (see integration_onnx_test.go).
func TestStubBackendUnavailable(t *testing.T) {
	b, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer b.Close()
	if b.Info().Available {
		t.Fatal("stub backend must report Available=false")
	}
	if b.Info().Reason == "" {
		t.Fatal("stub backend should explain why it is unavailable")
	}
	if _, err := b.Open("model.onnx", []string{"in"}, []string{"out"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Open err = %v, want ErrUnavailable", err)
	}
}
