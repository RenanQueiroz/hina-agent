package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/auth"
	"github.com/RenanQueiroz/hina-agent/internal/config"
	"github.com/RenanQueiroz/hina-agent/internal/events"
	"github.com/RenanQueiroz/hina-agent/internal/httpapi"
	"github.com/RenanQueiroz/hina-agent/internal/llm"
	"github.com/RenanQueiroz/hina-agent/internal/platform"
	"github.com/RenanQueiroz/hina-agent/internal/rtc"
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

	// WebRTC voice bridge (Phase 3). The bus is its SSE event sink so live
	// audio/lifecycle events are observable on the conversation stream.
	rtcMgr, err := rtc.NewManager(rtc.Config{ICEServers: iceServers(a.cfg.Realtime.ICEServers), Log: a.log}, bus)
	if err != nil {
		return err
	}
	defer rtcMgr.Close()

	srv := httpapi.New(a.cfg, a.store, bus, am, provider, a.logs, a.log)
	srv.SetRealtime(rtcMgr)
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
