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
	"github.com/RenanQueiroz/hina-agent/internal/config"
	"github.com/RenanQueiroz/hina-agent/internal/events"
	"github.com/RenanQueiroz/hina-agent/internal/httpapi"
	"github.com/RenanQueiroz/hina-agent/internal/llm"
	"github.com/RenanQueiroz/hina-agent/internal/onnx"
	"github.com/RenanQueiroz/hina-agent/internal/platform"
	"github.com/RenanQueiroz/hina-agent/internal/rtc"
	"github.com/RenanQueiroz/hina-agent/internal/tts"
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
	if _, err := platform.LoadOrCreateMasterKey(a.paths.MasterKeyPath()); err != nil {
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

	// WebRTC voice bridge (Phase 3). The bus is its SSE event sink so live
	// audio/lifecycle events are observable on the conversation stream.
	rtcMgr, err := rtc.NewManager(rtc.Config{ICEServers: iceServers(a.cfg.Realtime.ICEServers), TTS: ttsEngine, ASR: asrEngine, Log: a.log}, bus)
	if err != nil {
		return err
	}
	defer rtcMgr.Close()

	srv := httpapi.New(a.cfg, a.store, bus, am, provider, a.logs, a.log)
	srv.SetRealtime(rtcMgr)
	srv.SetTTS(ttsEngine)
	srv.SetASR(asrEngine)
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
		a.log.Warn("tts: local voice is gated to Phase 11 on Windows; leaving it disabled")
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
		a.log.Warn("asr: local voice is gated to Phase 11 on Windows; leaving it disabled")
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
