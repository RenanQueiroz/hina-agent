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

describe("LiveSession playback stop", () => {
  it("keeps the gate open (no flush) on normal completion, closes+flushes on truncation", () => {
    const s = new LiveSession(() => {}) as unknown as {
      node: { port: { postMessage: ReturnType<typeof vi.fn> } };
      gate: { epoch: number | null; lastSeq: number };
      onControl: (data: string) => void;
    };
    s.node = { port: { postMessage: vi.fn() } };
    s.gate = { epoch: 3, lastSeq: 0 };
    const stop = (truncated: boolean) =>
      JSON.stringify({ type: "PlaybackStopped", payload: { truncated } });

    // Normal completion: gate stays OPEN so a final in-flight frame for this epoch
    // (delivered just after the stop on the unreliable channel) isn't dropped; no flush.
    s.onControl(stop(false));
    expect(s.gate.epoch).toBe(3);
    expect(s.node.port.postMessage).not.toHaveBeenCalledWith({ type: "flush" });

    // Truncation (barge-in / explicit stop): close the gate and drop buffered audio.
    s.node.port.postMessage.mockClear();
    s.onControl(stop(true));
    expect(s.gate.epoch).toBe(null);
    expect(s.node.port.postMessage).toHaveBeenCalledWith({ type: "flush" });
  });
});

describe("LiveSession TTS completion", () => {
  it("surfaces a truncated reply from TTSCompleted into state", () => {
    const states: Array<{ replyTruncated?: boolean }> = [];
    const s = new LiveSession((st) => states.push(st)) as unknown as {
      onControl: (data: string) => void;
    };
    s.onControl(JSON.stringify({ type: "TTSCompleted", payload: { epoch: 1, truncated: true } }));
    expect(states.at(-1)?.replyTruncated).toBe(true);

    // A fresh playback clears the flag.
    s.onControl(JSON.stringify({ type: "PlaybackStarted", payload: { epoch: 2, source: "tts" } }));
    expect(states.at(-1)?.replyTruncated).toBe(false);
  });
});

describe("LiveSession ASR segments", () => {
  it("ignores a stale ASRFinal/partial from a previous segment", () => {
    const states: Array<{ listening?: boolean; transcript?: string; partial?: string }> = [];
    const s = new LiveSession((st) => states.push(st)) as unknown as {
      onControl: (data: string) => void;
    };
    // Segment 0 starts, then segment 1 starts (e.g. the prior one was still
    // finalizing server-side when the user started the next).
    s.onControl(JSON.stringify({ type: "ListenStarted", payload: { seg: 0 } }));
    s.onControl(JSON.stringify({ type: "ListenStarted", payload: { seg: 1 } }));
    expect(states.at(-1)?.listening).toBe(true);

    // A stale partial + final for segment 0 arrive late: both must be ignored so
    // the active segment 1's UI is neither cleared nor shown the old transcript.
    s.onControl(JSON.stringify({ type: "ASRPartial", payload: { seg: 0, text: "stale partial" } }));
    s.onControl(JSON.stringify({ type: "ASRFinal", payload: { seg: 0, text: "stale final" } }));
    expect(states.at(-1)?.listening).toBe(true);
    expect(states.at(-1)?.transcript).toBeUndefined();
    expect(states.at(-1)?.partial).not.toBe("stale partial");

    // Segment 1's own final commits and clears listening.
    s.onControl(JSON.stringify({ type: "ASRFinal", payload: { seg: 1, text: "real" } }));
    expect(states.at(-1)?.listening).toBe(false);
    expect(states.at(-1)?.transcript).toBe("real");
  });

  it("clears listening state on close so the control is usable after reconnect", async () => {
    const states: Array<{ listening?: boolean; partial?: string }> = [];
    const s = new LiveSession((st) => states.push(st)) as unknown as {
      onControl: (data: string) => void;
      close: () => Promise<void>;
    };
    // An active listening segment when the call ends (no terminal event arrives).
    s.onControl(JSON.stringify({ type: "ListenStarted", payload: { seg: 0 } }));
    s.onControl(JSON.stringify({ type: "ASRPartial", payload: { seg: 0, text: "half a sen" } }));
    expect(states.at(-1)?.listening).toBe(true);

    await s.close();
    // listening must be cleared so the Live page shows "Start listening" again.
    expect(states.at(-1)?.listening).toBe(false);
    expect(states.at(-1)?.partial).toBe("");
  });

  it("clears listening even if AudioContext.close never settles", () => {
    const states: Array<{ listening?: boolean; status?: string }> = [];
    const s = new LiveSession((st) => states.push(st)) as unknown as {
      onControl: (data: string) => void;
      close: () => Promise<void>;
      ctx?: { state: string; close: () => Promise<void> };
    };
    // An AudioContext whose close() never resolves (the documented hang case).
    s.ctx = { state: "running", close: () => new Promise<void>(() => {}) };
    s.onControl(JSON.stringify({ type: "ListenStarted", payload: { seg: 0 } }));
    expect(states.at(-1)?.listening).toBe(true);

    void s.close(); // do NOT await — it hangs on the never-settling AudioContext.close
    // The state must already be settled (patched before the hanging await).
    expect(states.at(-1)?.listening).toBe(false);
    expect(states.at(-1)?.status).toBe("closed");
  });
});

describe("LiveSession speak", () => {
  it("sends a SpeakText control envelope with the text/voice/lang", () => {
    const s = new LiveSession(() => {}) as unknown as {
      eventsDC: { readyState: string; send: ReturnType<typeof vi.fn> };
      speak: (text: string, voice?: string, lang?: string) => void;
    };
    s.eventsDC = { readyState: "open", send: vi.fn() };

    s.speak("Hello there.", "M1", "en");
    expect(s.eventsDC.send).toHaveBeenCalledTimes(1);
    const sent = JSON.parse(s.eventsDC.send.mock.calls[0][0] as string);
    expect(sent.type).toBe("SpeakText");
    expect(sent.source).toBe("client");
    expect(sent.payload).toEqual({ text: "Hello there.", voice: "M1", lang: "en" });
  });

  it("is a no-op when the control channel is not open", () => {
    const s = new LiveSession(() => {}) as unknown as {
      eventsDC: { readyState: string; send: ReturnType<typeof vi.fn> };
      speak: (text: string) => void;
    };
    s.eventsDC = { readyState: "connecting", send: vi.fn() };
    s.speak("hi");
    expect(s.eventsDC.send).not.toHaveBeenCalled();
  });
});
