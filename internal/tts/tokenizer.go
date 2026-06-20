package tts

import (
	"encoding/json"
	"fmt"
	"os"
)

// Tokenizer maps Unicode codepoints to Supertonic token IDs. The model ships a
// flat array (unicode_indexer.json) indexed by codepoint; the value is the token
// ID, or -1 for an unknown/out-of-table codepoint. There is no phonemizer and no
// subword vocabulary — tokenization is one ID per rune.
type Tokenizer struct {
	table []int64
}

// LoadTokenizer reads unicode_indexer.json (a JSON array of int64) from a path.
func LoadTokenizer(path string) (*Tokenizer, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("tts: read tokenizer %s: %w", path, err)
	}
	return TokenizerFromBytes(b)
}

// TokenizerFromBytes parses a tokenizer from in-memory (verified) bytes.
func TokenizerFromBytes(b []byte) (*Tokenizer, error) {
	var table []int64
	if err := json.Unmarshal(b, &table); err != nil {
		return nil, fmt.Errorf("tts: parse tokenizer: %w", err)
	}
	if len(table) == 0 {
		return nil, fmt.Errorf("tts: tokenizer is empty")
	}
	return &Tokenizer{table: table}, nil
}

// Encode maps each rune of text to its token ID (in rune order). A codepoint
// outside the table maps to -1, matching the reference implementation. The result
// length equals the rune count of text.
func (t *Tokenizer) Encode(text string) []int64 {
	runes := []rune(text)
	ids := make([]int64, len(runes))
	for i, r := range runes {
		cp := int(r)
		if cp >= 0 && cp < len(t.table) {
			ids[i] = t.table[cp]
		} else {
			ids[i] = -1
		}
	}
	return ids
}
