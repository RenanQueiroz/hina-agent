import { useCallback, useEffect, useRef, useState } from "react";
import { Ear, Mic, MicOff, MessageSquare, Radio, Square, Volume2 } from "lucide-react";
import { LiveSession, type LiveState } from "../lib/rtc";
import { Button, Card } from "../components/ui";

// LivePage is the Phase 3 "go live" test surface: capture the mic over WebRTC,
// hear audio looped back (or a generated tone) from the server, and exercise the
// manual barge-in. Full network metrics live in the Admin view; this page shows
// the client-side state and cursor.
export function LivePage() {
  const [state, setState] = useState<LiveState>({ status: "idle", mode: "idle" });
  const [devices, setDevices] = useState<MediaDeviceInfo[]>([]);
  const [deviceId, setDeviceId] = useState<string>("");
  const [speakText, setSpeakText] = useState("");
  const sessionRef = useRef<LiveSession | null>(null);

  // When a session reaches a terminal state — including a server-side
  // disconnect/failure that the session tears itself down on — drop our ref so a
  // fresh "Go live" can create a new session (the connect guard checks the ref).
  const onState = useCallback((s: LiveState) => {
    setState(s);
    if (s.status === "closed" || s.status === "error") {
      sessionRef.current = null;
    }
  }, []);

  // Tear the session down on unmount so the mic is released.
  useEffect(() => {
    return () => {
      sessionRef.current?.close();
      sessionRef.current = null;
    };
  }, []);

  const refreshDevices = useCallback(async () => {
    try {
      const all = await navigator.mediaDevices.enumerateDevices();
      setDevices(all.filter((d) => d.kind === "audioinput"));
    } catch {
      /* enumeration needs permission on some browsers; ignore until granted */
    }
  }, []);

  useEffect(() => {
    refreshDevices();
  }, [refreshDevices]);

  const connect = async () => {
    if (sessionRef.current) return; // don't start a second session over an existing one
    const session = new LiveSession(onState);
    sessionRef.current = session;
    try {
      await session.connect({ deviceId: deviceId || undefined });
      // Labels become available once mic permission is granted.
      refreshDevices();
    } catch {
      sessionRef.current = null;
    }
  };

  const disconnect = async () => {
    await sessionRef.current?.close();
    sessionRef.current = null;
  };

  const live = state.status === "connecting" || state.status === "connected";

  return (
    <div className="mx-auto max-w-2xl p-6">
      <div className="mb-4 flex items-center gap-2">
        <Radio className="text-indigo-500" size={20} />
        <h1 className="text-lg font-semibold">Live voice (loopback)</h1>
        <StatusPill status={state.status} />
      </div>

      <Card className="mb-4 p-4">
        <label className="mb-1 block text-xs font-medium text-zinc-500">Microphone</label>
        <select
          value={deviceId}
          onChange={(e) => setDeviceId(e.target.value)}
          disabled={live}
          className="w-full rounded-md border border-zinc-300 bg-white px-3 py-2 text-sm dark:border-zinc-700 dark:bg-zinc-900"
        >
          <option value="">System default</option>
          {devices.map((d) => (
            <option key={d.deviceId} value={d.deviceId}>
              {d.label || `Microphone ${d.deviceId.slice(0, 6)}`}
            </option>
          ))}
        </select>

        <div className="mt-4 flex flex-wrap gap-2">
          {!live ? (
            <Button onClick={connect}>
              <Mic size={16} /> Go live
            </Button>
          ) : (
            <Button variant="danger" onClick={disconnect}>
              <MicOff size={16} /> End
            </Button>
          )}
          <Button
            variant="ghost"
            disabled={state.status !== "connected"}
            onClick={() => sessionRef.current?.setMode("loopback")}
          >
            <Volume2 size={16} /> Loopback
          </Button>
          <Button
            variant="ghost"
            disabled={state.status !== "connected"}
            onClick={() => sessionRef.current?.setMode("tone")}
          >
            <Volume2 size={16} /> Tone
          </Button>
          <Button
            variant="ghost"
            disabled={state.status !== "connected" || state.mode === "idle"}
            onClick={() => sessionRef.current?.interrupt()}
          >
            <Square size={16} /> Interrupt
          </Button>
        </div>
      </Card>

      <Card className="mb-4 p-4">
        <h2 className="mb-2 text-sm font-semibold text-zinc-600 dark:text-zinc-300">
          Speak (local TTS)
        </h2>
        <textarea
          value={speakText}
          onChange={(e) => setSpeakText(e.target.value)}
          placeholder="Type a message to hear it spoken aloud over the live session…"
          rows={2}
          maxLength={4000}
          className="w-full resize-y rounded-md border border-zinc-300 bg-white px-3 py-2 text-sm dark:border-zinc-700 dark:bg-zinc-900"
        />
        <div className="mt-2 flex items-center gap-2">
          <Button
            disabled={state.status !== "connected" || speakText.trim() === ""}
            onClick={() => sessionRef.current?.speak(speakText.trim())}
          >
            <MessageSquare size={16} /> Speak
          </Button>
          <span className="text-xs text-zinc-400">
            Requires local TTS enabled on the server ([tts] + onnx build + assets).
          </span>
        </div>
      </Card>

      <Card className="mb-4 p-4">
        <h2 className="mb-2 text-sm font-semibold text-zinc-600 dark:text-zinc-300">
          Listen (local ASR)
        </h2>
        <div className="flex flex-wrap items-center gap-2">
          {!state.listening ? (
            <Button
              disabled={state.status !== "connected"}
              onClick={() => sessionRef.current?.startListen()}
            >
              <Ear size={16} /> Start listening
            </Button>
          ) : (
            <Button variant="danger" onClick={() => sessionRef.current?.stopListen()}>
              <Square size={16} /> Stop &amp; transcribe
            </Button>
          )}
          <span className="text-xs text-zinc-400">
            Requires local ASR enabled on the server ([asr] + onnx build + assets).
          </span>
        </div>
        {state.listening && (
          <p className="mt-3 min-h-[1.25rem] text-sm italic text-zinc-500">
            {state.partial ? state.partial : "Listening…"}
          </p>
        )}
        {!state.listening && state.transcript !== undefined && (
          <div className="mt-3">
            <p className="text-sm text-zinc-800 dark:text-zinc-200">
              {state.transcript || <span className="text-zinc-400">(no speech detected)</span>}
            </p>
            {state.wakeDetected && (
              <p className="mt-1 text-xs text-indigo-500">Agent addressed (wake word detected).</p>
            )}
            {state.transcriptTruncated && (
              <p className="mt-1 text-xs text-amber-600 dark:text-amber-400">
                {truncationNote(state.transcriptTruncationReason)}
              </p>
            )}
          </div>
        )}
      </Card>

      <Card className="p-4">
        <h2 className="mb-2 text-sm font-semibold text-zinc-600 dark:text-zinc-300">Session</h2>
        <dl className="grid grid-cols-2 gap-x-4 gap-y-1 text-sm">
          <Stat label="Mode" value={state.mode} />
          <Stat label="Played" value={state.playedMs != null ? `${state.playedMs} ms` : "—"} />
          <Stat label="Captured" value={state.captureMs != null ? `${state.captureMs} ms` : "—"} />
          <Stat label="Out frame #" value={state.framesOut != null ? String(state.framesOut) : "—"} />
        </dl>
        {state.error && <p className="mt-3 text-sm text-red-500">{state.error}</p>}
        {state.replyTruncated && (
          <p className="mt-3 text-sm text-amber-600 dark:text-amber-400">
            The last spoken reply was cut short (synthesis cap or dropped audio).
          </p>
        )}
        <p className="mt-3 text-xs text-zinc-400">
          Speak and choose <strong>Loopback</strong> to hear yourself echoed back, or{" "}
          <strong>Tone</strong> for a generated test tone. Network loss/jitter/RTT are in Admin →
          Live sessions. Mic on a second LAN device needs HTTPS.
        </p>
      </Card>
    </div>
  );
}

// truncationNote explains why a transcript was cut short server-side.
function truncationNote(reason?: string): string {
  switch (reason) {
    case "dropped":
      return "Some audio was dropped under load — this transcript may be incomplete.";
    case "max_duration":
      return "The segment hit the maximum listening duration — this transcript may be incomplete.";
    case "capped":
      return "The segment reached its processing limit — this transcript may be incomplete.";
    default:
      return "This transcript may be incomplete.";
  }
}

function Stat({ label, value }: { label: string; value: string }) {
  return (
    <>
      <dt className="text-zinc-500">{label}</dt>
      <dd className="text-right font-mono text-zinc-800 dark:text-zinc-200">{value}</dd>
    </>
  );
}

function StatusPill({ status }: { status: LiveState["status"] }) {
  const styles: Record<LiveState["status"], string> = {
    idle: "bg-zinc-100 text-zinc-600 dark:bg-zinc-800 dark:text-zinc-300",
    connecting: "bg-amber-100 text-amber-700 dark:bg-amber-950 dark:text-amber-300",
    connected: "bg-green-100 text-green-700 dark:bg-green-950 dark:text-green-300",
    closed: "bg-zinc-100 text-zinc-600 dark:bg-zinc-800 dark:text-zinc-300",
    error: "bg-red-100 text-red-700 dark:bg-red-950 dark:text-red-300",
  };
  return (
    <span className={`ml-auto rounded-full px-2.5 py-0.5 text-xs font-medium ${styles[status]}`}>
      {status}
    </span>
  );
}
