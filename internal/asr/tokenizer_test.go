package asr

import (
	"encoding/binary"
	"math"
	"reflect"
	"testing"
)

// spModelBytes builds a minimal SentencePiece ModelProto from parallel
// piece/score slices, so tokenizer tests stay hermetic (the real 406 KB
// tokenizer.model is exercised only in the onnx integration test).
func spModelBytes(pieces []string, scores []float32) []byte {
	var out []byte
	appendVarint := func(dst []byte, x uint64) []byte {
		for x >= 0x80 {
			dst = append(dst, byte(x)|0x80)
			x >>= 7
		}
		return append(dst, byte(x))
	}
	for i, pc := range pieces {
		var msg []byte
		// field 1: piece string (tag = 1<<3 | 2 = 0x0a)
		msg = append(msg, 0x0a)
		msg = appendVarint(msg, uint64(len(pc)))
		msg = append(msg, pc...)
		// field 2: score float32 (tag = 2<<3 | 5 = 0x15)
		msg = append(msg, 0x15)
		var f [4]byte
		binary.LittleEndian.PutUint32(f[:], math.Float32bits(scores[i]))
		msg = append(msg, f[:]...)
		// top-level field 1: piece message (tag = 0x0a)
		out = append(out, 0x0a)
		out = appendVarint(out, uint64(len(msg)))
		out = append(out, msg...)
	}
	return out
}

func TestTokenizerParseDecode(t *testing.T) {
	pieces := []string{"<unk>", spaceMarker, spaceMarker + "Hi", "na", "<en-US>", "<bg-BG>", "."}
	scores := []float32{-10, -1, -2, -3, -1, -1, -1}
	tk, err := TokenizerFromBytes(spModelBytes(pieces, scores))
	if err != nil {
		t.Fatal(err)
	}
	if tk.Size() != len(pieces) {
		t.Fatalf("size = %d, want %d", tk.Size(), len(pieces))
	}
	// Decode: marker -> space, lang tags stripped, left-trimmed.
	got := tk.Decode([]int{2, 3, 4, 6}) // "▁Hi" + "na" + "<en-US>"(strip) + "."
	if got != "Hina." {
		t.Fatalf("decode = %q, want %q", got, "Hina.")
	}
	if !tk.IsLangTag(4) || !tk.IsLangTag(5) {
		t.Fatal("expected <en-US> and <bg-BG> to be language tags")
	}
	if tk.IsLangTag(2) {
		t.Fatal("▁Hi must not be a language tag")
	}
	// Out-of-range ids are ignored, not panicking.
	if tk.Decode([]int{99, -1, 3}) != "na" {
		t.Fatalf("decode with out-of-range ids = %q", tk.Decode([]int{99, -1, 3}))
	}
}

func TestTokenizerEncodeViterbi(t *testing.T) {
	// With "▁Hina" present and cheapest, Encode("Hina") picks the single piece.
	pieces := []string{"<unk>", spaceMarker, spaceMarker + "Hi", "na", spaceMarker + "Hina"}
	scores := []float32{-100, -5, -3, -3, -1} // ▁Hina (-1) beats ▁Hi+na (-6)
	tk, err := TokenizerFromBytes(spModelBytes(pieces, scores))
	if err != nil {
		t.Fatal(err)
	}
	if got := tk.Encode("Hina"); !reflect.DeepEqual(got, []int{4}) {
		t.Fatalf("encode = %v, want [4] (▁Hina)", got)
	}

	// Drop "▁Hina"; now the best path is ▁Hi + na.
	pieces2 := []string{"<unk>", spaceMarker, spaceMarker + "Hi", "na"}
	scores2 := []float32{-100, -5, -2, -2}
	tk2, _ := TokenizerFromBytes(spModelBytes(pieces2, scores2))
	if got := tk2.Encode("Hina"); !reflect.DeepEqual(got, []int{2, 3}) {
		t.Fatalf("encode = %v, want [2 3] (▁Hi na)", got)
	}
	// Empty input -> nil.
	if got := tk2.Encode("   "); got != nil {
		t.Fatalf("encode(spaces) = %v, want nil", got)
	}
}

func TestTokenizerRejectsEmpty(t *testing.T) {
	if _, err := TokenizerFromBytes(nil); err == nil {
		t.Fatal("expected error for an empty tokenizer model")
	}
}
