package asr

import (
	"context"
	"testing"

	"github.com/RenanQueiroz/hina-agent/internal/onnx"
)

// fakeDecoder is a scripted decoder_joint session: logitsFor returns the logits
// for each call (keyed by the target token and call index), and it echoes the
// LSTM state tensors back unchanged.
type fakeDecoder struct {
	logitsFor func(target int32, call int) []float32
	calls     int
	runErr    error
}

func (f *fakeDecoder) Run(ctx context.Context, in map[string]onnx.Tensor) (map[string]onnx.Tensor, error) {
	if f.runErr != nil {
		return nil, f.runErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	target := in["targets"].Int32[0]
	logits := f.logitsFor(target, f.calls)
	f.calls++
	return map[string]onnx.Tensor{
		"outputs":         onnx.NewFloat32([]int64{1, 1, 1, int64(len(logits))}, logits),
		"prednet_lengths": onnx.NewInt32([]int64{1}, []int32{1}),
		"output_states_1": in["input_states_1"],
		"output_states_2": in["input_states_2"],
	}, nil
}
func (f *fakeDecoder) Close() error { return nil }

func emptyChunk(frames int) encodedChunk {
	return encodedChunk{data: make([]float32, hiddenDim*frames), stride: frames, frames: frames}
}

func TestDecodeFramesEmitsThenBlank(t *testing.T) {
	const blank = 4
	// Per frame: first call emits token 1, second call is blank.
	sess := &fakeDecoder{logitsFor: func(target int32, call int) []float32 {
		l := make([]float32, 5)
		if call%2 == 0 {
			l[1] = 10 // emit token 1
		} else {
			l[blank] = 10 // blank -> next frame
		}
		return l
	}}
	st := newDecoderState(blank, NewBiasContext(nil, 0, 0))
	toks, err := decodeFrames(context.Background(), sess, emptyChunk(2), &st, NewBiasContext(nil, 0, 0), blank)
	if err != nil {
		t.Fatal(err)
	}
	if len(toks) != 2 {
		t.Fatalf("emitted %d tokens, want 2 (one per frame)", len(toks))
	}
	for _, tk := range toks {
		if tk.id != 1 {
			t.Fatalf("token id = %d, want 1", tk.id)
		}
		if tk.logprob > 0 {
			t.Fatalf("logprob must be <= 0, got %g", tk.logprob)
		}
	}
}

func TestDecodeFramesRespectsMaxSymbols(t *testing.T) {
	const blank = 4
	// Never emit blank -> the per-frame cap must stop the inner loop.
	sess := &fakeDecoder{logitsFor: func(target int32, call int) []float32 {
		l := make([]float32, 5)
		l[2] = 5
		return l
	}}
	st := newDecoderState(blank, nil)
	toks, err := decodeFrames(context.Background(), sess, emptyChunk(1), &st, nil, blank)
	if err != nil {
		t.Fatal(err)
	}
	if len(toks) != maxSymbolsPerStep {
		t.Fatalf("emitted %d tokens, want the cap %d", len(toks), maxSymbolsPerStep)
	}
}

func TestDecodeFramesRejectsWrongLogitsWidth(t *testing.T) {
	const blank = 4 // valid joint width is blank+1 = 5
	for _, tc := range []struct {
		name  string
		width int
	}{
		{"too short", blank},    // blank id is out of range -> can never select blank
		{"too long", blank + 5}, // oversized -> an OOB id could be selected
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess := &fakeDecoder{logitsFor: func(int32, int) []float32 {
				l := make([]float32, tc.width)
				if len(l) > 0 {
					l[0] = 1 // never blank (blank index may be out of range anyway)
				}
				return l
			}}
			st := newDecoderState(blank, nil)
			if _, err := decodeFrames(context.Background(), sess, emptyChunk(1), &st, nil, blank); err == nil {
				t.Fatalf("decodeFrames must reject a %s logits vector (len %d, want %d)", tc.name, tc.width, blank+1)
			}
		})
	}
}

// TestDecodeBiasingFlipsConfusedToken is the decode-level proof that name biasing
// rescues a mis-heard name: the model marginally prefers the wrong word-initial
// token, and the boost flips the choice to the biased one.
func TestDecodeBiasingFlipsConfusedToken(t *testing.T) {
	const blank = 4
	const wantTok, confuseTok = 1, 2 // e.g. ▁H vs ▁N
	// First call: confuseTok logit 1.0 just edges out wantTok 0.5; then blank.
	logitsFor := func(target int32, call int) []float32 {
		l := make([]float32, 5)
		if call%2 == 0 {
			l[wantTok] = 0.5
			l[confuseTok] = 1.0
		} else {
			l[blank] = 10
		}
		return l
	}

	// Biasing OFF: the model emits the confused token.
	off := &fakeDecoder{logitsFor: logitsFor}
	stOff := newDecoderState(blank, nil)
	toksOff, _ := decodeFrames(context.Background(), off, emptyChunk(1), &stOff, nil, blank)
	if len(toksOff) != 1 || toksOff[0].id != confuseTok {
		t.Fatalf("biasing off: got %v, want one token id %d", toksOff, confuseTok)
	}

	// Biasing ON for the phrase starting with wantTok: the +contextScore boost
	// (1.0) lifts wantTok (0.5 -> 1.5) past confuseTok (1.0).
	bias := NewBiasContext([][]int{{wantTok, 3}}, 0, 0)
	on := &fakeDecoder{logitsFor: logitsFor}
	stOn := newDecoderState(blank, bias)
	toksOn, _ := decodeFrames(context.Background(), on, emptyChunk(1), &stOn, bias, blank)
	if len(toksOn) != 1 || toksOn[0].id != wantTok {
		t.Fatalf("biasing on: got %v, want one token id %d", toksOn, wantTok)
	}
}
