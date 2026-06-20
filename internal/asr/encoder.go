package asr

import (
	"context"
	"fmt"

	"github.com/RenanQueiroz/hina-agent/internal/onnx"
)

// Nemotron 3.5 streaming 0.6B model dimensions (config.json + the parakeet-rs
// reference). These are fixed by the pinned export; the encoder graph's cache
// inputs encode the same shapes.
const (
	numEncoderLayers  = 24
	hiddenDim         = 1024
	leftContext       = 56 // cache_last_channel time dim
	convContext       = 8  // cache_last_time conv dim
	decoderLSTMDim    = 640
	decoderLSTMLayers = 2
	subsamplingFactor = 8 // FastConformer subsamples mel frames 8x

	// Streaming chunk geometry (NeMo streaming_cfg): 56 main mel frames preceded
	// by 9 pre-encode-cache frames of left context = 65 frames per encoder call,
	// which the encoder subsamples to 7 output frames.
	chunkFrames    = 56
	preEncodeCache = 9
	encoderChunk   = preEncodeCache + chunkFrames // 65

	// promptAuto is the language prompt index for automatic detection
	// (config.json prompt_dictionary "auto").
	promptAuto = 101

	// maxSymbolsPerStep caps RNNT emissions per encoder frame, bounding the inner
	// decode loop against a degenerate non-blank run (reference uses 10).
	maxSymbolsPerStep = 10
)

// Encoder I/O tensor names, verified against the exported graph (encoder.onnx).
var (
	encInputs = []string{
		"processed_signal", "processed_signal_length",
		"cache_last_channel", "cache_last_time", "cache_last_channel_len",
		"prompt_index",
	}
	encOutputs = []string{
		"encoded", "encoded_len",
		"cache_last_channel_next", "cache_last_time_next", "cache_last_channel_len_next",
	}
)

// encoderCache is the cache-aware streaming state threaded between encoder calls:
// the attention (cache_last_channel) and convolution (cache_last_time) caches and
// the running valid-length counter. Owned per stream; the graph slices the
// "_next" outputs to the fixed shapes, so they feed straight back.
type encoderCache struct {
	lastChannel onnx.Tensor // [24,1,56,1024] float32
	lastTime    onnx.Tensor // [24,1,1024,8] float32
	lastLen     onnx.Tensor // [1] int64
}

// newEncoderCache builds a zeroed cache (length 0), the start-of-utterance state.
func newEncoderCache() encoderCache {
	return encoderCache{
		lastChannel: onnx.NewFloat32([]int64{numEncoderLayers, 1, leftContext, hiddenDim},
			make([]float32, numEncoderLayers*leftContext*hiddenDim)),
		lastTime: onnx.NewFloat32([]int64{numEncoderLayers, 1, hiddenDim, convContext},
			make([]float32, numEncoderLayers*hiddenDim*convContext)),
		lastLen: onnx.NewInt64([]int64{1}, []int64{0}),
	}
}

// encodedChunk is one encoder output: the [hidden, time] activations (flat,
// row-major as the graph emits [1,1024,stride]). stride is the PHYSICAL time
// dimension of the tensor (the row-major step between hidden channels); frames is
// the number of VALID frames to decode (<= stride). These differ when the graph
// returns trailing padded frames (encoded_len < the tensor's time dim), so the
// stride must stay separate from the valid count — indexing with the valid count
// would read every hidden channel after the first at the wrong offset.
type encodedChunk struct {
	data   []float32 // len == hiddenDim*stride; encoded[d][t] = data[d*stride+t]
	stride int       // physical time dim of [1,1024,stride]
	frames int       // valid frames to decode (<= stride)
}

// frame copies encoder output frame t into a [1,hiddenDim,1] tensor for the joint,
// using the physical stride so channels are read from the right offsets.
func (e encodedChunk) frame(t int) onnx.Tensor {
	buf := make([]float32, hiddenDim)
	for d := 0; d < hiddenDim; d++ {
		buf[d] = e.data[d*e.stride+t]
	}
	return onnx.NewFloat32([]int64{1, hiddenDim, 1}, buf)
}

// runEncoder runs one cache-aware encoder step over a mel chunk laid out
// [nMels][frames] (the front-end's output for this chunk), returning the encoder
// activations and the advanced cache. length is the number of valid mel frames
// in the chunk. ctx cancels an in-flight run (barge-in / shutdown).
func runEncoder(ctx context.Context, sess onnx.Session, mel [][]float32, length int, cache encoderCache, promptIndex int64) (encodedChunk, encoderCache, error) {
	frames := len(mel[0])
	signal := make([]float32, nMels*frames)
	for m := 0; m < nMels; m++ {
		row := mel[m]
		base := m * frames
		for t := 0; t < frames; t++ {
			signal[base+t] = row[t]
		}
	}
	in := map[string]onnx.Tensor{
		"processed_signal":        onnx.NewFloat32([]int64{1, nMels, int64(frames)}, signal),
		"processed_signal_length": onnx.NewInt64([]int64{1}, []int64{int64(length)}),
		"cache_last_channel":      cache.lastChannel,
		"cache_last_time":         cache.lastTime,
		"cache_last_channel_len":  cache.lastLen,
		"prompt_index":            onnx.NewInt64([]int64{1}, []int64{promptIndex}),
	}
	out, err := sess.Run(ctx, in)
	if err != nil {
		return encodedChunk{}, cache, fmt.Errorf("asr: encoder: %w", err)
	}
	enc, ok := out["encoded"]
	if !ok || enc.Dtype() != onnx.DtypeFloat32 {
		return encodedChunk{}, cache, fmt.Errorf("asr: encoder output 'encoded' missing or not float32")
	}
	encLen, err := scalarInt(out["encoded_len"])
	if err != nil {
		return encodedChunk{}, cache, fmt.Errorf("asr: encoded_len: %w", err)
	}
	// encoded shape is [1,1024,T']: T' is the physical stride; the valid frame
	// count is clamped to encoded_len so trailing padded frames aren't decoded.
	stride := framesOf(enc.Shape)
	if stride < 0 || len(enc.Float32) != hiddenDim*stride {
		return encodedChunk{}, cache, fmt.Errorf("asr: encoder output has %d values, want %d (hidden %d x stride %d)", len(enc.Float32), hiddenDim*stride, hiddenDim, stride)
	}
	outFrames := stride
	if encLen >= 0 && encLen < outFrames {
		outFrames = encLen
	}
	next := encoderCache{
		lastChannel: requireF32(out, "cache_last_channel_next", cache.lastChannel),
		lastTime:    requireF32(out, "cache_last_time_next", cache.lastTime),
		lastLen:     requireI64(out, "cache_last_channel_len_next", cache.lastLen),
	}
	return encodedChunk{data: enc.Float32, stride: stride, frames: outFrames}, next, nil
}

// framesOf returns the last dimension of an [1,1024,T'] encoder output shape.
func framesOf(shape []int64) int {
	if len(shape) == 0 {
		return 0
	}
	return int(shape[len(shape)-1])
}

// scalarInt reads the first element of an int64/int32 tensor as an int.
func scalarInt(t onnx.Tensor) (int, error) {
	switch t.Dtype() {
	case onnx.DtypeInt64:
		if len(t.Int64) == 0 {
			return 0, fmt.Errorf("empty int64 tensor")
		}
		return int(t.Int64[0]), nil
	case onnx.DtypeInt32:
		if len(t.Int32) == 0 {
			return 0, fmt.Errorf("empty int32 tensor")
		}
		return int(t.Int32[0]), nil
	default:
		return 0, fmt.Errorf("not an integer tensor")
	}
}

// requireF32 / requireI64 return the named output tensor if present with the
// right dtype, else the supplied fallback (the prior cache) so a missing "_next"
// output degrades to a held cache rather than a nil tensor.
func requireF32(out map[string]onnx.Tensor, name string, fallback onnx.Tensor) onnx.Tensor {
	if t, ok := out[name]; ok && t.Dtype() == onnx.DtypeFloat32 {
		return t
	}
	return fallback
}

func requireI64(out map[string]onnx.Tensor, name string, fallback onnx.Tensor) onnx.Tensor {
	if t, ok := out[name]; ok && t.Dtype() == onnx.DtypeInt64 {
		return t
	}
	return fallback
}
