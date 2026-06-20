package rtc

import (
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/audio"
	"github.com/RenanQueiroz/hina-agent/internal/events"
	"github.com/pion/opus"
	"github.com/pion/webrtc/v4"
)

// maxDecodedSamples bounds the per-packet decode buffer at 120 ms of 48 kHz
// stereo — larger than any single Opus frame, so DecodeToFloat32 never needs a
// bigger scratch buffer.
const maxDecodedSamples = audio.InputSampleRate / 1000 * 120 * 2

// inbound is the browser→server mic pipeline: read RTP, decode Opus to 48 kHz
// PCM, resample to 16 kHz for the (Phase 6) ASR consumer, and resample to 24 kHz
// to feed the loopback source. In Phase 3 the 16 kHz stream is only metered (the
// capture cursor); no model consumes it yet.
type inbound struct {
	s     *Session
	rsASR *audio.Resampler // 48k -> 16k
	rsOut *audio.Resampler // 48k -> 24k

	// Touched only by the single readLoop goroutine, so no lock is needed.
	captureSamples int64 // accumulated 16 kHz samples (capture cursor)
	lastMeta       time.Duration
	rtp            rtpStats // receiver-side loss/jitter, computed from RTP
}

func newInbound(s *Session) *inbound {
	in := &inbound{s: s}
	// The rates are fixed valid constants, so construction does not fail in
	// practice; on the off chance it does, leave the resampler nil and the
	// pipeline degrades to "metered, not forwarded" rather than crashing.
	if r, err := audio.NewResampler(audio.InputSampleRate, audio.ASRSampleRate); err == nil {
		in.rsASR = r
	} else {
		s.log.Error("rtc: build ASR resampler", "err", err)
	}
	if r, err := audio.NewResampler(audio.InputSampleRate, audio.OutputSampleRate); err == nil {
		in.rsOut = r
	} else {
		s.log.Error("rtc: build loopback resampler", "err", err)
	}
	return in
}

// readLoop reads RTP from the mic track until the track/peer closes (ReadRTP
// then returns an error), decoding and processing each packet. It runs on its
// own goroutine spawned from OnTrack.
func (in *inbound) readLoop(track *webrtc.TrackRemote) {
	dec, err := opus.NewDecoderWithOutput(audio.InputSampleRate, audio.Channels)
	if err != nil {
		in.s.log.Error("rtc: new opus decoder", "err", err)
		return
	}
	pcm := make([]float32, maxDecodedSamples)
	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			return // track closed (peer hung up / session closed)
		}
		in.s.metrics.incRTP()
		// Update receiver-side network stats (loss/jitter) straight from the RTP
		// header — no pc.GetStats(), so the admin path never races teardown.
		in.rtp.observe(pkt.SequenceNumber, pkt.Timestamp, in.s.nowMicros())
		received, lost, jitter := in.rtp.snapshot()
		in.s.metrics.setNetwork(received, lost, jitter)
		if len(pkt.Payload) == 0 {
			continue // silence/keepalive packet
		}
		n, derr := dec.DecodeToFloat32(pkt.Payload, pcm)
		if derr != nil {
			in.s.metrics.incDecodeErr()
			continue
		}
		if n > 0 {
			in.processPCM48(pcm[:n])
		}
	}
}

// processPCM48 runs the DSP on one chunk of decoded 48 kHz mono PCM: downsample
// to 16 kHz (metered as the capture cursor) and to 24 kHz (fed to the loopback
// source), then emit throttled AudioInputFrame meta. Split out from readLoop so
// it can be unit-tested with synthetic PCM (no Opus encoder needed).
func (in *inbound) processPCM48(pcm []float32) {
	if in.rsASR != nil {
		if asr16k, err := in.rsASR.Process(pcm); err == nil {
			in.captureSamples += int64(len(asr16k))
			// Route to the recognizer when a listening segment is active (Phase 5);
			// a no-op otherwise. Turn boundaries are client-driven here, VAD in Phase 6.
			in.s.feedASR(asr16k)
		} else {
			in.s.log.Debug("rtc: ASR resample", "err", err)
		}
	}
	if in.rsOut != nil {
		if out24, err := in.rsOut.Process(pcm); err == nil && len(out24) > 0 {
			in.s.out.feedLoopback(out24)
		} else if err != nil {
			in.s.log.Debug("rtc: loopback resample", "err", err)
		}
	}
	in.maybeEmitMeta()
}

// maybeEmitMeta emits an AudioInputFrame meta event at most every metaThrottle,
// carrying the 16 kHz capture cursor (the foundation for truncation/barge-in).
func (in *inbound) maybeEmitMeta() {
	now := time.Duration(in.s.nowMicros()) * time.Microsecond
	if now-in.lastMeta < metaThrottle {
		return
	}
	in.lastMeta = now
	captureMs := in.captureSamples / (audio.ASRSampleRate / 1000)
	in.s.metrics.setCaptureMs(captureMs)
	in.s.emit(events.TypeAudioInputFrame, map[string]any{
		"sample_rate": audio.ASRSampleRate,
		"channels":    audio.Channels,
		"capture_ms":  captureMs,
	})
}
