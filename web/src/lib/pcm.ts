// PCM datachannel framing — the browser mirror of internal/audio/frame.go.
// Each audio-datachannel message is a binary frame: a 20-byte little-endian
// header followed by s16le mono PCM. Keeping this in one small, pure, tested
// module means the wire layout can't silently drift from the Go encoder.

export const AUDIO_FRAME_HEADER_LEN = 20;

export interface AudioFrame {
  seq: number;
  /** Playback epoch; bumped server-side on every (re)start. */
  epoch: number;
  /** Server monotonic microseconds since session start (echoed back for RTT). */
  sendMicros: number;
  /** Decoded mono samples in [-1, 1]. */
  pcm: Float32Array;
}

// parseAudioFrame decodes one audio-datachannel message. It returns null for a
// short or internally-inconsistent frame (declared sample count disagreeing with
// the body length), so a truncated frame is dropped rather than played as noise.
export function parseAudioFrame(buf: ArrayBuffer): AudioFrame | null {
  if (buf.byteLength < AUDIO_FRAME_HEADER_LEN) return null;
  const dv = new DataView(buf);
  const seq = dv.getUint32(0, true);
  const epoch = dv.getUint32(4, true);
  const samples = dv.getUint32(8, true);
  const sendMicros = Number(dv.getBigInt64(12, true));
  const bodyBytes = buf.byteLength - AUDIO_FRAME_HEADER_LEN;
  if (bodyBytes !== samples * 2) return null;
  const pcm = new Float32Array(samples);
  for (let i = 0; i < samples; i++) {
    pcm[i] = dv.getInt16(AUDIO_FRAME_HEADER_LEN + i * 2, true) / 32768;
  }
  return { seq, epoch, sendMicros, pcm };
}

// PlaybackGate tracks the active playback so the unreliable, unordered audio
// channel can be played safely. epoch=null means "not playing" (drop everything,
// e.g. after an interrupt until the next PlaybackStarted).
export interface PlaybackGate {
  epoch: number | null;
  lastSeq: number;
}

// acceptFrame decides whether an incoming audio frame should be played and
// updates the gate. A frame is accepted only if its epoch is the active one and
// its sequence advances — so reordered, duplicated, or post-interrupt frames
// from a superseded playback are dropped instead of glitching or lying about the
// barge-in truncation point.
export function acceptFrame(gate: PlaybackGate, frame: AudioFrame): boolean {
  if (gate.epoch === null || frame.epoch !== gate.epoch) return false;
  if (frame.seq <= gate.lastSeq) return false;
  gate.lastSeq = frame.seq;
  return true;
}
