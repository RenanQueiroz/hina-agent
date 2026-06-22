package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"runtime"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/asr"
	"github.com/RenanQueiroz/hina-agent/internal/assets"
	"github.com/RenanQueiroz/hina-agent/internal/auth"
	"github.com/RenanQueiroz/hina-agent/internal/automation"
	"github.com/RenanQueiroz/hina-agent/internal/autorun"
	"github.com/RenanQueiroz/hina-agent/internal/config"
	"github.com/RenanQueiroz/hina-agent/internal/events"
	"github.com/RenanQueiroz/hina-agent/internal/httpapi"
	"github.com/RenanQueiroz/hina-agent/internal/llm"
	"github.com/RenanQueiroz/hina-agent/internal/onnx"
	"github.com/RenanQueiroz/hina-agent/internal/platform"
	"github.com/RenanQueiroz/hina-agent/internal/rtc"
	"github.com/RenanQueiroz/hina-agent/internal/sandbox"
	"github.com/RenanQueiroz/hina-agent/internal/tts"
	"github.com/RenanQueiroz/hina-agent/internal/vad"
	"github.com/RenanQueiroz/hina-agent/internal/vault"
	"github.com/pion/webrtc/v4"
)

func cmdServer(args []string) error {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	host := fs.String("host", "", "override bind host")
	port := fs.Int("port", 0, "override bind port")
	_ = fs.Parse(args)

	a, err := openApp()
	if err != nil {
		return err
	}
	defer a.close()

	if *host != "" {
		a.cfg.Server.Host = *host
	}
	if *port != 0 {
		a.cfg.Server.Port = *port
	}
	if err := a.cfg.Validate(); err != nil {
		return err
	}

	ctx := context.Background()
	if _, err := a.store.Migrate(ctx); err != nil {
		return err
	}
	if err := ensureMasterKey(a); err != nil {
		return err
	}
	res, err := auth.EnsureAdmin(ctx, a.store)
	if err != nil {
		return err
	}
	if res.Created {
		printBootstrapCredential(res)
	}

	// LAN gate: refuse a non-loopback bind until the bootstrap password is changed.
	if !a.cfg.Server.IsLoopbackBind() {
		allowed, err := auth.LANAllowed(ctx, a.store)
		if err != nil {
			return err
		}
		if !allowed {
			return fmt.Errorf("refusing to bind non-loopback host %q: change the bootstrap admin password first", a.cfg.Server.Host)
		}
	}

	provider, err := llm.Build(llm.Config{
		Provider:     a.cfg.LLM.Provider,
		Model:        a.cfg.LLM.Model,
		BaseURL:      a.cfg.LLM.BaseURL,
		APIKey:       a.cfg.LLM.APIKey,
		SystemPrompt: a.cfg.LLM.SystemPrompt,
	})
	if err != nil {
		return err
	}
	a.log.Info("llm provider", "provider", provider.Name(), "model", a.cfg.LLM.Model)

	bus := events.NewBus(a.store)
	am := auth.NewManager(a.store, a.cfg.Server.TLSEnabled())

	// Local TTS engine (Phase 4). Off unless [tts] enabled; even then it reports
	// itself unavailable in the default CGo-free build (the onnx runtime isn't
	// linked) or when the model assets aren't installed. The bus is its event sink
	// so RuntimeModel*/load events are observable.
	ttsEngine := buildTTS(a, bus)
	if ttsEngine != nil {
		defer ttsEngine.Close()
	}

	// Local ASR engine (Phase 5). Off unless [asr] enabled; reports itself
	// unavailable in the default CGo-free build or without installed assets. The
	// bus is its event sink for RuntimeModel*/load observability.
	asrEngine := buildASR(a, bus)
	if asrEngine != nil {
		defer asrEngine.Close()
	}

	// Local VAD engine (Phase 6). Off unless [voice] enabled; reports itself
	// unavailable in the default CGo-free build or without the installed Silero
	// model. Drives the live conversation loop's turn detection.
	vadEngine := buildVAD(a, bus)
	if vadEngine != nil {
		defer vadEngine.Close()
	}

	// The HTTP server is built before the WebRTC manager because it implements the
	// rtc.AgentService (the live-voice loop's turn-runner), which the manager needs.
	srv := httpapi.New(a.cfg, a.store, bus, am, provider, a.logs, a.log)

	// WebRTC voice bridge (Phase 3). The bus is its SSE event sink so live
	// audio/lifecycle events are observable on the conversation stream. VAD + the
	// server's agent turn-runner power the Phase 6 live conversation loop.
	rtcCfg := rtc.Config{ICEServers: iceServers(a.cfg.Realtime.ICEServers), TTS: ttsEngine, ASR: asrEngine, Log: a.log}
	if a.cfg.Voice.Enabled && vadEngine != nil {
		rtcCfg.VAD = vadEngine
		rtcCfg.Agent = srv
	}
	rtcMgr, err := rtc.NewManager(rtcCfg, bus)
	if err != nil {
		return err
	}
	defer rtcMgr.Close()

	srv.SetRealtime(rtcMgr)
	srv.SetTTS(ttsEngine)
	srv.SetASR(asrEngine)
	if vadEngine != nil {
		srv.SetVAD(vadEngine)
	}

	// Phase 7 sandbox + secret vault. The vault + workspace manager are always
	// built (they back per-user secret/environment management even when tool
	// execution is off); the sbx runner reports itself unavailable when sbx isn't
	// installed. SetSandbox builds the tool Router only when [sandbox] is enabled.
	sandboxRunner, workspaces := buildSandbox(a)
	vaultV := buildVault(a)
	srv.SetSandbox(vaultV, workspaces, sandboxRunner)
	// Phase 8 callable agents (Codex/Claude/Cursor/Pi) are built inside SetSandbox
	// (they reuse the sandbox Router + vault), gated on [agents].enabled + the sandbox
	// stack being present. Pi additionally needs the Phase 11 local endpoint.
	if a.cfg.Agents.Enabled {
		a.log.Info("callable agents", "enabled", true,
			"sandbox_enabled", a.cfg.Sandbox.Enabled,
			"network_isolated", a.cfg.Sandbox.NetworkIsolated,
			"pi_endpoint", a.cfg.Agents.LocalEndpoint != "")
	}

	// Phase 9 automations: the durable scheduler that runs user-owned workflows inside
	// sbx. Built only when [automations] is enabled AND the sandbox stack + vault are
	// present (runs execute in sbx and resolve granted secrets).
	autoSvc := buildAutomations(a, srv, sandboxRunner, workspaces, vaultV, provider)
	if autoSvc != nil {
		srv.SetAutomations(autoSvc)
	}

	srv.SetReady(true)

	httpSrv := &http.Server{
		Addr:              a.cfg.Server.Addr(),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Graceful shutdown on OS signals.
	sigCtx, stop := platform.ShutdownContext(context.Background())
	defer stop()
	go func() {
		<-sigCtx.Done()
		a.log.Info("shutting down")
		sc, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(sc)
	}()

	// Reap stale ephemeral sandbox scratch in the background until shutdown.
	if workspaces != nil {
		go workspaces.Janitor(sigCtx, 10*time.Minute, a.cfg.Sandbox.ScratchTTLOr(time.Hour))
	}

	// Start the automation scheduler (resumes enabled automations, recomputes next
	// runs). Stop it on shutdown so in-flight runs are cancelled + finalized cleanly —
	// nothing lingers. Stop BEFORE the HTTP server fully exits so run records persist.
	if autoSvc != nil {
		autoSvc.Start(sigCtx)
		defer autoSvc.Stop()
		a.log.Info("automations", "enabled", true, "tick", a.cfg.Automations.TickOr(5*time.Second).String())
	}

	scheme := "http"
	if a.cfg.Server.TLSEnabled() {
		scheme = "https"
	}
	a.log.Info("listening", "url", fmt.Sprintf("%s://%s", scheme, a.cfg.Server.Addr()))

	if a.cfg.Server.TLSEnabled() {
		err = httpSrv.ListenAndServeTLS(a.cfg.Server.TLSCert, a.cfg.Server.TLSKey)
	} else {
		err = httpSrv.ListenAndServe()
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// buildAutomations constructs the Phase 9 automation service + durable scheduler. It
// returns nil (logging why) unless [automations] is enabled AND the runtime it needs
// is present: a usable sbx runner (runs execute in the sandbox), the workspace
// manager (ephemeral run workspaces), and the vault (granted-secret resolution +
// redaction). The scheduler itself is started/stopped by the caller.
func buildAutomations(a *app, srv *httpapi.Server, runner sandbox.Runner, ws *sandbox.WorkspaceManager, v *vault.Vault, provider llm.Provider) *autorun.Service {
	if !a.cfg.Automations.Enabled {
		return nil
	}
	switch {
	case !a.cfg.Sandbox.Enabled || runner == nil || !runner.Available():
		a.log.Warn("automations: disabled — [automations] needs an available sbx sandbox ([sandbox] enabled + installed)")
		return nil
	case ws == nil:
		a.log.Warn("automations: disabled — the workspace manager is unavailable")
		return nil
	case v == nil:
		a.log.Warn("automations: disabled — the secret vault is unavailable (needed for redaction/granted secrets)")
		return nil
	}
	exec := autorun.ExecConfig{
		Runner:          runner,
		Secrets:         v,
		Provider:        provider,
		Audit:           a.store, // durable per-tool sandbox_runs audit (survives automation deletion)
		NetworkIsolated: a.cfg.Sandbox.NetworkIsolated,
		Limits: sandbox.Limits{
			CPUs:    a.cfg.Sandbox.CPUs,
			Memory:  a.cfg.Sandbox.Memory,
			PIDs:    a.cfg.Sandbox.PIDs,
			Timeout: a.cfg.Sandbox.TimeoutOr(5 * time.Minute),
		},
		Log: a.log,
	}
	// Avoid the typed-nil interface trap: only set Agents when the router truly exists.
	if ar := srv.AgentRouter(); ar != nil {
		exec.Agents = ar
	}
	concurrent := a.cfg.Automations.MaxConcurrentRuns
	if concurrent == 0 {
		concurrent = 16 // bounded by default so many due automations can't exhaust sbx
	}
	perUser := a.cfg.Automations.MaxRunsPerUser
	if perUser == 0 {
		perUser = 4
	}
	workspaceMB := a.cfg.Automations.MaxWorkspaceMB
	if workspaceMB == 0 {
		workspaceMB = 2048 // 2 GiB per-run scratch cap by default (watchdog kills overruns)
	}
	minFreeMB := a.cfg.Automations.MinFreeMB
	if minFreeMB == 0 {
		minFreeMB = 1024 // kill runs if the scratch filesystem drops below 1 GiB free (host-disk guard)
	}
	enabledPerUser := a.cfg.Automations.MaxEnabledPerUser
	if enabledPerUser == 0 {
		enabledPerUser = 100 // a user may have up to 100 automations enabled by default
	}
	svc := autorun.New(autorun.ServiceConfig{
		Store:             a.store,
		Exec:              exec,
		Workspaces:        ws,
		Caps:              automationCaps(a.cfg.Automations),
		ArtifactDir:       filepath.Join(a.paths.Data, "automation-artifacts"),
		Tick:              a.cfg.Automations.TickOr(5 * time.Second),
		Eligibility:       srv.AutomationEligibility,
		MaxConcurrentRuns: concurrent,
		MaxRunsPerUser:    perUser,
		MaxEnabledPerUser: enabledPerUser,
		MaxWorkspaceBytes: int64(workspaceMB) << 20,
		MinFreeBytes:      int64(minFreeMB) << 20,
		Log:               a.log,
	})
	return svc
}

// automationCaps maps the [automations] config ceilings onto the engine caps,
// applying sane defaults for any unset ceiling so a run is always bounded.
func automationCaps(c config.AutomationsConfig) automation.Caps {
	pick := func(v, def int) int {
		if v <= 0 {
			return def
		}
		return v
	}
	pick64 := func(v, def int64) int64 {
		if v <= 0 {
			return def
		}
		return v
	}
	return automation.Caps{
		Timeout:          c.MaxTimeoutOr(30 * time.Minute),
		MaxModelCalls:    pick(c.MaxModelCalls, 50),
		MaxAgentRuns:     pick(c.MaxAgentRuns, 16),
		MaxToolCalls:     pick(c.MaxToolCalls, 200),
		MaxLogBytes:      pick64(c.MaxLogBytes, 10<<20),
		MaxArtifactBytes: pick64(c.MaxArtifactByt, 50<<20),
		MaxParallelism:   pick(c.MaxParallelism, 8),
	}
}

// buildVault constructs the per-user secret vault, loading (or creating) the
// local master key through internal/platform. It returns nil (logging why) when
// the key or vault can't be initialized, so the server still runs with secret
// management disabled rather than failing to start.
func buildVault(a *app) *vault.Vault {
	// Windows owner-only ACL / DPAPI master-key protection is a no-op until Phase 12
	// (internal/platform), so the vault's on-disk boundary is not yet enforced there
	// — gate it off like local voice rather than store secrets unprotected.
	if runtime.GOOS == "windows" {
		a.log.Warn("vault: per-user secret vault is gated to Phase 12 on Windows (owner-only ACL/DPAPI not yet enforced); disabled")
		return nil
	}
	key, err := platform.LoadOrCreateMasterKey(a.paths.MasterKeyPath())
	if err != nil {
		a.log.Warn("vault: master key unavailable; secret vault disabled", "err", err)
		return nil
	}
	v, err := vault.New(key, filepath.Join(a.paths.Data, "vault"), a.store)
	if err != nil {
		a.log.Warn("vault: init failed; secret vault disabled", "err", err)
		return nil
	}
	return v
}

// buildSandbox constructs the workspace manager and the sbx runner. Neither
// requires [sandbox] enabled (the runner just reports unavailable when sbx is
// absent, and the workspace manager backs storage); SetSandbox decides whether to
// build the tool Router. Returns a nil workspace manager only if its roots can't
// be created.
func buildSandbox(a *app) (sandbox.Runner, *sandbox.WorkspaceManager) {
	ws, err := sandbox.NewWorkspaceManager(a.paths.Data, a.paths.Runtime, a.log)
	if err != nil {
		a.log.Warn("sandbox: workspace manager init failed; sandbox storage disabled", "err", err)
		ws = nil
	}
	// Do NOT probe an external sbx binary at startup for a DISABLED, opt-in feature.
	// `hina doctor` reports sbx availability separately; the server only resolves/
	// version-probes/smoke-tests sbx when [sandbox] is enabled.
	if !a.cfg.Sandbox.Enabled {
		a.log.Info("sandbox: tools disabled ([sandbox] enabled=false); not probing sbx")
		return nil, ws
	}
	runner := sandbox.NewCLIRunner(sandbox.Config{
		Path:                 a.cfg.Sandbox.SbxPath,
		Kit:                  a.cfg.Sandbox.Kit,
		OutputDir:            filepath.Join(a.paths.Runtime, "sandbox-output"),
		AllowVersionMismatch: a.cfg.Sandbox.AllowVersionMismatch,
		Defaults: sandbox.Limits{
			CPUs:    a.cfg.Sandbox.CPUs,
			Memory:  a.cfg.Sandbox.Memory,
			PIDs:    a.cfg.Sandbox.PIDs,
			Timeout: a.cfg.Sandbox.TimeoutOr(5 * time.Minute),
		},
		Log: a.log,
	})
	switch {
	case runtime.GOOS == "windows":
		// Workspace/capture owner-only ACLs are a Phase 12 no-op on Windows, so the
		// per-user boundary isn't enforced — gate tools off until then.
		runner.MarkUnavailable("sandbox tools gated to Phase 12 on Windows (owner-only ACL not yet enforced)")
		a.log.Warn("sandbox: tools gated to Phase 12 on Windows; disabled")
	case runner.Available():
		// Fail closed: run the pinned command-line smoke test before trusting sbx,
		// so a drifted CLI can't silently run production tool calls.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := runner.Smoke(ctx); err != nil {
			runner.MarkUnavailable("sbx command-line smoke test failed: " + err.Error())
			a.log.Warn("sandbox: smoke test failed; tools disabled", "err", err)
		}
		cancel()
	}
	st := runner.Status()
	a.log.Info("sandbox runner", "enabled", a.cfg.Sandbox.Enabled,
		"available", st.Available, "version", st.Version, "reason", st.Reason)
	return runner, ws
}

// buildTTS constructs the local TTS engine when [tts] is enabled, wiring the
// shared ONNX runtime (from the app-managed lib dir) and the installed Supertonic
// assets. It returns nil when TTS is disabled; when enabled but unbuilt (default
// CGo-free build) or uninstalled, the returned engine reports itself unavailable
// and SpeakText requests are rejected at runtime.
func buildTTS(a *app, sink tts.EventSink) tts.Engine {
	if !a.cfg.TTS.Enabled {
		return nil
	}
	if assets.WindowsLocalVoiceGated && runtime.GOOS == "windows" {
		a.log.Warn("tts: local voice is gated to Phase 12 on Windows; leaving it disabled")
		return nil
	}
	root := assetsRoot(a.cfg, a.paths)
	// Make the asset root owner-only BEFORE verifying/loading: the ORT library is
	// dlopen'd (native code) and that verify->dlopen step is only safe if no other
	// local principal can swap files. The default cache dir is already private, but
	// a custom [tts] assets_dir bypasses that — so secure it (0700) or fail closed
	// if another user owns it.
	if !secureAssetRoot(root, a.log) {
		return nil
	}
	// Verify the ORT native library AND every Supertonic model/config/voice by
	// checksum on disk BEFORE onnx.New dlopens the runtime or the engine opens a
	// graph — so a stale, corrupted, or swapped asset is never loaded, and TTS
	// fails closed (disabled) rather than reporting available and then misbehaving.
	// Only the TTS subset is required (an ASR-only install shouldn't block TTS and
	// vice-versa), even though one `hina assets pull` installs both.
	if ok, reason := assets.ORTVerified(root, runtime.GOOS, runtime.GOARCH); !ok {
		a.log.Warn("tts: ONNX Runtime not verified; local TTS disabled (run: hina assets pull)", "reason", reason)
		return nil
	}
	if ok, reason := assets.SupertonicVerified(root); !ok {
		a.log.Warn("tts: Supertonic assets failed verification; local TTS disabled (run: hina assets pull)", "reason", reason)
		return nil
	}
	_, onnxDir, voiceDir := assets.Layout(root)
	// Load EXACTLY the verified library path (not a dir search), so the loaded
	// native code is the file whose checksum we just confirmed.
	libFile := assets.ORTLibPath(root, runtime.GOOS, runtime.GOARCH)
	backend, err := onnx.New(onnx.Config{LibFile: libFile, IntraOpThreads: a.cfg.TTS.Threads})
	if err != nil {
		a.log.Warn("tts: onnx backend init failed", "err", err)
	}
	engine := tts.NewSynthesizer(tts.Config{
		Backend:  backend,
		OnnxDir:  onnxDir,
		VoiceDir: voiceDir,
		IdleTTL:  a.cfg.TTS.IdleTTLOr(5 * time.Minute),
		Defaults: tts.Options{Voice: a.cfg.TTS.Voice, Lang: a.cfg.TTS.Lang, Speed: a.cfg.TTS.Speed, Steps: a.cfg.TTS.Steps},
		Sink:     sink,
		Log:      a.log,
		// Load ONNX graphs/config and voices from CHECKSUM-VERIFIED bytes, so an
		// asset tampered with after startup is never fed to ORT (the verified bytes
		// are exactly the bytes loaded — no reopen window).
		ReadAsset: func(file string) ([]byte, error) {
			return assets.ReadVerified(root, filepath.Join("supertonic", "onnx", file))
		},
		ReadVoiceAsset: func(id string) ([]byte, error) {
			return assets.ReadVerified(root, filepath.Join("supertonic", "voice_styles", id+".json"))
		},
	})
	st := engine.Status()
	a.log.Info("tts engine", "available", st.Available, "reason", st.Reason, "ort", st.Runtime.Version)
	return engine
}

// buildASR constructs the local streaming ASR engine when [asr] is enabled,
// wiring the shared ONNX runtime and the installed Nemotron assets. It mirrors
// buildTTS: nil when disabled; an unavailable engine (default CGo-free build, or
// uninstalled/unverified assets) when it can't run, so ListenStarted is rejected
// at runtime. The encoder is loaded by its verified PATH (it has external weights
// ORT resolves on disk); the decoder + tokenizer load from verified bytes.
func buildASR(a *app, sink asr.EventSink) asr.Engine {
	if !a.cfg.ASR.Enabled {
		return nil
	}
	if assets.WindowsLocalVoiceGated && runtime.GOOS == "windows" {
		a.log.Warn("asr: local voice is gated to Phase 12 on Windows; leaving it disabled")
		return nil
	}
	root := assetsRoot(a.cfg, a.paths)
	if !secureAssetRoot(root, a.log) {
		return nil
	}
	if ok, reason := assets.ORTVerified(root, runtime.GOOS, runtime.GOARCH); !ok {
		a.log.Warn("asr: ONNX Runtime not verified; local ASR disabled (run: hina assets pull)", "reason", reason)
		return nil
	}
	if ok, reason := assets.ASRVerified(root); !ok {
		a.log.Warn("asr: Nemotron assets failed verification; local ASR disabled (run: hina assets pull)", "reason", reason)
		return nil
	}
	libFile := assets.ORTLibPath(root, runtime.GOOS, runtime.GOARCH)
	backend, err := onnx.New(onnx.Config{LibFile: libFile, IntraOpThreads: a.cfg.ASR.Threads})
	if err != nil {
		a.log.Warn("asr: onnx backend init failed", "err", err)
	}
	engine := asr.NewRecognizer(asr.Config{
		Backend:     backend,
		ModelDir:    assets.ASRDir(root),
		EncoderPath: assets.ASREncoderPath(root),
		IdleTTL:     a.cfg.ASR.IdleTTLOr(5 * time.Minute),
		Defaults:    asr.Options{Language: a.cfg.ASR.Language},
		Agent: asr.AgentBias{
			Name:         a.cfg.Agent.Name,
			Aliases:      a.cfg.Agent.NameAliases,
			ContextScore: a.cfg.ASR.ContextScore,
			DepthScaling: a.cfg.ASR.DepthScaling,
		},
		Sink: sink,
		Log:  a.log,
		// Load the self-contained decoder + tokenizer from CHECKSUM-VERIFIED bytes.
		ReadDecoder: func() ([]byte, error) {
			return assets.ReadVerified(root, filepath.Join("nemotron", "decoder_joint.onnx"))
		},
		ReadTokenizer: func() ([]byte, error) {
			return assets.ReadVerified(root, filepath.Join("nemotron", "tokenizer.model"))
		},
	})
	st := engine.Status()
	a.log.Info("asr engine", "available", st.Available, "reason", st.Reason, "biasing", st.Biasing, "ort", st.Runtime.Version)
	return engine
}

// buildVAD constructs the local Silero VAD engine when [voice] is enabled, wiring
// the shared ONNX runtime and the installed Silero model. It mirrors buildASR: nil
// when disabled; an unavailable engine (default CGo-free build, or uninstalled/
// unverified model) when it can't run, so the live loop is rejected at runtime. The
// small self-contained model loads from verified bytes (no external data).
func buildVAD(a *app, sink vad.EventSink) *vad.Engine {
	if !a.cfg.Voice.Enabled {
		return nil
	}
	if assets.WindowsLocalVoiceGated && runtime.GOOS == "windows" {
		a.log.Warn("voice: local voice is gated to Phase 12 on Windows; leaving the live loop disabled")
		return nil
	}
	root := assetsRoot(a.cfg, a.paths)
	if !secureAssetRoot(root, a.log) {
		return nil
	}
	if ok, reason := assets.ORTVerified(root, runtime.GOOS, runtime.GOARCH); !ok {
		a.log.Warn("voice: ONNX Runtime not verified; live voice disabled (run: hina assets pull)", "reason", reason)
		return nil
	}
	if ok, reason := assets.VADVerified(root); !ok {
		a.log.Warn("voice: Silero VAD model failed verification; live voice disabled (run: hina assets pull)", "reason", reason)
		return nil
	}
	libFile := assets.ORTLibPath(root, runtime.GOOS, runtime.GOARCH)
	backend, err := onnx.New(onnx.Config{LibFile: libFile, IntraOpThreads: a.cfg.ASR.Threads})
	if err != nil {
		a.log.Warn("voice: onnx backend init failed", "err", err)
	}
	engine := vad.NewEngine(vad.Config{
		Backend: backend,
		ReadModel: func() ([]byte, error) {
			return assets.ReadVerified(root, filepath.Join("vad", "silero_vad.onnx"))
		},
		IdleTTL: a.cfg.Voice.IdleTTLOr(5 * time.Minute),
		Params:  voiceVADParams(a.cfg.Voice),
		Sink:    sink,
		Log:     a.log,
	})
	st := engine.Status()
	a.log.Info("vad engine", "available", st.Available, "reason", st.Reason, "ort", st.Runtime.Version)
	return engine
}

// voiceVADParams maps the [voice] config to the VAD engine's default tunables (0
// fields fall back to the engine defaults; the client's per-session turn_detection
// overrides these).
func voiceVADParams(c config.VoiceConfig) vad.Params {
	p := vad.Params{Threshold: c.Threshold}
	if c.SilenceMs > 0 {
		p.MinSilence = time.Duration(c.SilenceMs) * time.Millisecond
	}
	if c.PreSpeechMs > 0 {
		p.PreSpeech = time.Duration(c.PreSpeechMs) * time.Millisecond
	}
	if c.MinSpeechMs > 0 {
		p.MinSpeech = time.Duration(c.MinSpeechMs) * time.Millisecond
	}
	if c.MaxDurationS > 0 {
		p.MaxDuration = time.Duration(c.MaxDurationS) * time.Second
	}
	return p
}

// secureAssetRoot makes the local-inference asset root owner-private (0700 on
// Unix) so no other local principal can swap an asset in the verify->load window,
// then confirms it. It returns false (logging why) if Hina can't secure it — e.g.
// the directory is owned by another user — so the engine fails closed rather than
// loading native code from a writable-by-others location.
func secureAssetRoot(root string, log *slog.Logger) bool {
	if err := assets.SecureRoot(root); err != nil {
		log.Warn("local-inference: cannot secure the asset root; disabled", "root", root, "err", err)
		return false
	}
	return true
}

// iceServers maps configured ICE servers to Pion's ICEServer list, carrying
// TURN credentials when present. Empty input yields no servers (host candidates
// only — fine for localhost/LAN). Config validation has already ensured any
// turn:/turns: entry has credentials.
func iceServers(servers []config.ICEServer) []webrtc.ICEServer {
	if len(servers) == 0 {
		return nil
	}
	out := make([]webrtc.ICEServer, 0, len(servers))
	for _, s := range servers {
		ice := webrtc.ICEServer{URLs: s.URLs}
		if s.Username != "" {
			ice.Username = s.Username
			ice.Credential = s.Credential
			ice.CredentialType = webrtc.ICECredentialTypePassword
		}
		out = append(out, ice)
	}
	return out
}
