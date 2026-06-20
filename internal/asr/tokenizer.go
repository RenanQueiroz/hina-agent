package asr

import (
	"encoding/binary"
	"errors"
	"math"
	"os"
	"strings"
)

// spaceMarker is SentencePiece's whitespace marker U+2581 ("▁"); it prefixes a
// word and is rendered back to a space on decode.
const spaceMarker = "▁"

// unkID is the conventional SentencePiece <unk> id (piece 0). Used as the
// fallback when a rune can't be covered by any piece during Viterbi encoding.
const unkID = 0

// Tokenizer is the Nemotron SentencePiece (UNIGRAM) vocabulary. It is built once
// per loaded model and used read-only: Decode turns model-emitted ids into text
// (the hot path), and Encode runs the unigram Viterbi segmentation used to build
// the name-biasing trie. Safe for concurrent use after construction.
type Tokenizer struct {
	pieces   []string       // index == token id
	scores   []float32      // unigram log-probs, parallel to pieces
	byID     map[string]int // piece -> id
	langTags map[int]bool   // ids whose piece looks like a <xx>/<xx-XX> language tag
	maxRunes int            // longest piece length in runes (bounds Viterbi back-scan)
}

// LoadTokenizer reads a SentencePiece .model from path.
func LoadTokenizer(path string) (*Tokenizer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return TokenizerFromBytes(data)
}

// TokenizerFromBytes parses a SentencePiece .model protobuf. Only the
// ModelProto.pieces (field 1: piece string=1, score float=2, type=3) are needed;
// trainer/normalizer specs are skipped. The byte layout follows the reference
// (parakeet-rs nemotron.rs) plus score parsing for unigram encoding.
func TokenizerFromBytes(data []byte) (*Tokenizer, error) {
	tk := &Tokenizer{byID: map[string]int{}, langTags: map[int]bool{}}
	p := &pbuf{b: data}
	for {
		num, wt, ok := p.field()
		if !ok {
			break
		}
		if num == 1 && wt == 2 { // a SentencePiece piece
			piece, score, perr := parsePiece(p.bytes())
			if perr != nil {
				return nil, perr
			}
			tk.pieces = append(tk.pieces, piece)
			tk.scores = append(tk.scores, score)
		} else {
			p.skip(wt)
		}
	}
	if len(tk.pieces) == 0 {
		return nil, errors.New("asr: tokenizer has no pieces")
	}
	for i, pc := range tk.pieces {
		tk.byID[pc] = i
		if isLangTag(pc) {
			tk.langTags[i] = true
		}
		if r := len([]rune(pc)); r > tk.maxRunes {
			tk.maxRunes = r
		}
	}
	return tk, nil
}

// parsePiece reads one SentencePiece message: field 1 (string piece), field 2
// (float32 score, wire-type 5). Type (field 3) is ignored.
func parsePiece(b []byte) (piece string, score float32, err error) {
	p := &pbuf{b: b}
	for {
		num, wt, ok := p.field()
		if !ok {
			break
		}
		switch {
		case num == 1 && wt == 2:
			piece = string(p.bytes())
		case num == 2 && wt == 5: // 32-bit float
			if p.i+4 > len(p.b) {
				return "", 0, errors.New("asr: truncated piece score")
			}
			score = math.Float32frombits(binary.LittleEndian.Uint32(p.b[p.i : p.i+4]))
			p.i += 4
		default:
			p.skip(wt)
		}
	}
	return piece, score, nil
}

// Size is the number of pieces (the real vocabulary; the RNNT blank id is this
// value, one past the last piece).
func (t *Tokenizer) Size() int { return len(t.pieces) }

// IsLangTag reports whether id's piece is a language tag (e.g. <en-US>), which
// the multilingual model emits inline and which Decode strips.
func (t *Tokenizer) IsLangTag(id int) bool { return t.langTags[id] }

// Piece returns the raw piece string for id (empty if out of range).
func (t *Tokenizer) Piece(id int) string {
	if id < 0 || id >= len(t.pieces) {
		return ""
	}
	return t.pieces[id]
}

// Decode joins the pieces for ids into text: the whitespace marker becomes a
// space, language-tag and out-of-range ids are dropped, and the result is
// left-trimmed (the leading word marker). Mirrors the reference decode.
func (t *Tokenizer) Decode(ids []int) string {
	var b strings.Builder
	for _, id := range ids {
		if id < 0 || id >= len(t.pieces) || t.langTags[id] {
			continue
		}
		b.WriteString(strings.ReplaceAll(t.pieces[id], spaceMarker, " "))
	}
	return strings.TrimLeft(b.String(), " ")
}

// DecodeSingle decodes one id's piece (marker -> space), without trimming.
func (t *Tokenizer) DecodeSingle(id int) string {
	if id < 0 || id >= len(t.pieces) {
		return ""
	}
	return strings.ReplaceAll(t.pieces[id], spaceMarker, " ")
}

// Encode runs the SentencePiece UNIGRAM Viterbi segmentation of text into token
// ids. The input is normalized the SentencePiece way: leading/trailing space
// trimmed, internal spaces and a dummy prefix mapped to the word marker. Used to
// build the name-biasing trie, so it operates on short phrases; a rune no piece
// covers falls back to <unk>. Returns nil for empty input.
func (t *Tokenizer) Encode(text string) []int {
	norm := normalizeForEncode(text)
	runes := []rune(norm)
	n := len(runes)
	if n == 0 {
		return nil
	}
	const negInf = math.MaxFloat32 * -1
	best := make([]float64, n+1) // best score to cover runes[:i]
	backPiece := make([]int, n+1)
	backStart := make([]int, n+1)
	for i := 1; i <= n; i++ {
		best[i] = negInf
		backPiece[i] = -1
	}
	for end := 1; end <= n; end++ {
		lo := end - t.maxRunes
		if lo < 0 {
			lo = 0
		}
		for start := lo; start < end; start++ {
			if best[start] == negInf {
				continue
			}
			sub := string(runes[start:end])
			id, ok := t.byID[sub]
			if !ok {
				continue
			}
			cand := best[start] + float64(t.scores[id])
			if cand > best[end] {
				best[end] = cand
				backPiece[end] = id
				backStart[end] = start
			}
		}
		// Single-rune fallback to <unk> when nothing else reaches this position,
		// so an out-of-vocab character can't strand the Viterbi path.
		if best[end] == negInf {
			best[end] = best[end-1] - 100 // heavy penalty; keeps the path connected
			backPiece[end] = unkID
			backStart[end] = end - 1
		}
	}
	// Walk back to recover the id sequence.
	var rev []int
	for i := n; i > 0; {
		rev = append(rev, backPiece[i])
		i = backStart[i]
	}
	out := make([]int, len(rev))
	for i := range rev {
		out[i] = rev[len(rev)-1-i]
	}
	return out
}

// normalizeForEncode applies SentencePiece's whitespace handling: trim the
// phrase, then prepend a word marker and replace internal spaces with it
// (add_dummy_prefix + escape_whitespace). Names are short ASCII/Unicode phrases,
// so no NFKC folding is needed here.
func normalizeForEncode(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	return spaceMarker + strings.ReplaceAll(text, " ", spaceMarker)
}

// isLangTag reports whether piece is a language-tag piece: "<xx>" (2 lowercase)
// or "<xx-XX>" (lang-COUNTRY). Mirrors the reference detector.
func isLangTag(piece string) bool {
	b := []byte(piece)
	if len(b) < 4 || b[0] != '<' || b[len(b)-1] != '>' {
		return false
	}
	inner := b[1 : len(b)-1]
	switch len(inner) {
	case 2:
		return isLower(inner[0]) && isLower(inner[1])
	case 5:
		return isLower(inner[0]) && isLower(inner[1]) && inner[2] == '-' && isUpper(inner[3]) && isUpper(inner[4])
	default:
		return false
	}
}

func isLower(c byte) bool { return c >= 'a' && c <= 'z' }
func isUpper(c byte) bool { return c >= 'A' && c <= 'Z' }

// pbuf is a minimal protobuf wire reader (varint + length-delimited + skip).
type pbuf struct {
	b []byte
	i int
}

func (p *pbuf) varint() uint64 {
	var x uint64
	var s uint
	for p.i < len(p.b) {
		c := p.b[p.i]
		p.i++
		x |= uint64(c&0x7f) << s
		if c < 0x80 {
			return x
		}
		s += 7
	}
	return x
}

func (p *pbuf) field() (num, wireType int, ok bool) {
	if p.i >= len(p.b) {
		return 0, 0, false
	}
	t := p.varint()
	return int(t >> 3), int(t & 7), true
}

func (p *pbuf) bytes() []byte {
	n := int(p.varint())
	if n < 0 || p.i+n > len(p.b) {
		n = len(p.b) - p.i
	}
	s := p.b[p.i : p.i+n]
	p.i += n
	return s
}

func (p *pbuf) skip(wireType int) {
	switch wireType {
	case 0:
		p.varint()
	case 1:
		p.i += 8
	case 2:
		p.bytes()
	case 5:
		p.i += 4
	default:
		p.i = len(p.b) // unknown wire type: stop
	}
}
