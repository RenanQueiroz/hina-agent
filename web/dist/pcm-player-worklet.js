// PCM player AudioWorklet — the browser playback side of the Phase 3 voice
// path. It receives decoded 24 kHz mono float32 frames from the main thread
// (parsed off the audio datachannel) and plays them through a small ring buffer,
// reporting back the playback cursor so the server knows exactly how much audio
// was actually heard (the foundation for barge-in/truncation).
//
// The AudioContext is created at 24 kHz on the main thread, so process() runs at
// the same rate as the incoming PCM and no resampling is needed here.
//
// Both the port message handler and process() run on the audio rendering
// thread, so the ring buffer needs no locking.

const RING_CAPACITY = 2880; // 120 ms @ 24 kHz — matches the server backpressure budget
const SAMPLES_PER_MS = 24; // 24 kHz
const REPORT_INTERVAL = 1024; // ~43 ms between cursor reports

class PCMPlayer extends AudioWorkletProcessor {
  constructor() {
    super();
    this.ring = new Float32Array(RING_CAPACITY);
    this.readPos = 0;
    this.available = 0;
    this.writtenSamples = 0; // real samples ever enqueued
    this.playedSamples = 0; // real samples ever output (silence excluded)
    this.markers = []; // {endWritten, sendMicros} per enqueued frame
    this.lastSendMicros = 0;
    this.underruns = 0;
    this.sinceReport = 0;
    this.gen = 0; // playback generation (the server epoch), stamped into reports
    this.port.onmessage = (e) => {
      const m = e.data;
      if (m.type === "frame") this.enqueue(m.pcm, m.sendMicros);
      else if (m.type === "flush") this.flush();
      else if (m.type === "reset") this.reset(m.epoch);
    };
  }

  // reset starts a fresh playback: adopt its generation, drop buffered audio AND
  // zero the cursor, so the played-cursor reported during a new playback (and the
  // interrupt truncation point) is relative to that playback. The generation is
  // echoed in every report so the main thread can drop a stale report from a
  // superseded playback.
  reset(gen) {
    this.gen = gen | 0;
    this.available = 0;
    this.readPos = 0;
    this.writtenSamples = 0;
    this.playedSamples = 0;
    this.markers.length = 0;
    this.lastSendMicros = 0;
    this.report();
  }

  enqueue(pcm, sendMicros) {
    const n = pcm.length;
    // Unreliable channel + bounded latency: if the ring can't hold the frame,
    // drop it rather than overwrite unplayed audio.
    if (n > RING_CAPACITY - this.available) return;
    let w = (this.readPos + this.available) % RING_CAPACITY;
    for (let i = 0; i < n; i++) {
      this.ring[w] = pcm[i];
      w = w + 1 === RING_CAPACITY ? 0 : w + 1;
    }
    this.available += n;
    this.writtenSamples += n;
    this.markers.push({ endWritten: this.writtenSamples, sendMicros });
  }

  // flush drops buffered audio immediately (barge-in) but keeps the cursor, then
  // reports it (flagged as a flush acknowledgement) so the main thread can tell
  // the precise final cursor apart from an ordinary periodic progress report.
  flush() {
    this.available = 0;
    this.readPos = 0;
    this.markers.length = 0;
    this.report(true);
  }

  report(flush = false) {
    this.port.postMessage({
      gen: this.gen,
      flush,
      playedSamples: this.playedSamples,
      playedMs: Math.round(this.playedSamples / SAMPLES_PER_MS),
      lastSendMicros: this.lastSendMicros,
      underruns: this.underruns,
    });
  }

  process(_inputs, outputs) {
    const channel = outputs[0] && outputs[0][0];
    if (!channel) return true;
    const n = channel.length;
    let produced = 0;
    for (let i = 0; i < n; i++) {
      if (this.available > 0) {
        channel[i] = this.ring[this.readPos];
        this.readPos = this.readPos + 1 === RING_CAPACITY ? 0 : this.readPos + 1;
        this.available--;
        produced++;
      } else {
        channel[i] = 0;
      }
    }
    if (produced < n) this.underruns++;
    this.playedSamples += produced;
    // Retire markers for frames fully played; remember the newest send stamp.
    while (this.markers.length > 0 && this.markers[0].endWritten <= this.playedSamples) {
      this.lastSendMicros = this.markers.shift().sendMicros;
    }
    this.sinceReport += n;
    if (this.sinceReport >= REPORT_INTERVAL) {
      this.sinceReport = 0;
      this.report();
    }
    return true;
  }
}

registerProcessor("pcm-player", PCMPlayer);
