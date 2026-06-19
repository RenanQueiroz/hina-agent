package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"time"

	"github.com/RenanQueiroz/hina-agent/internal/auth"
	"github.com/RenanQueiroz/hina-agent/internal/events"
	"github.com/RenanQueiroz/hina-agent/internal/httpapi"
	"github.com/RenanQueiroz/hina-agent/internal/platform"
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

	bus := events.NewBus(a.store)
	am := auth.NewManager(a.store, a.cfg.Server.TLSEnabled())
	srv := httpapi.New(a.cfg, a.store, bus, am, a.log)
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
