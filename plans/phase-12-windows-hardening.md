# Phase 12 — Windows native validation & hardening

Status: runs as the gating pass once the features it covers exist (after Phases 4, 5, 7, 11; ideally before each track's GA).
Depends on: Phase 4 (TTS/ORT), Phase 5 (ASR), Phase 7 (sandbox/vault), Phase 11 (managed llama.cpp backend), and the OS primitives written across all phases.
Unblocks: native Windows GA; local ONNX voice on Windows; macOS validation if it was deferred.

## Goal

Turn "built for Windows" into "validated on Windows." Everything was written cross-platform from commit 1 (per the roadmap's Windows-deferral strategy), with Windows-specific code stubbed where a host was needed. This phase runs those validations on **real Windows 11 x64 hosts**, fixes what breaks, and flips the gates that keep local ONNX voice disabled on Windows. It is intentionally a distinct phase per the user's direction: *support Windows from the start, but test it after an initial working version.*

This phase executes the **DEFERRED** items in [`research-findings.md` Part C / C1](research-findings.md#part-c--deferred-does-not-block-starting-validated-in-phase) and the main plan's **Remaining Spikes** Windows list.

## Scope

### In — the Windows spike matrix
1. **Platform spike**: install/run `hina server` from PowerShell; create app state/cache/data dirs with correct ACLs; run SQLite migrations (CGo-free `modernc.org/sqlite`); start and **cancel child processes with Job-Object process-tree cleanup** (no orphaned `llama-server.exe`/`sbx.exe`/agent processes); verify logs (CRLF normalization, path/secret redaction); `hina doctor` without WSL.
2. **`sbx` on Windows**: `winget install -h Docker.sbx`; Docker login; Hypervisor Platform detection; `sbx run shell`; generated kit; workspace mount with **spaces / Unicode / long paths / drive letters / case-insensitive collisions**; `sbx cp`; network policy + `host.docker.internal` host-service access via allow-list; secret injection; failure reporting. Validate `sbx run --name` re-attach (the v0.33.0 change) and the pinned version's exact command lines.
3. **llama.cpp on Windows** — the hands-on Windows validation of the **Phase 11 managed llama.cpp backend**: install via upstream release zip or `winget install ggml.llamacpp`; launch `llama-server.exe` with `--models-preset … --models-max 1 --sleep-idle-seconds …`; health, idle unload, restart, log streaming, cancellation; CUDA (separate `win-cuda-*` zip + `cudart-*` DLLs) vs Vulkan (winget default) capability detection; Pi access through the host-inference gateway.
4. **ONNX Runtime Go spike (the local-voice gate)**: download/pin ORT CPU DLLs into the app runtime dir; load via `yalue` `SetSharedLibraryPath` (not system path); run a tiny ONNX fixture; then **minimal Supertonic and Nemotron fixture passes** with streaming-cache reset across turns and stable latency. Record whether CGo is build-time only or leaks into end-user setup; document WinML/DirectML/CPU EP choice. **Passing this flips local TTS (Phase 4) and local ASR (Phase 5) from "unavailable" to enabled on Windows.**
5. **Secret-vault spike**: DPAPI / Credential Manager vs ACL-guarded key file for **desktop and service-account** deployments; backup/restore behavior; redaction in logs/artifacts. Flip the `internal/platform` Windows master-key impl from stub to validated.
6. **Path/permission fixtures**: workspace roots, long paths, drive letters, case collisions, symlinks/reparse-point privileges, ACL failures, CRLF logs, sandbox mount translation.
7. **WebRTC on Windows**: native browser capture/playback + Pion on localhost loopback (Phase 3 matrix) with latency metrics; full OpenAI Realtime from a Windows browser; agent **browser/device/paste-code auth from a Windows browser** while the CLI runs inside `sbx` (Phase 8).
8. **CI/release smoke on Windows**: server startup, migrations, web assets, config parsing, path handling, process supervision, text-mode chat — already running each phase; here it's expanded to cover the now-validated native features.

### Out
- New product features — this is a hardening/validation pass. Anything broken gets fixed; new capability belongs to its own phase.

## Acceptance gates this phase flips
- Local ASR (Nemotron) and local TTS (Supertonic) **enabled on Windows** after item 4 passes; until then `hina doctor` keeps reporting them unavailable and Windows runs text + cloud STT/TTS + full OpenAI Realtime.
- `sbx` sandboxes pass create/run/cp/policy/secret/mount smoke on Windows; host inference goes through the same gateway/allow-list as macOS/Linux.
- Secret-vault master-key storage uses DPAPI/Credential Manager or ACL-checked files, validated for desktop + service-account.
- Process cleanup uses Job Objects — restarts/shutdowns leave no orphans.

## Testable exit criteria
- [ ] `hina server`, migrations, `hina doctor`, and text-mode chat all work from native PowerShell with no WSL.
- [ ] `sbx` full smoke (install → login → run shell → kit → mounts with spaces/Unicode/long paths → `cp` → policy → `host.docker.internal` → secret) passes on a real Win11 x64 host.
- [ ] `llama-server.exe` install/launch/health/idle-unload/restart/cancel works; CUDA vs Vulkan detected correctly.
- [ ] ORT DLLs load via the `yalue` binding from the app runtime dir; Supertonic + Nemotron fixtures pass with stable latency → local voice gate flipped.
- [ ] Vault master key stored via DPAPI/Credential Manager (or ACL file) survives backup/restore; no secret leakage in logs/artifacts.
- [ ] Job-Object kill leaves no orphaned model/sbx/agent processes on shutdown/restart.
- [ ] Path/permission fixtures pass; logs are CRLF-normalized and redacted.
- [ ] Browser WebRTC loopback + full OpenAI Realtime + agent browser/device-code auth all work from a Windows browser.

## Risks & mitigations
- **ORT Go binding / CGo / DLL / EP issues block local voice** → keep text + cloud + OpenAI Realtime fully functional on Windows regardless; local voice stays gated, not blocking GA of the rest.
- **Windows process cleanup ≠ POSIX groups** → Job Objects validated here; the `internal/platform` primitive was built in Phase 1 to make this swap clean.
- **Filesystem semantics break naive sandbox/workspace code** → dedicated fixtures (long paths, case, drive letters, reparse, ACL).
- **`sbx` on Windows is a moving platform dependency** → validate install/login/policy/host-service/mounts/`cp`/secrets/failure modes on real hosts before marking Windows Automations GA.

## References
- Deferred Windows items + gates: [`research-findings.md`](research-findings.md) Part C / C1; `hina-agent-plan.md` (Remaining Spikes, Major Risks Windows entries, Exit Criteria).
