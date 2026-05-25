package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("inkyframe-server: ")

	configPath := flag.String("config", "config.yaml", "path to the YAML config file")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}

	apps, err := buildApps(cfg)
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Build every app once synchronously so misconfiguration/connectivity
	// issues surface immediately and the first request is served fast. If any
	// app fails to build we exit non-zero rather than start half-broken.
	for _, app := range apps {
		log.Printf("building initial image for %q...", app.Name())
		if err := app.Refresh(ctx); err != nil {
			log.Fatalf("initial build for %q failed: %v", app.Name(), err)
		}
	}
	log.Print("all initial images ready")

	// Apps don't refresh on a timer. Each serve returns the cached image and
	// then triggers a background regeneration on that app (via the manager), so
	// the next serve shows a fresh image. ctx bounds those background builds.
	mgr := newManager(ctx, apps, cfg.Rotate)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /image", mgr.handleImage)
	mux.HandleFunc("GET /{$}", mgr.handleImage) // root alias for /image
	mux.HandleFunc("GET /next-app", mgr.handleNextApp)
	mux.HandleFunc("GET /prev-app", mgr.handlePrevApp)
	mux.HandleFunc("GET /next-image", mgr.handleNextImage)
	mux.HandleFunc("GET /prev-image", mgr.handlePrevImage)
	mux.HandleFunc("GET /current-image", mgr.handleCurrentImage)
	mux.HandleFunc("GET /toggle-rotate", mgr.handleToggleRotate)
	mux.HandleFunc("GET /healthz", mgr.handleHealth)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("listening on %s (%d apps, rotate=%v)", cfg.ListenAddr, len(apps), cfg.Rotate)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	log.Print("shutdown complete")
}
