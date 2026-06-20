package tts

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Voice is a preset speaker's style vectors, loaded from voice_styles/<id>.json.
// Supertonic conditions on two style tensors: styleTTL (fed to the text encoder
// and vector estimator) and styleDP (fed to the duration predictor). V2 ships
// preset voices only — no cloning — so these come from the model repo, never from
// user-supplied reference audio (research-findings B10).
type Voice struct {
	ID       string
	StyleTTL []float32 // row-major, shape StyleTTLDims (e.g. [1,50,256])
	TTLDims  []int64
	StyleDP  []float32 // row-major, shape StyleDPDims (e.g. [1,8,16])
	DPDims   []int64
}

// styleField is one {data,dims,type} block in a voice JSON file. data is nested
// arrays (1–3 dims); dims gives the shape.
type styleField struct {
	Data json.RawMessage `json:"data"`
	Dims []int64         `json:"dims"`
	Type string          `json:"type"`
}

type voiceFile struct {
	StyleTTL styleField `json:"style_ttl"`
	StyleDP  styleField `json:"style_dp"`
}

// LoadVoice reads and flattens a voice style file from a path.
func LoadVoice(path, id string) (*Voice, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("tts: read voice %s: %w", path, err)
	}
	return VoiceFromBytes(b, id)
}

// VoiceFromBytes parses a voice from in-memory (verified) bytes.
func VoiceFromBytes(b []byte, id string) (*Voice, error) {
	var vf voiceFile
	if err := json.Unmarshal(b, &vf); err != nil {
		return nil, fmt.Errorf("tts: parse voice %s: %w", id, err)
	}
	ttl, err := flattenStyle(vf.StyleTTL)
	if err != nil {
		return nil, fmt.Errorf("tts: voice %s style_ttl: %w", id, err)
	}
	dp, err := flattenStyle(vf.StyleDP)
	if err != nil {
		return nil, fmt.Errorf("tts: voice %s style_dp: %w", id, err)
	}
	return &Voice{ID: id, StyleTTL: ttl, TTLDims: vf.StyleTTL.Dims, StyleDP: dp, DPDims: vf.StyleDP.Dims}, nil
}

// flattenStyle decodes the nested data array into a row-major float32 slice and
// checks its length equals the product of dims.
func flattenStyle(f styleField) ([]float32, error) {
	if len(f.Dims) == 0 {
		return nil, fmt.Errorf("missing dims")
	}
	want := int64(1)
	for _, d := range f.Dims {
		if d < 0 {
			return nil, fmt.Errorf("negative dim in %v", f.Dims)
		}
		want *= d
	}
	var out []float32
	if err := flattenJSON(f.Data, &out); err != nil {
		return nil, err
	}
	if int64(len(out)) != want {
		return nil, fmt.Errorf("data has %d elements, dims %v imply %d", len(out), f.Dims, want)
	}
	return out, nil
}

// flattenJSON recursively appends every number in a nested JSON array to out,
// row-major. It accepts arbitrary nesting depth.
func flattenJSON(raw json.RawMessage, out *[]float32) error {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil {
		for _, e := range arr {
			if err := flattenJSON(e, out); err != nil {
				return err
			}
		}
		return nil
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err != nil {
		return fmt.Errorf("expected number or array, got %s", truncate(string(raw)))
	}
	*out = append(*out, float32(n))
	return nil
}

func truncate(s string) string {
	if len(s) > 32 {
		return s[:32] + "…"
	}
	return s
}

// presetVoices is the fixed, shipped voice set (research-findings B10: preset
// voices only, no cloning). Every requested or configured voice id is validated
// against this allowlist before being mapped to a filename, so a voice id can
// never be a relative/absolute path or a traversal — it is one of these literals
// or it is rejected.
var presetVoices = map[string]bool{
	"F1": true, "F2": true, "F3": true, "F4": true, "F5": true,
	"M1": true, "M2": true, "M3": true, "M4": true, "M5": true,
}

// PresetVoices returns the sorted list of shipped voice ids.
func PresetVoices() []string {
	out := make([]string, 0, len(presetVoices))
	for v := range presetVoices {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// validVoice reports whether id is a known preset voice.
func validVoice(id string) bool { return presetVoices[id] }

// voicePath returns the file path for an ALLOWLISTED voice id under a voice_styles
// dir. Callers must validVoice(id) first; the id is a bare token (no separators),
// so the join cannot escape dir.
func voicePath(dir, id string) string {
	return filepath.Join(dir, id+".json")
}
