// Command relay is the whatsmeow-backed WhatsApp relay.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"yourwhatsapp/relay/internal/api"
	"yourwhatsapp/relay/internal/config"
	"yourwhatsapp/relay/internal/outbox"
	"yourwhatsapp/relay/internal/session"
	"yourwhatsapp/relay/internal/store"
)

const (
	httpShutdownTimeout    = 10 * time.Second
	sessionShutdownTimeout = 15 * time.Second
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(runHealthcheck())
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("relay failed", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DataDir)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	workerCtx, cancelWorker := context.WithCancel(context.Background())
	workerDone := make(chan struct{})
	worker := outbox.NewWorker(st, outbox.NewFrappePoster(cfg.FrappeBaseURL, cfg.FrappeSiteName, cfg.APIKey), log)
	go func() {
		defer close(workerDone)
		worker.Run(workerCtx)
	}()

	enqueue := func(ctx context.Context, method string, payload any) error {
		err := st.EnqueueCallback(ctx, method, payload)
		if err == nil {
			worker.Notify()
		}
		return err
	}
	manager := session.NewManager(st, enqueue, session.NewWhatsmeowClient, cfg, log)
	if err := manager.RestoreAll(ctx); err != nil {
		log.Error("restore sessions", "error", err)
	}

	srv := &http.Server{
		Addr:              net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		Handler:           api.NewHandler(manager, st, cfg.APIKey, log),
		ReadHeaderTimeout: 10 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()
	log.Info("relay listening", "addr", srv.Addr)

	select {
	case err := <-serveErr:
		stop()
		shutdown(log, srv, manager, cancelWorker, workerDone)
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}
	log.Info("shutdown signal received")
	shutdown(log, srv, manager, cancelWorker, workerDone)
	log.Info("shutdown complete")
	return nil
}

func shutdown(log *slog.Logger, srv *http.Server, m *session.Manager, cancelWorker context.CancelFunc, workerDone <-chan struct{}) {
	httpCtx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(httpCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("http shutdown", "error", err)
	}
	sessCtx, cancelSess := context.WithTimeout(context.Background(), sessionShutdownTimeout)
	defer cancelSess()
	if err := m.Shutdown(sessCtx); err != nil {
		log.Error("session shutdown", "error", err)
	}
	cancelWorker()
	<-workerDone
}
