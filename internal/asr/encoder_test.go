package asr

import (
	"context"
	"testing"

	"github.com/RenanQueiroz/hina-agent/internal/onnx"
)

// encodedChunk.frame must index by the PHYSICAL stride, not the valid frame
// count, so a padded encoder output (stride > frames) still reads each hidden
// channel from the right offset.
func TestEncodedChunkFrameStride(t *testing.T) {
	const stride, frames = 7, 3 // 4 trailing padded frames
	data := make([]float32, hiddenDim*stride)
	for d := 0; d < hiddenDim; d++ {
		for x := 0; x < stride; x++ {
			data[d*stride+x] = float32(d*1000 + x) // unique per (channel, time)
		}
	}
	e := encodedChunk{data: data, stride: stride, frames: frames}
	for ts := 0; ts < frames; ts++ {
		got := e.frame(ts)
		for d := 0; d < hiddenDim; d++ {
			want := float32(d*1000 + ts)
			if got.Float32[d] != want {
				t.Fatalf("frame(%d)[%d] = %g, want %g (wrong stride?)", ts, d, got.Float32[d], want)
			}
		}
	}
}

// paddedEncoder returns an encoded tensor whose physical time dim (stride) is
// larger than encoded_len, exercising the trailing-padded-frames path.
type paddedEncoder struct {
	stride, validLen int
}

func (e *paddedEncoder) Run(ctx context.Context, in map[string]onnx.Tensor) (map[string]onnx.Tensor, error) {
	data := make([]float32, hiddenDim*e.stride)
	for d := 0; d < hiddenDim; d++ {
		for x := 0; x < e.stride; x++ {
			data[d*e.stride+x] = float32(d*1000 + x)
		}
	}
	return map[string]onnx.Tensor{
		"encoded":                     onnx.NewFloat32([]int64{1, hiddenDim, int64(e.stride)}, data),
		"encoded_len":                 onnx.NewInt64([]int64{1}, []int64{int64(e.validLen)}),
		"cache_last_channel_next":     in["cache_last_channel"],
		"cache_last_time_next":        in["cache_last_time"],
		"cache_last_channel_len_next": in["cache_last_channel_len"],
	}, nil
}
func (e *paddedEncoder) Close() error { return nil }

func TestRunEncoderClampsValidFramesKeepsStride(t *testing.T) {
	sess := &paddedEncoder{stride: 7, validLen: 3}
	mel := make([][]float32, nMels)
	for m := range mel {
		mel[m] = make([]float32, encoderChunk)
	}
	enc, _, err := runEncoder(context.Background(), sess, mel, encoderChunk, newEncoderCache(), promptAuto)
	if err != nil {
		t.Fatal(err)
	}
	if enc.stride != 7 || enc.frames != 3 {
		t.Fatalf("stride=%d frames=%d, want stride 7 / frames 3", enc.stride, enc.frames)
	}
	// Frame 2 (still valid) must read channel d from offset d*stride+2.
	got := enc.frame(2)
	for d := 0; d < hiddenDim; d++ {
		if want := float32(d*1000 + 2); got.Float32[d] != want {
			t.Fatalf("frame(2)[%d] = %g, want %g", d, got.Float32[d], want)
		}
	}
}

// A truncated encoder output (fewer values than hidden*stride) is rejected, not
// indexed out of bounds.
func TestRunEncoderRejectsShortOutput(t *testing.T) {
	sess := &shortEncoder{}
	mel := make([][]float32, nMels)
	for m := range mel {
		mel[m] = make([]float32, encoderChunk)
	}
	if _, _, err := runEncoder(context.Background(), sess, mel, encoderChunk, newEncoderCache(), promptAuto); err == nil {
		t.Fatal("expected an error for an encoder output shorter than hidden*stride")
	}
}

type shortEncoder struct{}

func (shortEncoder) Run(ctx context.Context, in map[string]onnx.Tensor) (map[string]onnx.Tensor, error) {
	// Shape claims stride 7 but only ships 4 values -> inconsistent.
	return map[string]onnx.Tensor{
		"encoded":                     onnx.NewFloat32([]int64{1, hiddenDim, 7}, []float32{0, 0, 0, 0}),
		"encoded_len":                 onnx.NewInt64([]int64{1}, []int64{7}),
		"cache_last_channel_next":     in["cache_last_channel"],
		"cache_last_time_next":        in["cache_last_time"],
		"cache_last_channel_len_next": in["cache_last_channel_len"],
	}, nil
}
func (shortEncoder) Close() error { return nil }
