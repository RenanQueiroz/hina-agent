import { afterEach, describe, expect, it, vi } from "vitest";
import { LiveSession } from "./rtc";
import { AUDIO_FRAME_HEADER_LEN } from "./pcm";

describe("LiveSession connect cancellation", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("releases the mic and establishes no session if closed during getUserMedia", async () => {
    const stop = vi.fn();
    const track = { stop, kind: "audio" };
    const stream = { getTracks: () => [track], getAudioTracks: () => [track] };
    let resolveGUM: (s: unknown) => void = () => {};
    const getUserMedia = vi.fn(() => new Promise((res) => (resolveGUM = res)));
    vi.stubGlobal("navigator", { mediaDevices: { getUserMedia } });
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);

    const s = new LiveSession(() => {});
    const connecting = s.connect(); // awaits getUserMedia (pending)
    await s.close(); // user ends the call mid-setup
    resolveGUM(stream); // getUserMedia resolves AFTER close
    await connecting; // connect must bail, not proceed

    expect(stop).toHaveBeenCalled(); // mic released
    expect(fetchMock).not.toHaveBeenCalled(); // no server call / session established
  });

  it("aborts the in-flight SDP POST signal on close", async () => {
    const s = new LiveSession(() => {}) as unknown as {
      abort: AbortController;
      close: () => Promise<void>;
    };
    expect(s.abort.signal.aborted).toBe(false);
    await s.close();
    expect(s.abort.signal.aborted).toBe(true); // postOffer's fetch is given this signal
  });
});

// frame builds a minimal audio-datachannel message for the given seq/epoch.
function frame(seq: number, epoch: number): ArrayBuffer {
  const buf = new ArrayBuffer(AUDIO_FRAME_HEADER_LEN + 4); // 2 s16 samples
  const dv = new DataView(buf);
  dv.setUint32(0, seq, true);
  dv.setUint32(4, epoch, true);
  dv.setUint32(8, 2, true);
  dv.setBigInt64(12, 0n, true);
  return buf;
}

describe("LiveSession terminal cleanup", () => {
  const fakeMedia = () => {
    const stop = vi.fn();
    const track = { stop, kind: "audio" };
    const stream = { getTracks: () => [track], getAudioTracks: () => [track] };
    return { stop, stream };
  };

  it("releases the mic and reports error on a failed connection", async () => {
    const { stop, stream } = fakeMedia();
    const states: string[] = [];
    const s = new LiveSession((st) => states.push(st.status)) as unknown as {
      stream: unknown;
      pc: { close: () => void };
      onConnState: (st: string) => void;
    };
    s.stream = stream;
    s.pc = { close: vi.fn() };
    s.onConnState("failed");
    await Promise.resolve();
    expect(stop).toHaveBeenCalled();
    expect(states).toContain("error");
  });

  it("waits a grace period before tearing down a transient disconnect", async () => {
    vi.useFakeTimers();
    try {
      const { stop, stream } = fakeMedia();
      const s = new LiveSession(() => {}) as unknown as {
        stream: unknown;
        pc: { close: () => void; connectionState: string };
        onConnState: (st: string) => void;
      };
      s.stream = stream;
      s.pc = { close: vi.fn(), connectionState: "disconnected" };
      s.onConnState("disconnected");
      expect(stop).not.toHaveBeenCalled(); // not torn down immediately
      vi.advanceTimersByTime(5000);
      await Promise.resolve();
      expect(stop).toHaveBeenCalled(); // still disconnected after grace -> torn down
    } finally {
      vi.useRealTimers();
    }
  });

  it("does not tear down if the connection recovers within the grace", async () => {
    vi.useFakeTimers();
    try {
      const { stop, stream } = fakeMedia();
      const s = new LiveSession(() => {}) as unknown as {
        stream: unknown;
        pc: { close: () => void; connectionState: string };
        onConnState: (st: string) => void;
      };
      s.stream = stream;
      s.pc = { close: vi.fn(), connectionState: "disconnected" };
      s.onConnState("disconnected");
      s.pc.connectionState = "connected";
      s.onConnState("connected"); // recovered: cancels the grace timer
      vi.advanceTimersByTime(5000);
      await Promise.resolve();
      expect(stop).not.toHaveBeenCalled();
    } finally {
      vi.useRealTimers();
    }
  });

  it("still stops the mic and pc if AudioContext.close rejects", async () => {
    const { stop, stream } = fakeMedia();
    const pcClose = vi.fn();
    const s = new LiveSession(() => {}) as unknown as {
      stream: unknown;
      pc: { close: () => void };
      ctx: { state: string; close: () => Promise<void> };
      close: () => Promise<void>;
    };
    s.stream = stream;
    s.pc = { close: pcClose };
    s.ctx = { state: "running", close: vi.fn(() => Promise.reject(new Error("boom"))) };
    await s.close(); // must not throw
    expect(stop).toHaveBeenCalled();
    expect(pcClose).toHaveBeenCalled();
  });
});

describe("LiveSession interrupt gating", () => {
  it("drops old-epoch frames once an interrupt is pending", () => {
    // Drive the private audio/interrupt paths directly with fakes (no real
    // RTCPeerConnection / AudioContext needed).
    const s = new LiveSession(() => {}) as unknown as {
      node: { port: { postMessage: ReturnType<typeof vi.fn> } };
      eventsDC: { readyState: string; send: ReturnType<typeof vi.fn> };
      gate: { epoch: number | null; lastSeq: number };
      onAudio: (b: ArrayBuffer) => void;
      interrupt: () => void;
    };
    const post = vi.fn();
    s.node = { port: { postMessage: post } };
    s.eventsDC = { readyState: "open", send: vi.fn() };
    s.gate = { epoch: 5, lastSeq: 0 };

    // A matching-epoch frame is accepted and forwarded to the worklet.
    s.onAudio(frame(1, 5));
    expect(post).toHaveBeenCalledWith(
      expect.objectContaining({ type: "frame" }),
      expect.anything(),
    );

    // Interrupt closes the gate; a subsequent old-epoch frame (arriving before
    // the worklet's flush cursor callback) must be dropped.
    s.interrupt();
    post.mockClear();
    s.onAudio(frame(2, 5));
    expect(post).not.toHaveBeenCalled();
  });

  it("does not let a stale flush reply wedge a newer playback gate", () => {
    const s = new LiveSession(() => {}) as unknown as {
      node: { port: { postMessage: ReturnType<typeof vi.fn> } };
      eventsDC: { readyState: string; send: ReturnType<typeof vi.fn> };
      gate: { epoch: number | null; lastSeq: number };
      onAudio: (b: ArrayBuffer) => void;
      onControl: (data: string) => void;
      onCursor: (c: unknown) => void;
      interrupt: () => void;
    };
    const post = vi.fn();
    s.node = { port: { postMessage: post } };
    s.eventsDC = { readyState: "open", send: vi.fn() };
    s.gate = { epoch: 5, lastSeq: 0 };

    s.interrupt(); // interrupting=5, gate closed, flush posted
    // A new playback (epoch 6) starts before the flush reply lands.
    s.onControl(JSON.stringify({ type: "PlaybackStarted", payload: { epoch: 6, source: "tone" } }));

    // The stale flush cursor for epoch 5 arrives now. It must NOT be re-tagged as
    // progress/interrupt for epoch 6.
    s.eventsDC.send.mockClear();
    s.onCursor({ gen: 5, flush: true, playedSamples: 100, playedMs: 4, lastSendMicros: 0, underruns: 0 });
    expect(s.eventsDC.send).not.toHaveBeenCalled();

    // And the epoch-6 gate must survive: a fresh epoch-6 frame is still accepted.
    post.mockClear();
    s.onAudio(frame(1, 6));
    expect(post).toHaveBeenCalledWith(
      expect.objectContaining({ type: "frame" }),
      expect.anything(),
    );

    // A matching epoch-6 progress report IS forwarded.
    s.onCursor({ gen: 6, flush: false, playedSamples: 48, playedMs: 2, lastSendMicros: 0, underruns: 0 });
    expect(s.eventsDC.send).toHaveBeenCalled();
  });

  it("finalizes an interrupt only from the flush ack, not a stale same-epoch report", () => {
    const s = new LiveSession(() => {}) as unknown as {
      node: { port: { postMessage: ReturnType<typeof vi.fn> } };
      eventsDC: { readyState: string; send: ReturnType<typeof vi.fn> };
      gate: { epoch: number | null; lastSeq: number };
      onCursor: (c: unknown) => void;
      interrupt: () => void;
    };
    s.node = { port: { postMessage: vi.fn() } };
    s.eventsDC = { readyState: "open", send: vi.fn() };
    s.gate = { epoch: 7, lastSeq: 0 };

    s.interrupt(); // interrupting=7, gate closed, flush posted

    // A periodic (non-flush) report for epoch 7 arrives before the flush ack — it
    // must NOT finalize the interrupt.
    s.eventsDC.send.mockClear();
    s.onCursor({ gen: 7, flush: false, playedSamples: 240, playedMs: 10, lastSendMicros: 0, underruns: 0 });
    expect(s.eventsDC.send).not.toHaveBeenCalled();

    // The real flush ack carries the precise final cursor and finalizes.
    s.onCursor({ gen: 7, flush: true, playedSamples: 480, playedMs: 20, lastSendMicros: 0, underruns: 0 });
    expect(s.eventsDC.send).toHaveBeenCalledTimes(1);
    const sent = JSON.parse(s.eventsDC.send.mock.calls[0][0] as string);
    expect(sent.type).toBe("UserInterrupted");
    expect(sent.payload.played_samples).toBe(480);
  });
});
