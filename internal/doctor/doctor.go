// Package doctor reports host capabilities and per-feature availability. It is
// the user's primary "what works on my machine" surface and is built before the
// features it reports on, returning "unavailable" until each lands.
package doctor

import (
	"context"
	"fmt"
	"runtime"
	"strings"

	"github.com/RenanQueiroz/hina-agent/internal/assets"
	"github.com/RenanQueiroz/hina-agent/internal/config"
	"github.com/RenanQueiroz/hina-agent/internal/onnx"
	"github.com/RenanQueiroz/hina-agent/internal/platform"
	"github.com/RenanQueiroz/hina-agent/internal/sandbox"
	"github.com/RenanQueiroz/hina-agent/internal/store"
)

// Check is one capability result.
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | missing | unavailable | warn | error
	Detail string `json:"detail"`
}

// Report is the full host capability report.
type Report struct {
	OS        string  `json:"os"`
	Arch      string  `json:"arch"`
	Tier      string  `json:"tier"`
	GoVersion string  `json:"go_version"`
	Checks    []Check `json:"checks"`
}

// Run gathers the capability report. It does not mutate state beyond ensuring
// the application directories exist.
func Run(ctx context.Context, cfg config.Config, paths platform.Paths) Report {
	r := Report{
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		Tier:      tier(),
		GoVersion: runtime.Version(),
	}

	// Application directories.
	if err := platform.EnsureAll(paths); err != nil {
		r.add("app directories", "error", err.Error())
	} else {
		r.add("app directories", "ok", paths.Data)
	}

	// Database.
	if st, err := store.Open(paths.DBPath()); err != nil {
		r.add("database (sqlite)", "error", err.Error())
	} else {
		_ = st.Close()
		r.add("database (sqlite)", "ok", paths.DBPath())
	}

	// External runtimes (present-or-not; deeper validation in their phases).
	r.addTool(ctx, "docker", "Phase 7 sandbox prerequisite", "docker", "--version")
	r.addTool(ctx, "llama.cpp (llama-server)", "Phase 11 managed local LLM", "llama-server", "--version")

	// Sandbox runner (Phase 7): report the pinned-vs-detected `sbx` version so an
	// unvetted upgrade is visible, then the sandbox-tools feature availability.
	runner := sandbox.NewCLIRunner(sandbox.Config{
		Path:                 cfg.Sandbox.SbxPath,
		AllowVersionMismatch: cfg.Sandbox.AllowVersionMismatch,
	})
	sb := runner.Status()
	switch {
	case !sb.Available:
		detail := "sbx not installed — Phase 7 sandbox runtime"
		if sb.Reason != "" {
			detail = sb.Reason
		}
		r.add("sbx (sandbox runtime)", "missing", detail)
	case sb.Reason != "":
		// Available but the detected version differs from the pinned one (opted in).
		r.add("sbx (sandbox runtime)", "warn", sb.Reason)
	default:
		r.add("sbx (sandbox runtime)", "ok", fmt.Sprintf("sbx %s (pinned %s) at %s", sb.Version, sb.Pinned, sb.Path))
	}
	switch {
	case runtime.GOOS == "windows":
		r.add("sandbox tools", "unavailable", "gated to Phase 12 on Windows (owner-only ACL/DPAPI not yet enforced)")
	case !cfg.Sandbox.Enabled:
		r.add("sandbox tools", "unavailable", "disabled: set [sandbox] enabled=true (needs a pinned sbx install)")
	case !sb.Available:
		r.add("sandbox tools", "unavailable", "sbx runtime not available — install/pin sbx")
	default:
		// Run the pinned command-line smoke test so an unvetted/drifted sbx surfaces
		// here rather than at the first tool call.
		if err := runner.Smoke(ctx); err != nil {
			r.add("sandbox tools", "warn", "sbx command-line smoke test failed: "+firstLine(err.Error()))
		} else {
			r.add("sandbox tools", "ok", "shell/file/HTTP tool calls run inside per-user sbx sandboxes (smoke passed)")
		}
	}

	// WebRTC voice bridge — pure Go (Pion), so always available with no native
	// toolchain. Hands-on browser loopback is validated in Phase 12.
	r.add("webrtc voice bridge (pion)", "ok", "pure-Go media bridge; no native toolchain")

	// HTTPS / LAN. Browser mic capture (getUserMedia) requires a secure context:
	// localhost is exempt, but a second LAN device needs HTTPS with a real cert.
	if cfg.Server.TLSEnabled() {
		r.add("https cert", "ok", cfg.Server.TLSCert)
	} else {
		r.add("https cert", "unavailable", "no cert configured (localhost mic is fine; LAN mic needs HTTPS — see mkcert/reverse-proxy guidance)")
	}

	// Shared ONNX runtime + local TTS/ASR (Phases 4–5). In the default CGo-free
	// build the runtime is the stub (unavailable); the onnx-tagged build links ORT
	// and loads it from the app-managed lib dir. TTS and ASR share one asset root
	// but each verifies ONLY its own models, so a TTS-only (or ASR-only) install is
	// reported correctly rather than dragged down by the other engine's assets.
	root := cfg.AssetsRoot(paths.Cache)
	// ORTAsset's bool is "supported" (true on linux/amd64 + darwin/arm64); invert
	// it for the unsupported-platform branch.
	_, ortSupported := assets.ORTAsset(runtime.GOOS, runtime.GOARCH)
	ortUnsupported := !ortSupported
	// Make the root owner-private AND verify the ORT library's checksum BEFORE
	// constructing the backend, so this command never dlopens a stale/corrupted/
	// swapped native library from a writable-by-others location, and load the EXACT
	// verified path (not a dir search).
	var info onnx.Info
	if secureErr := assets.SecureRoot(root); secureErr != nil {
		info = onnx.Info{Available: false, Reason: "asset root not owner-private: " + secureErr.Error()}
	} else if ortOK, ortReason := assets.ORTVerified(root, runtime.GOOS, runtime.GOARCH); ortOK {
		backend, _ := onnx.New(onnx.Config{LibFile: assets.ORTLibPath(root, runtime.GOOS, runtime.GOARCH)})
		info = backend.Info()
		_ = backend.Close()
	} else {
		info = onnx.Info{Available: false, Reason: ortReason}
	}
	if info.Available {
		r.add("onnx runtime", "ok", fmt.Sprintf("ORT %s (%s) — %s", info.Version, info.Provider, info.LibPath))
	} else {
		reason := info.Reason
		if reason == "" {
			reason = "not linked"
		}
		r.add("onnx runtime", "unavailable", reason)
	}

	// Local TTS (Phase 4) — gated on the ORT runtime + the Supertonic assets ONLY.
	ttsOK, ttsReason := assets.SupertonicVerified(root)
	switch {
	case ortUnsupported:
		r.add("local tts (supertonic)", "unavailable", "no ONNX Runtime build for this platform (Windows local voice gated to Phase 12)")
	case !cfg.TTS.Enabled:
		r.add("local tts (supertonic)", "unavailable", "disabled: set [tts] enabled=true, build with -tags onnx, and run 'hina assets pull'")
	case !info.Available:
		r.add("local tts (supertonic)", "unavailable", "onnx runtime not linked (build with -tags onnx)")
	case !ttsOK:
		r.add("local tts (supertonic)", "unavailable", "model assets not installed (run: hina assets pull): "+ttsReason)
	default:
		r.add("local tts (supertonic)", "ok", "Supertonic models installed; runtime linked")
	}

	// Local streaming ASR (Phase 5, Nemotron). Same shared ONNX runtime; gated on
	// the ORT runtime + the Nemotron assets ONLY. Windows stays gated to Phase 12.
	asrOK, asrReason := assets.ASRVerified(root)
	switch {
	case ortUnsupported:
		r.add("local asr (nemotron)", "unavailable", "no ONNX Runtime build for this platform (Windows local voice gated to Phase 12)")
	case !cfg.ASR.Enabled:
		r.add("local asr (nemotron)", "unavailable", "disabled: set [asr] enabled=true, build with -tags onnx, and run 'hina assets pull'")
	case !info.Available:
		r.add("local asr (nemotron)", "unavailable", "onnx runtime not linked (build with -tags onnx)")
	case !asrOK:
		r.add("local asr (nemotron)", "unavailable", "model assets not installed (run: hina assets pull): "+asrReason)
	default:
		r.add("local asr (nemotron)", "ok", "Nemotron models installed; runtime linked")
	}

	// Live voice (Phase 6) — the continuous VAD->ASR->agent->TTS loop. Gated on the
	// Silero VAD model AND that local ASR + TTS can run (it needs all three). Windows
	// stays gated to Phase 12.
	vadOK, vadReason := assets.VADVerified(root)
	switch {
	case ortUnsupported:
		r.add("live voice (vad)", "unavailable", "no ONNX Runtime build for this platform (Windows local voice gated to Phase 12)")
	case !cfg.Voice.Enabled:
		r.add("live voice (vad)", "unavailable", "disabled: set [voice] enabled=true (also needs [tts]+[asr]), build with -tags onnx, and run 'hina assets pull'")
	case !info.Available:
		r.add("live voice (vad)", "unavailable", "onnx runtime not linked (build with -tags onnx)")
	case !vadOK:
		r.add("live voice (vad)", "unavailable", "Silero VAD model not installed (run: hina assets pull): "+vadReason)
	case !cfg.TTS.Enabled || !cfg.ASR.Enabled:
		r.add("live voice (vad)", "unavailable", "the live loop also needs [tts] and [asr] enabled")
	default:
		r.add("live voice (vad)", "ok", "Silero VAD installed; runtime linked (live loop ready)")
	}

	return r
}

func (r *Report) add(name, status, detail string) {
	r.Checks = append(r.Checks, Check{Name: name, Status: status, Detail: detail})
}

func (r *Report) addTool(ctx context.Context, name, purpose, bin string, args ...string) {
	path, err := platform.LookPath(bin)
	if err != nil {
		r.add(name, "missing", "not installed — "+purpose)
		return
	}
	out, err := platform.Output(ctx, path, args...)
	if err != nil {
		r.add(name, "ok", path)
		return
	}
	r.add(name, "ok", firstLine(string(out)))
}

func firstLine(s string) string {
	return strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
}

func tier() string {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64", "darwin/arm64":
		return "Tier 1"
	case "windows/amd64":
		return "Tier 1 (built; hands-on validation pending — Phase 12)"
	default:
		return "Tier 2 / unvalidated"
	}
}
