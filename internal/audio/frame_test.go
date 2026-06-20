package audio

import (
	"errors"
	"math"
	"testing"
)

func TestAudioFrameRoundTrip(t *testing.T) {
	samples := []float32{0, 0.5, -0.5, 0.9, -0.9}
	dst := make([]byte, EncodedAudioFrameLen(len(samples)))
	n := EncodeAudioFrame(dst, 42, 3, 1_234_567, samples)
	if n != len(dst) {
		t.Fatalf("encoded %d bytes, want %d", n, len(dst))
	}

	f, err := DecodeAudioFrame(dst)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if f.Seq != 42 || f.Epoch != 3 || f.Samples != uint32(len(samples)) || f.SendMicros != 1_234_567 {
		t.Fatalf("header mismatch: %+v", f)
	}
	got := make([]float32, f.Samples)
	S16LEToFloat32(got, f.PCM)
	for i := range samples {
		if d := math.Abs(float64(samples[i] - got[i])); d > 1e-4 {
			t.Errorf("sample %d: in=%v out=%v", i, samples[i], got[i])
		}
	}
}

func TestDecodeAudioFrameRejectsShortHeader(t *testing.T) {
	if _, err := DecodeAudioFrame([]byte{1, 2, 3}); !errors.Is(err, ErrShortAudioFrame) {
		t.Fatalf("err=%v, want ErrShortAudioFrame", err)
	}
}

func TestDecodeAudioFrameRejectsTruncatedBody(t *testing.T) {
	// Declares 5 samples (10 PCM bytes) but only carries 4.
	dst := make([]byte, EncodedAudioFrameLen(5))
	EncodeAudioFrame(dst, 1, 0, 0, make([]float32, 5))
	if _, err := DecodeAudioFrame(dst[:AudioFrameHeaderLen+4]); !errors.Is(err, ErrShortAudioFrame) {
		t.Fatalf("err=%v, want ErrShortAudioFrame for truncated body", err)
	}
}
