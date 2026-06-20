package audio

import (
	"math"
	"testing"
)

func TestFloatToS16LERoundTrip(t *testing.T) {
	in := []float32{0, 0.5, -0.5, 0.25, -0.25, 1, -1}
	buf := make([]byte, len(in)*2)
	if n := FloatToS16LE(buf, in); n != len(in)*2 {
		t.Fatalf("FloatToS16LE wrote %d bytes, want %d", n, len(in)*2)
	}
	out := make([]float32, len(in))
	if n := S16LEToFloat32(out, buf); n != len(in) {
		t.Fatalf("S16LEToFloat32 read %d samples, want %d", n, len(in))
	}
	for i := range in {
		// s16 quantization is ~1/32768; allow a couple LSBs of error.
		if d := math.Abs(float64(in[i] - out[i])); d > 1e-4 {
			t.Errorf("sample %d: in=%v out=%v diff=%v", i, in[i], out[i], d)
		}
	}
}

func TestFloatToS16LEClampsOutOfRange(t *testing.T) {
	// Values beyond [-1,1] must saturate, never wrap to the opposite sign.
	in := []float32{2, -2, 1.5, -1.5}
	buf := make([]byte, len(in)*2)
	FloatToS16LE(buf, in)
	out := make([]float32, len(in))
	S16LEToFloat32(out, buf)
	if out[0] < 0.99 || out[1] > -0.99 {
		t.Fatalf("clamp failed: +over=%v -over=%v", out[0], out[1])
	}
	if out[2] < 0.99 || out[3] > -0.99 {
		t.Fatalf("clamp failed on 1.5/-1.5: %v %v", out[2], out[3])
	}
}

func TestFloatToS16LEFullScale(t *testing.T) {
	// +1.0 must map to 32767 (not overflow to -32768).
	buf := make([]byte, 2)
	FloatToS16LE(buf, []float32{1})
	if got := int16(uint16(buf[0]) | uint16(buf[1])<<8); got != 32767 {
		t.Fatalf("+1.0 -> %d, want 32767", got)
	}
}

func TestS16LEToFloat32IgnoresOddTrailingByte(t *testing.T) {
	src := []byte{0, 0, 1} // one full sample + a dangling byte
	out := make([]float32, 4)
	if n := S16LEToFloat32(out, src); n != 1 {
		t.Fatalf("read %d samples from 3 bytes, want 1", n)
	}
}
