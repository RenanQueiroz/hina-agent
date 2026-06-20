package asr

import "context"

// Segment resource bounds. A listening segment that never ends (a client that
// stops sending ListenStopped, or a stuck UI) must not pin continuous inference
// or grow the transcript without limit. Once a segment has consumed
// maxSegmentSamples of audio or emitted maxSegmentTokens, the recognizer stops
// running chunks and dropping further audio — freezing CPU + memory — until the
// segment is finalized + reset. The rtc layer pairs this with a wall-clock
// max-duration timer that emits the terminal event. Vars (not consts) so tests
// can lower them. (Mirrors the TTS engine's per-request output budget.)
var (
	maxSegmentSamples = 300 * sampleRate // 5 minutes of audio
	maxSegmentTokens  = 16384            // far above any real 5-min transcript; guards a degenerate model
)

// streamProc is the per-stream streaming decode state machine: it buffers raw
// 16 kHz audio, recomputes the log-mel over the buffered window, and feeds
// fixed 65-frame chunks (9 pre-encode-cache + 56 main) to the cache-aware
// encoder, threading the encoder cache and RNNT decoder state across chunks so a
// turn decodes continuously. It is NOT goroutine-safe; the owning Stream drives
// it from a single goroutine. The chunking, pre-encode-cache fill, and buffer
// trim mirror the validated parakeet-rs reference (process_chunk / process_audio).
type streamProc struct {
	m     *models
	tok   *Tokenizer
	front *melFront
	bias  *BiasContext

	blankID     int
	promptIndex int64

	audioBuf       []float32 // buffered raw 16 kHz samples
	audioProcessed int       // samples consumed into chunks, relative to the (trimmed) buffer
	consumed       int       // CUMULATIVE samples consumed this segment (never trimmed); for the cap
	cache          encoderCache
	dec            decoderState
	chunkIdx       int
	tokens         []int // accumulated emitted ids for the whole utterance
}

func newStreamProc(m *models, tok *Tokenizer, front *melFront, bias *BiasContext, blankID int, promptIndex int64) *streamProc {
	p := &streamProc{m: m, tok: tok, front: front, bias: bias, blankID: blankID, promptIndex: promptIndex}
	p.reset()
	return p
}

// reset clears all per-utterance state for a new turn: encoder cache, decoder
// LSTM state + bias cursor, audio buffer, and accumulated tokens. The model
// bundle is untouched (never reloaded per turn).
func (p *streamProc) reset() {
	p.audioBuf = p.audioBuf[:0]
	p.audioProcessed = 0
	p.consumed = 0
	p.cache = newEncoderCache()
	p.dec = newDecoderState(p.blankID, p.bias)
	p.chunkIdx = 0
	p.tokens = p.tokens[:0]
}

// capped reports whether the segment has hit its resource budget (audio duration
// or token count) and should stop processing further audio.
func (p *streamProc) capped() bool {
	return p.consumed >= maxSegmentSamples || len(p.tokens) >= maxSegmentTokens
}

// Capped reports whether the segment hit its resource budget (exported for the
// owner to surface a truncated result / log).
func (p *streamProc) Capped() bool { return p.capped() }

// feed appends pcm (16 kHz mono float32) and decodes every chunk now fully
// available, threading state across chunks. It returns the number of encoder
// chunks processed this call and whether any new tokens were emitted (so the
// caller can publish an updated partial). ctx cancels an in-flight run. Note
// chunks can be >0 with advanced==false — a chunk of silence decodes to all
// blanks, which is still a processed chunk.
func (p *streamProc) feed(ctx context.Context, pcm []float32) (chunks int, advanced bool, err error) {
	// Past the segment budget: drop audio entirely (don't even buffer it) so a
	// segment that's never stopped can't grow memory or keep running inference.
	if p.capped() {
		return 0, false, nil
	}
	p.audioBuf = append(p.audioBuf, pcm...)
	if len(p.audioBuf) < winLength {
		return 0, false, nil
	}
	mel := p.front.computeMel(p.audioBuf)
	if mel == nil {
		return 0, false, nil
	}
	total := len(mel[0])
	for {
		if p.capped() {
			break // segment budget reached mid-feed: stop running further chunks
		}
		processed := p.audioProcessed / hopLength
		if total-processed < chunkFrames {
			break
		}
		n, cerr := p.decodeChunk(ctx, mel, processed, chunkFrames, encoderChunk)
		if cerr != nil {
			return chunks, advanced, cerr
		}
		p.audioProcessed += chunkFrames * hopLength
		p.consumed += chunkFrames * hopLength
		p.chunkIdx++
		chunks++
		if n > 0 {
			advanced = true
		}
	}
	p.trim()
	return chunks, advanced, nil
}

// flush decodes the trailing audio shorter than a full chunk (the tail of a
// turn), as one final zero-padded chunk whose valid length is pre-encode-cache +
// the remaining frames. It returns the number of chunks processed (0 or 1) and
// whether new tokens were emitted. Safe to call when there is nothing left.
func (p *streamProc) flush(ctx context.Context) (chunks int, advanced bool, err error) {
	if p.capped() || len(p.audioBuf) < winLength {
		return 0, false, nil
	}
	mel := p.front.computeMel(p.audioBuf)
	if mel == nil {
		return 0, false, nil
	}
	total := len(mel[0])
	processed := p.audioProcessed / hopLength
	mainLen := total - processed
	if mainLen <= 0 {
		return 0, false, nil
	}
	n, err := p.decodeChunk(ctx, mel, processed, mainLen, preEncodeCache+mainLen)
	if err != nil {
		return 0, false, err
	}
	p.audioProcessed += mainLen * hopLength
	p.consumed += mainLen * hopLength
	p.chunkIdx++
	return 1, n > 0, nil
}

// decodeChunk builds the fixed 65-wide encoder input for the mel window starting
// at mainStart (mainLen valid main frames preceded by up to preEncodeCache frames
// of left context, zero-padded), runs the encoder with validLen valid frames,
// then RNNT-decodes the encoder output, appending emitted ids. Returns the number
// of tokens emitted by this chunk.
func (p *streamProc) decodeChunk(ctx context.Context, mel [][]float32, mainStart, mainLen, validLen int) (int, error) {
	chunk := buildEncoderChunk(mel, mainStart, mainLen, p.chunkIdx)
	enc, next, err := runEncoder(ctx, p.m.enc, chunk, validLen, p.cache, p.promptIndex)
	if err != nil {
		return 0, err
	}
	p.cache = next
	toks, err := decodeFrames(ctx, p.m.dec, enc, &p.dec, p.bias, p.blankID)
	if err != nil {
		return 0, err
	}
	for _, tk := range toks {
		p.tokens = append(p.tokens, tk.id)
	}
	return len(toks), nil
}

// transcript decodes the accumulated token ids into text (language tags stripped
// by the tokenizer).
func (p *streamProc) transcript() string { return p.tok.Decode(p.tokens) }

// trim bounds the raw-audio buffer once it grows well past the context a chunk
// needs, removing only whole mel frames (a multiple of hopLength) so the mel grid
// stays aligned with audioProcessed.
func (p *streamProc) trim() {
	keep := (preEncodeCache+chunkFrames)*hopLength + winLength
	if len(p.audioBuf) <= keep*2 {
		return
	}
	remove := len(p.audioBuf) - keep
	if remove > p.audioProcessed {
		remove = p.audioProcessed
	}
	remove -= remove % hopLength // whole frames only
	if remove <= 0 {
		return
	}
	p.audioBuf = append(p.audioBuf[:0], p.audioBuf[remove:]...)
	p.audioProcessed -= remove
}

// buildEncoderChunk assembles the [nMels][encoderChunk] (65-wide) mel input: up
// to preEncodeCache frames of left context, then mainLen main frames, the rest
// zero. The first chunk has no left context (zero-padded). Mirrors the
// reference's chunk_data fill.
func buildEncoderChunk(mel [][]float32, mainStart, mainLen, chunkIdx int) [][]float32 {
	out := make([][]float32, nMels)
	for m := range out {
		out[m] = make([]float32, encoderChunk)
	}
	// Left context (pre-encode cache): frames [cacheStart, mainStart), right-
	// aligned to the slot just before the main frames.
	if chunkIdx > 0 {
		cacheStart := mainStart - preEncodeCache
		if cacheStart < 0 {
			cacheStart = 0
		}
		cacheFrames := mainStart - cacheStart
		cacheOffset := preEncodeCache - cacheFrames
		for f := 0; f < cacheFrames; f++ {
			for m := 0; m < nMels; m++ {
				out[m][cacheOffset+f] = mel[m][cacheStart+f]
			}
		}
	}
	// Main frames.
	for f := 0; f < mainLen; f++ {
		src := mainStart + f
		if src >= len(mel[0]) {
			break
		}
		for m := 0; m < nMels; m++ {
			out[m][preEncodeCache+f] = mel[m][src]
		}
	}
	return out
}
