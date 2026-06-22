package platform

import "testing"

func TestFreeBytes(t *testing.T) {
	n, err := FreeBytes(t.TempDir())
	if err != nil {
		t.Fatalf("FreeBytes: %v", err)
	}
	if n <= 0 {
		t.Fatalf("FreeBytes = %d, want > 0", n)
	}
}
