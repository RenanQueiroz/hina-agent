package onnx

import (
	"testing"
)

func TestTensorDtypeAndValidate(t *testing.T) {
	f := NewFloat32([]int64{2, 3}, make([]float32, 6))
	if f.Dtype() != DtypeFloat32 {
		t.Fatalf("dtype = %v, want float32", f.Dtype())
	}
	if f.Elements() != 6 {
		t.Fatalf("elements = %d, want 6", f.Elements())
	}
	if err := f.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	i := NewInt64([]int64{4}, []int64{1, 2, 3, 4})
	if i.Dtype() != DtypeInt64 {
		t.Fatalf("dtype = %v, want int64", i.Dtype())
	}
	if err := i.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	// int32 (the RNNT decoder's targets/lengths use it, distinct from int64).
	i32 := NewInt32([]int64{1, 1}, []int32{7})
	if i32.Dtype() != DtypeInt32 {
		t.Fatalf("dtype = %v, want int32", i32.Dtype())
	}
	if i32.Elements() != 1 {
		t.Fatalf("elements = %d, want 1", i32.Elements())
	}
	if err := i32.Validate(); err != nil {
		t.Fatalf("validate int32: %v", err)
	}
	if err := (Tensor{Shape: []int64{2}, Int32: []int32{1}}).Validate(); err == nil {
		t.Fatal("expected validate error on int32 shape/length mismatch")
	}

	// Length/shape mismatch is rejected.
	if err := (Tensor{Shape: []int64{2, 2}, Float32: []float32{1}}).Validate(); err == nil {
		t.Fatal("expected validate error on shape/length mismatch")
	}
	// Empty tensor (no data) is rejected.
	if err := (Tensor{Shape: []int64{1}}).Validate(); err == nil {
		t.Fatal("expected validate error on empty tensor")
	}
}
