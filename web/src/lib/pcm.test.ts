import { describe, expect, it } from "vitest";
import { acceptFrame, AUDIO_FRAME_HEADER_LEN, parseAudioFrame, type PlaybackGate } from "./pcm";

// encodeFrame mirrors internal/audio/frame.go's EncodeAudioFrame so the test
// pins the exact little-endian wire layout the Go server produces.
function encodeFrame(seq: number, epoch: number, sendMicros: number, samples: number[]): ArrayBuffer {
  const buf = new ArrayBuffer(AUDIO_FRAME_HEADER_LEN + samples.length * 2);
  const dv = new DataView(buf);
  dv.setUint32(0, seq, true);
  dv.setUint32(4, epoch, true);
  dv.setUint32(8, samples.length, true);
  dv.setBigInt64(12, BigInt(sendMicros), true);
  for (let i = 0; i < samples.length; i++) {
    const clamped = Math.max(-1, Math.min(1, samples[i]));
    dv.setInt16(AUDIO_FRAME_HEADER_LEN + i * 2, Math.round(clamped * 32767), true);
  }
  return buf;
}

describe("parseAudioFrame", () => {
  it("round-trips header fields and PCM", () => {
    const buf = encodeFrame(7, 4, 1234567, [0, 0.5, -0.5, 1, -1]);
    const frame = parseAudioFrame(buf);
    expect(frame).not.toBeNull();
    expect(frame!.seq).toBe(7);
    expect(frame!.epoch).toBe(4);
    expect(frame!.sendMicros).toBe(1234567);
    expect(frame!.pcm.length).toBe(5);
    expect(frame!.pcm[1]).toBeCloseTo(0.5, 3);
    expect(frame!.pcm[3]).toBeCloseTo(1, 3);
    expect(frame!.pcm[4]).toBeCloseTo(-1, 3);
  });

  it("returns null for a short header", () => {
    expect(parseAudioFrame(new ArrayBuffer(8))).toBeNull();
  });

  it("returns null when the body length disagrees with the sample count", () => {
    const buf = encodeFrame(1, 1, 0, [0, 0, 0]);
    expect(parseAudioFrame(buf.slice(0, AUDIO_FRAME_HEADER_LEN + 4))).toBeNull();
  });
});

describe("acceptFrame", () => {
  const f = (seq: number, epoch: number) => ({ seq, epoch, sendMicros: 0, pcm: new Float32Array(0) });

  it("drops everything when not playing (epoch null)", () => {
    const gate: PlaybackGate = { epoch: null, lastSeq: 0 };
    expect(acceptFrame(gate, f(1, 1))).toBe(false);
  });

  it("accepts advancing sequence within the active epoch", () => {
    const gate: PlaybackGate = { epoch: 2, lastSeq: 0 };
    expect(acceptFrame(gate, f(5, 2))).toBe(true);
    expect(acceptFrame(gate, f(6, 2))).toBe(true);
    expect(gate.lastSeq).toBe(6);
  });

  it("drops reordered/duplicate frames (non-advancing seq)", () => {
    const gate: PlaybackGate = { epoch: 2, lastSeq: 6 };
    expect(acceptFrame(gate, f(6, 2))).toBe(false);
    expect(acceptFrame(gate, f(3, 2))).toBe(false);
  });

  it("drops frames from a superseded epoch (post-interrupt/stale)", () => {
    const gate: PlaybackGate = { epoch: 3, lastSeq: 0 };
    expect(acceptFrame(gate, f(100, 2))).toBe(false); // old epoch, even with high seq
    expect(gate.lastSeq).toBe(0);
  });
});
