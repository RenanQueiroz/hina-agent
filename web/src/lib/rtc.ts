// LiveSession drives the browser side of the Phase 3 WebRTC voice path: capture
// the mic, negotiate with the Go/Pion server over the application/sdp endpoint,
// receive PCM over the audio datachannel into the player worklet, and exchange
// the typed event envelope over the control datachannel.
import {
  TypeAudioInputFrame,
  TypeAudioOutputFrame,
  TypeError as TypeErrorEvent,
  TypeModeChanged,
  TypePlaybackProgress,
  TypePlaybackStarted,
  TypePlaybackStopped,
  TypeSpeakText,
  TypeTTSCompleted,
  TypeUserInterrupted,
  type Event as ServerEvent,
} from "./events.gen";
import { acceptFrame, parseAudioFrame, type PlaybackGate } from "./pcm";

export const PLAYBACK_SAMPLE_RATE = 24000;

// DISCONNECT_GRACE_MS mirrors the server's disconnectGrace: an ICE "disconnected"
// state is often transient, so wait before tearing the call down.
const DISCONNECT_GRACE_MS = 5000;

export type LiveStatus = "idle" | "connecting" | "connected" | "closed" | "error";

export interface LiveState {
  status: LiveStatus;
  mode: string; // idle | loopback | tone | tts
  error?: string;
  captureMs?: number;
  playedMs?: number;
  framesOut?: number;
  /** True when the last spoken reply was cut short (synthesis cap or dropped frames). */
  replyTruncated?: boolean;
}

interface ConnectOpts {
  deviceId?: string;
  conversationId?: string;
}

// cursorReport is what the player worklet posts back to the main thread. gen is
// the playback generation (server epoch) the report belongs to, so a stale
// report from a superseded playback can be dropped.
interface CursorReport {
  gen: number;
  /** True only for the report produced by a flush (the barge-in final cursor). */
  flush: boolean;
  playedSamples: number;
  playedMs: number;
  lastSendMicros: number;
  underruns: number;
}

export class LiveSession {
  private pc?: RTCPeerConnection;
  private eventsDC?: RTCDataChannel;
  private audioDC?: RTCDataChannel;
  private ctx?: AudioContext;
  private node?: AudioWorkletNode;
  private stream?: MediaStream;
  private state: LiveState = { status: "idle", mode: "idle" };
  // Gate for the unreliable audio channel: only play frames of the active epoch
  // in advancing sequence order (see pcm.acceptFrame).
  private gate: PlaybackGate = { epoch: null, lastSeq: 0 };
  // Epoch being interrupted while we await the worklet's final flush cursor; null
  // when not interrupting.
  private interrupting: number | null = null;
  // Set by close(): a multi-await connect() checks this after each await so an
  // End/unmount during setup can't resurrect the mic or a live server session.
  private closed = false;
  // Aborts the in-flight SDP POST when close() is called, so a cancelled call
  // doesn't wait on the network and the server can roll the session back.
  private readonly abort = new AbortController();
  // Grace timer for a transient ICE "disconnected" state.
  private disconnectTimer?: ReturnType<typeof setTimeout>;

  constructor(private readonly onState: (s: LiveState) => void) {}

  getState(): LiveState {
    return this.state;
  }

  private patch(p: Partial<LiveState>) {
    this.state = { ...this.state, ...p };
    this.onState(this.state);
  }

  async connect(opts: ConnectOpts = {}) {
    if (this.pc || this.closed) return;
    this.patch({ status: "connecting", error: undefined, mode: "idle" });
    try {
      this.stream = await navigator.mediaDevices.getUserMedia({
        audio: {
          echoCancellation: true,
          noiseSuppression: true,
          autoGainControl: true,
          ...(opts.deviceId ? { deviceId: { exact: opts.deviceId } } : {}),
        },
      });
      // After every await, bail if close() raced in — otherwise the just-acquired
      // mic / peer connection would outlive the user's teardown. close() stops the
      // mic and tears down whatever we created so far.
      if (this.closed) return void (await this.close());

      const pc = new RTCPeerConnection();
      this.pc = pc;
      pc.onconnectionstatechange = () => this.onConnState(pc.connectionState);

      this.eventsDC = pc.createDataChannel("events", { ordered: true });
      this.eventsDC.onmessage = (ev) => this.onControl(ev.data);

      this.audioDC = pc.createDataChannel("audio", { ordered: false, maxRetransmits: 0 });
      this.audioDC.binaryType = "arraybuffer";
      this.audioDC.onmessage = (ev) => this.onAudio(ev.data as ArrayBuffer);

      for (const track of this.stream.getAudioTracks()) pc.addTrack(track, this.stream);

      await this.setupPlayback();
      if (this.closed) return void (await this.close());

      const offer = await pc.createOffer();
      await pc.setLocalDescription(offer);
      await waitForIceGathering(pc);
      if (this.closed) return void (await this.close());

      const answer = await postOffer(pc.localDescription!.sdp, opts.conversationId, this.abort.signal);
      if (this.closed) return void (await this.close());
      await pc.setRemoteDescription({ type: "answer", sdp: answer });
      if (this.closed) return void (await this.close());
    } catch (err) {
      // An aborted SDP POST is an intentional cancellation (close() ran), not an
      // error — tear down quietly rather than surfacing a failure.
      if (err instanceof DOMException && err.name === "AbortError") {
        await this.close();
        return;
      }
      this.patch({ status: "error", error: errMessage(err) });
      await this.close();
      throw err;
    }
  }

  private async setupPlayback() {
    // 24 kHz context so the worklet plays the incoming PCM 1:1 (no resampling).
    const ctx = new AudioContext({ sampleRate: PLAYBACK_SAMPLE_RATE });
    this.ctx = ctx;
    await ctx.audioWorklet.addModule("/pcm-player-worklet.js");
    const node = new AudioWorkletNode(ctx, "pcm-player");
    node.connect(ctx.destination);
    node.port.onmessage = (e) => this.onCursor(e.data as CursorReport);
    this.node = node;
    if (ctx.state === "suspended") await ctx.resume();
  }

  private onConnState(state: RTCPeerConnectionState) {
    switch (state) {
      case "connected":
        this.clearDisconnectTimer(); // recovered
        this.patch({ status: "connected" });
        break;
      case "failed":
        // Terminal: surface the error AND release the mic / audio graph (a failed
        // RTCPeerConnection does not stop independent MediaStream tracks).
        this.patch({ status: "error", error: "connection failed" });
        void this.close();
        break;
      case "closed":
        void this.close();
        break;
      case "disconnected":
        // Transient/degraded: ICE may recover. Give it a grace period before
        // tearing down (mirrors the server), instead of dropping the call on a
        // short network blip.
        if (this.disconnectTimer === undefined) {
          this.disconnectTimer = setTimeout(() => {
            this.disconnectTimer = undefined;
            if (this.pc?.connectionState === "disconnected") void this.close();
          }, DISCONNECT_GRACE_MS);
        }
        break;
    }
  }

  private clearDisconnectTimer() {
    if (this.disconnectTimer !== undefined) {
      clearTimeout(this.disconnectTimer);
      this.disconnectTimer = undefined;
    }
  }

  private onAudio(buf: ArrayBuffer) {
    const frame = parseAudioFrame(buf);
    if (!frame || !this.node) return;
    // Once an interrupt is pending, drop everything — the gate is already closed,
    // but guard explicitly so no frame slips through before the flush reply.
    if (this.interrupting !== null) return;
    // Drop frames from a superseded playback or arriving out of order — the audio
    // channel is unordered/unreliable, so without this a late frame could play
    // after an interrupt or glitch playback.
    if (!acceptFrame(this.gate, frame)) return;
    // Transfer the PCM buffer to the audio thread to avoid a copy.
    this.node.port.postMessage(
      { type: "frame", pcm: frame.pcm, sendMicros: frame.sendMicros },
      [frame.pcm.buffer],
    );
  }

  private onControl(data: string) {
    let e: ServerEvent;
    try {
      e = JSON.parse(data) as ServerEvent;
    } catch {
      return;
    }
    const p = (e.payload ?? {}) as Record<string, unknown>;
    switch (e.type) {
      case TypeModeChanged:
        this.patch({ mode: String(p.mode ?? this.state.mode) });
        break;
      case TypePlaybackStarted:
        // New playback supersedes any pending interrupt: clear it so a late flush
        // reply for the old epoch can't null this new epoch's gate (wedging it).
        this.interrupting = null;
        // Adopt the new epoch and start the worklet + UI cursor fresh. The epoch
        // becomes the worklet's report generation so we can drop stale reports.
        this.gate = { epoch: Number(p.epoch ?? 0), lastSeq: 0 };
        this.node?.port.postMessage({ type: "reset", epoch: Number(p.epoch ?? 0) });
        this.patch({ mode: String(p.source ?? this.state.mode), playedMs: 0, replyTruncated: false });
        break;
      case TypeTTSCompleted:
        // A spoken reply finished; flag it if synthesis was capped or frames were
        // dropped (the spoken text was incomplete) so the UI can warn the user.
        this.patch({ replyTruncated: Boolean(p.truncated) });
        break;
      case TypePlaybackStopped:
        if (Boolean(p.truncated)) {
          // Barge-in / explicit stop: close the gate and drop buffered audio now.
          this.gate = { epoch: null, lastSeq: 0 };
          this.node?.port.postMessage({ type: "flush" });
        }
        // Normal completion (truncated=false): leave the gate OPEN. The reliable
        // control stop can overtake the LAST audio frame (sent just before it, on
        // the separate unreliable channel), so closing the gate here would drop
        // that frame and clip the tail. The worklet drains its ring buffer, and the
        // gate is reset by the next PlaybackStarted.
        this.patch({ mode: "idle" });
        break;
      case TypeAudioInputFrame:
        this.patch({ captureMs: Number(p.capture_ms ?? 0) });
        break;
      case TypeAudioOutputFrame:
        this.patch({ framesOut: Number(p.frame_seq ?? 0) });
        break;
      case TypeErrorEvent:
        this.patch({ error: String(p.error ?? "error") });
        break;
    }
  }

  private onCursor(c: CursorReport) {
    // A flush triggered by interrupt() produces the precise final cursor: send it
    // as the truncation point inside UserInterrupted. Only accept the report that
    // belongs to the epoch being interrupted — a stale report from another
    // playback must not finalize this interrupt.
    if (this.interrupting !== null) {
      // Finalize ONLY from the flush acknowledgement for the interrupted epoch —
      // a periodic progress report (same epoch) that was already queued before
      // the flush must not be mistaken for the final cursor.
      if (!c.flush || c.gen !== this.interrupting) return;
      const epoch = this.interrupting;
      this.interrupting = null;
      this.gate = { epoch: null, lastSeq: 0 };
      this.patch({ playedMs: c.playedMs });
      this.sendControl(TypeUserInterrupted, { epoch, played_samples: c.playedSamples });
      return;
    }
    // Drop a report whose generation isn't the active playback's: a delayed flush
    // report from a superseded epoch must NOT be re-tagged as progress for the new
    // one (which would advance the new cursor past what was actually heard).
    if (this.gate.epoch === null || c.gen !== this.gate.epoch) return;
    this.patch({ playedMs: c.playedMs });
    this.sendControl(TypePlaybackProgress, {
      epoch: this.gate.epoch,
      played_samples: c.playedSamples,
      ack_send_micros: c.lastSendMicros,
    });
  }

  // sendControl emits a full Phase 1 event envelope (not just type/payload) over
  // the control channel so the datachannel contract matches the SSE one.
  private sendControl(type: string, payload: unknown) {
    if (this.eventsDC?.readyState !== "open") return;
    const envelope = {
      event_id: clientEventID(),
      seq: 0,
      server_ts: new Date().toISOString(),
      source: "client",
      type,
      payload,
    };
    this.eventsDC.send(JSON.stringify(envelope));
  }

  /** Select the outbound source: "loopback", "tone", or "idle". */
  setMode(mode: string) {
    this.sendControl(TypeModeChanged, { mode });
  }

  /**
   * Speak text aloud over the live session using the server's local TTS engine
   * (the text-driven voice demo). Supersedes any in-flight spoken reply.
   */
  speak(text: string, voice?: string, lang?: string) {
    this.sendControl(TypeSpeakText, { text, voice, lang });
  }

  /**
   * Barge-in: stop accepting/playing audio immediately and tell the server,
   * carrying the precise played cursor as the truncation point. The worklet
   * flush replies with that cursor (handled in onCursor); without an audio graph
   * we interrupt right away.
   */
  interrupt() {
    if (this.gate.epoch === null) return; // nothing playing
    this.interrupting = this.gate.epoch;
    // Close the gate immediately so no further frames are accepted/played during
    // the async wait for the worklet's flush cursor.
    this.gate = { epoch: null, lastSeq: 0 };
    if (this.node) {
      this.node.port.postMessage({ type: "flush" });
    } else {
      const epoch = this.interrupting;
      this.interrupting = null;
      this.sendControl(TypeUserInterrupted, { epoch, played_samples: 0 });
    }
  }

  async close() {
    this.closed = true; // first, so any in-flight connect() bails after its awaits
    this.clearDisconnectTimer();
    safeCall(() => this.abort.abort()); // unblock any pending SDP POST
    // Snapshot then clear refs up front: a concurrent/duplicate close becomes a
    // no-op and each resource is released exactly once.
    const { stream, pc, eventsDC, audioDC, node, ctx } = this;
    this.pc = undefined;
    this.eventsDC = undefined;
    this.audioDC = undefined;
    this.node = undefined;
    this.ctx = undefined;
    this.stream = undefined;

    // Release the privacy/transport-critical resources first, each guarded so one
    // failure can't skip the others — the mic and peer connection must ALWAYS be
    // torn down, even if something below throws.
    stream?.getTracks().forEach((t) => safeCall(() => t.stop()));
    safeCall(() => eventsDC?.close());
    safeCall(() => audioDC?.close());
    safeCall(() => node?.disconnect());
    safeCall(() => pc?.close());

    // The AudioContext close is the only awaitable and may reject or hang; do it
    // last in its own guard so it can never block the teardown above.
    if (ctx && ctx.state !== "closed") {
      try {
        await ctx.close();
      } catch {
        /* best-effort; the mic/pc are already released */
      }
    }

    if (this.state.status !== "error") this.patch({ status: "closed", mode: "idle" });
  }
}

// postOffer sends the SDP offer to the signaling endpoint and returns the answer.
async function postOffer(sdp: string, conversationId?: string, signal?: AbortSignal): Promise<string> {
  const url = conversationId
    ? `/api/v1/realtime/calls?conversation_id=${encodeURIComponent(conversationId)}`
    : "/api/v1/realtime/calls";
  const res = await fetch(url, {
    method: "POST",
    credentials: "include",
    headers: { "content-type": "application/sdp" },
    body: sdp,
    signal,
  });
  if (!res.ok) {
    let msg = res.statusText;
    try {
      const body = await res.json();
      if (body?.error) msg = body.error;
    } catch {
      /* answer/error may be plain text */
    }
    throw new Error(`realtime call failed: ${msg}`);
  }
  return res.text();
}

// waitForIceGathering resolves once non-trickle gathering completes (the server
// answer is non-trickle too), with a short cap so a stalled gather still sends.
function waitForIceGathering(pc: RTCPeerConnection): Promise<void> {
  if (pc.iceGatheringState === "complete") return Promise.resolve();
  return new Promise((resolve) => {
    const done = () => {
      pc.removeEventListener("icegatheringstatechange", check);
      clearTimeout(timer);
      resolve();
    };
    const check = () => {
      if (pc.iceGatheringState === "complete") done();
    };
    const timer = setTimeout(done, 3000);
    pc.addEventListener("icegatheringstatechange", check);
  });
}

function errMessage(err: unknown): string {
  if (err instanceof Error) return err.message;
  return String(err);
}

// safeCall runs a best-effort teardown step, swallowing any error so one failing
// resource can't abort the rest of close().
function safeCall(fn: () => void) {
  try {
    fn();
  } catch {
    /* best-effort teardown */
  }
}

// clientEventID mints a client-side event id for the envelope. The server
// assigns the durable id/seq for persisted events; this just keeps the
// datachannel envelope well-formed.
function clientEventID(): string {
  const uuid =
    typeof crypto !== "undefined" && "randomUUID" in crypto
      ? crypto.randomUUID()
      : Math.random().toString(16).slice(2);
  return `evt_${uuid}`;
}
