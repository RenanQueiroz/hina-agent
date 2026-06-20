package tts

import (
	"encoding/json"
	"fmt"
	"os"
)

// params are the Supertonic geometry constants the Go pipeline needs, read from
// tts.json. Defaults match the shipped model so the pipeline is correct even if a
// field is absent (research-findings B2): 44.1 kHz, base chunk 512, TTL chunk
// compress 6, TTL latent dim 24 -> chunkSize 3072, latentDim 144.
type params struct {
	SampleRate    int
	BaseChunkSize int
	ChunkCompress int
	LatentDimBase int
}

func defaultParams() params {
	return params{SampleRate: NativeSampleRate, BaseChunkSize: 512, ChunkCompress: 6, LatentDimBase: 24}
}

// ttsJSON is the subset of tts.json the Go side reads; everything else is baked
// into the ONNX graphs.
type ttsJSON struct {
	AE struct {
		SampleRate    int `json:"sample_rate"`
		BaseChunkSize int `json:"base_chunk_size"`
	} `json:"ae"`
	TTL struct {
		ChunkCompressFactor int `json:"chunk_compress_factor"`
		LatentDim           int `json:"latent_dim"`
	} `json:"ttl"`
}

// loadParams parses tts.json from a path.
func loadParams(path string) (params, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return defaultParams(), fmt.Errorf("tts: read config %s: %w", path, err)
	}
	return paramsFromBytes(b)
}

// paramsFromBytes parses tts.json from in-memory (verified) bytes, falling back to
// the shipped defaults for any missing/zero field so a partial config can't yield
// a degenerate geometry.
func paramsFromBytes(b []byte) (params, error) {
	p := defaultParams()
	var j ttsJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return p, fmt.Errorf("tts: parse config: %w", err)
	}
	if j.AE.SampleRate > 0 {
		p.SampleRate = j.AE.SampleRate
	}
	if j.AE.BaseChunkSize > 0 {
		p.BaseChunkSize = j.AE.BaseChunkSize
	}
	if j.TTL.ChunkCompressFactor > 0 {
		p.ChunkCompress = j.TTL.ChunkCompressFactor
	}
	if j.TTL.LatentDim > 0 {
		p.LatentDimBase = j.TTL.LatentDim
	}
	return p, nil
}

// chunkSize is the audio samples per latent frame (baseChunkSize*chunkCompress).
func (p params) chunkSize() int { return p.BaseChunkSize * p.ChunkCompress }

// latentDim is the vector-estimator/vocoder channel count (latentDimBase*chunkCompress).
func (p params) latentDim() int { return p.LatentDimBase * p.ChunkCompress }
