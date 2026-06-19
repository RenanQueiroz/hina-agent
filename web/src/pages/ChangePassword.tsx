import { useState, type FormEvent } from "react";
import { api, ApiError } from "../lib/api";
import { Button, Card, Input } from "../components/ui";

// Forced first-run password change (the bootstrap admin must change before LAN).
export function ChangePassword({ onDone }: { onDone: () => void }) {
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [confirm, setConfirm] = useState("");
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setErr("");
    if (next.length < 8) return setErr("New password must be at least 8 characters.");
    if (next !== confirm) return setErr("Passwords do not match.");
    setBusy(true);
    try {
      await api.changePassword(current, next);
      onDone();
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : "could not change password");
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="grid h-full place-items-center bg-zinc-50 px-4 dark:bg-zinc-950">
      <Card className="w-full max-w-sm p-6">
        <h1 className="mb-1 text-xl font-semibold text-zinc-900 dark:text-zinc-100">
          Change your password
        </h1>
        <p className="mb-5 text-sm text-zinc-500">
          You're using the one-time bootstrap credential. Set a new password to continue.
        </p>
        <form onSubmit={submit} className="space-y-3">
          <Input
            type="password"
            placeholder="Current password"
            value={current}
            onChange={(e) => setCurrent(e.target.value)}
            autoFocus
          />
          <Input
            type="password"
            placeholder="New password (min 8)"
            value={next}
            onChange={(e) => setNext(e.target.value)}
          />
          <Input
            type="password"
            placeholder="Confirm new password"
            value={confirm}
            onChange={(e) => setConfirm(e.target.value)}
          />
          {err && <p className="text-sm text-red-600">{err}</p>}
          <Button type="submit" className="w-full" disabled={busy}>
            {busy ? "Saving…" : "Set password"}
          </Button>
        </form>
      </Card>
    </div>
  );
}
