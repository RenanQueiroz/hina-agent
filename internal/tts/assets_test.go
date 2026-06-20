package tts

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTokenizerEncode(t *testing.T) {
	tok, err := LoadTokenizer(filepath.Join("testdata", "onnx", FileIndexer))
	if err != nil {
		t.Fatalf("load tokenizer: %v", err)
	}
	// The fixture is an identity table for ASCII; codepoints >= table length -> -1.
	got := tok.Encode("AB")
	if len(got) != 2 || got[0] != 'A' || got[1] != 'B' {
		t.Fatalf("Encode(AB) = %v, want [65 66]", got)
	}
	// One non-table codepoint (em dash U+2014) maps to -1; rune count preserved.
	out := tok.Encode("—")
	if len(out) != 1 || out[0] != -1 {
		t.Fatalf("Encode(em dash) = %v, want [-1]", out)
	}
}

func TestLoadVoice(t *testing.T) {
	v, err := LoadVoice(filepath.Join("testdata", "voice_styles", "M1.json"), "M1")
	if err != nil {
		t.Fatalf("load voice: %v", err)
	}
	if v.ID != "M1" {
		t.Fatalf("id = %q", v.ID)
	}
	wantTTL := []float32{0.1, 0.2, 0.3, 0.4}
	if len(v.StyleTTL) != 4 || v.StyleTTL[0] != wantTTL[0] || v.StyleTTL[3] != wantTTL[3] {
		t.Fatalf("style_ttl = %v, want %v (row-major)", v.StyleTTL, wantTTL)
	}
	if len(v.TTLDims) != 3 || v.TTLDims[1] != 2 {
		t.Fatalf("ttl dims = %v", v.TTLDims)
	}
	if len(v.StyleDP) != 4 || v.StyleDP[0] != 0.5 {
		t.Fatalf("style_dp = %v", v.StyleDP)
	}
}

func TestLoadVoiceDimMismatch(t *testing.T) {
	// data length (3) doesn't match dims product (4) -> error.
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte(`{"style_ttl":{"data":[1,2,3],"dims":[4]},"style_dp":{"data":[1],"dims":[1]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadVoice(path, "bad"); err == nil {
		t.Fatal("expected dim/length mismatch error")
	}
}

func TestLoadParams(t *testing.T) {
	p, err := loadParams(filepath.Join("testdata", "onnx", FileConfig))
	if err != nil {
		t.Fatalf("load params: %v", err)
	}
	if p.SampleRate != 100 || p.BaseChunkSize != 2 || p.ChunkCompress != 1 || p.LatentDimBase != 2 {
		t.Fatalf("params = %+v", p)
	}
	if p.chunkSize() != 2 || p.latentDim() != 2 {
		t.Fatalf("derived chunkSize=%d latentDim=%d", p.chunkSize(), p.latentDim())
	}
}

func TestLoadParamsDefaultsOnMissing(t *testing.T) {
	// A missing file returns the shipped defaults (plus an error the caller logs).
	p, err := loadParams(filepath.Join("testdata", "does-not-exist.json"))
	if err == nil {
		t.Fatal("expected error for missing config")
	}
	if p != defaultParams() {
		t.Fatalf("params on missing file = %+v, want defaults", p)
	}
}
