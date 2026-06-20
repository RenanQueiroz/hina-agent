package asr

import (
	"context"
	"fmt"
	"math"

	"github.com/RenanQueiroz/hina-agent/internal/onnx"
)

// Decoder+joint I/O tensor names, verified against decoder_joint.onnx. targets /
// target_length are int32 (distinct from the encoder's int64 lengths).
var (
	decInputs  = []string{"encoder_outputs", "targets", "target_length", "input_states_1", "input_states_2"}
	decOutputs = []string{"outputs", "prednet_lengths", "output_states_1", "output_states_2"}
)

// token is one RNNT emission: vocabulary id, its log-softmax confidence over the
// full (unbiased) logits, and the encoder frame index it was emitted at.
type token struct {
	id      int
	logprob float32
	frame   int
}

// decoderState is the per-stream RNNT prediction-network state: the two LSTM
// carry tensors [2,1,640], the last emitted token (the prediction input; seeded
// with blank as the start symbol), and the name-biasing trie cursor. Reset
// between utterances.
type decoderState struct {
	state1    onnx.Tensor
	state2    onnx.Tensor
	lastToken int32
	cursor    *biasNode
}

// newDecoderState builds the start-of-utterance prediction state: zeroed LSTM
// carries, lastToken = blank (the RNNT start symbol), bias cursor at the trie
// root.
func newDecoderState(blankID int, bias *BiasContext) decoderState {
	n := decoderLSTMLayers * decoderLSTMDim // [2,1,640] flattened
	return decoderState{
		state1:    onnx.NewFloat32([]int64{decoderLSTMLayers, 1, decoderLSTMDim}, make([]float32, n)),
		state2:    onnx.NewFloat32([]int64{decoderLSTMLayers, 1, decoderLSTMDim}, make([]float32, n)),
		lastToken: int32(blankID),
		cursor:    bias.cursor(),
	}
}

// runDecoder runs one joint step: given an encoder frame [1,1024,1], the last
// token, and the LSTM state, it returns the flattened logits over the full vocab
// (+blank) and the advanced LSTM state.
func runDecoder(ctx context.Context, sess onnx.Session, frame onnx.Tensor, lastToken int32, state1, state2 onnx.Tensor) (logits []float32, ns1, ns2 onnx.Tensor, err error) {
	in := map[string]onnx.Tensor{
		"encoder_outputs": frame,
		"targets":         onnx.NewInt32([]int64{1, 1}, []int32{lastToken}),
		"target_length":   onnx.NewInt32([]int64{1}, []int32{1}),
		"input_states_1":  state1,
		"input_states_2":  state2,
	}
	out, err := sess.Run(ctx, in)
	if err != nil {
		return nil, state1, state2, fmt.Errorf("asr: decoder: %w", err)
	}
	o, ok := out["outputs"]
	if !ok || o.Dtype() != onnx.DtypeFloat32 || len(o.Float32) == 0 {
		return nil, state1, state2, fmt.Errorf("asr: decoder output 'outputs' missing or not float32")
	}
	ns1 = requireF32(out, "output_states_1", state1)
	ns2 = requireF32(out, "output_states_2", state2)
	return o.Float32, ns1, ns2, nil
}

// decodeFrames runs RNNT greedy decoding over an encoder chunk, mutating st (LSTM
// state, last token, bias cursor) so decoding continues seamlessly into the next
// chunk. For each encoder frame it emits non-blank tokens (capped by
// maxSymbolsPerStep) until the joint predicts blank, advancing the LSTM state and
// bias cursor on each emission. ctx cancels an in-flight run.
func decodeFrames(ctx context.Context, sess onnx.Session, enc encodedChunk, st *decoderState, bias *BiasContext, blankID int) ([]token, error) {
	var tokens []token
	for t := 0; t < enc.frames; t++ {
		frame := enc.frame(t)
		for sym := 0; sym < maxSymbolsPerStep; sym++ {
			logits, ns1, ns2, err := runDecoder(ctx, sess, frame, st.lastToken, st.state1, st.state2)
			if err != nil {
				return tokens, err
			}
			// The joint must emit exactly vocab+blank logits. A mismatch (truncated
			// output, or the decoder paired with the wrong tokenizer/model) is a
			// terminal contract violation: with too few logits blank could never be
			// selected (every frame would emit maxSymbolsPerStep arbitrary tokens),
			// and with too many an out-of-vocabulary id could be chosen and fed back
			// as the next target. Fail closed rather than emit a hallucinated transcript.
			if len(logits) != blankID+1 {
				return tokens, fmt.Errorf("asr: decoder produced %d logits, want vocab+blank = %d", len(logits), blankID+1)
			}
			// Select over a biased copy (so name tokens are preferred), but report
			// confidence over the ORIGINAL logits — honest model probability.
			sel := logits
			if bias.Enabled() {
				sel = append([]float32(nil), logits...)
				bias.apply(sel, st.cursor)
			}
			idx, _ := argmax(sel)
			if idx == blankID {
				break // blank: advance to the next frame, state unchanged
			}
			maxOrig := logits[idx]
			for _, v := range logits {
				if v > maxOrig {
					maxOrig = v
				}
			}
			tokens = append(tokens, token{id: idx, logprob: logSoftmaxAt(logits, idx, maxOrig), frame: t})
			st.lastToken = int32(idx)
			st.state1, st.state2 = ns1, ns2
			st.cursor = bias.advance(st.cursor, idx)
		}
	}
	return tokens, nil
}

// argmax returns the index and value of the largest element. For an empty slice
// it returns (-1, -Inf).
func argmax(xs []float32) (int, float32) {
	if len(xs) == 0 {
		return -1, float32(math.Inf(-1))
	}
	bi, bv := 0, xs[0]
	for i, v := range xs {
		if v > bv {
			bv, bi = v, i
		}
	}
	return bi, bv
}

// logSoftmaxAt is the numerically stable log-softmax value at idx, given the
// precomputed max logit (one extra pass for the exp-sum). Matches the reference.
func logSoftmaxAt(logits []float32, idx int, maxLogit float32) float32 {
	var sum float64
	for _, v := range logits {
		sum += math.Exp(float64(v - maxLogit))
	}
	lse := float64(maxLogit) + math.Log(sum)
	return float32(float64(logits[idx]) - lse)
}
