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
	"yourwhatsapp/relay/internal/claim"
	"yourwhatsapp/relay/internal/config"
	"yourwhatsapp/relay/internal/outbox"
	"yourwhatsapp/relay/internal/session"
	"yourwhatsapp/relay/internal/store"
)

const (
	httpShutdownTimeout    = 10 * time.Second
	sessionShutdownTimeout = 15 * time.Second
	// lostSendGrace gives restored sessions time to reconnect before the
	// sends lost in the restart are failed over.
	lostSendGrace = 30 * time.Second
)

// version is stamped at build time: -ldflags "-X main.version=...".
var version = "dev"

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

	cs, err := claim.New(st, cfg, time.Now)
	if err != nil {
		return fmt.Errorf("load relay identity: %w", err)
	}
	log.Info("relay identity", "mode", cs.Mode(), "version", version)

	workerCtx, cancelWorker := context.WithCancel(context.Background())
	workerDone := make(chan struct{})
	worker := outbox.NewWorker(st, outbox.NewFrappePoster(cs.Target), log)
	claimed := claimedSignal(cs)
	go func() {
		defer close(workerDone)
		select {
		case <-claimed:
			worker.Run(workerCtx)
		case <-workerCtx.Done():
		}
	}()

	enqueue := func(ctx context.Context, method string, payload any) error {
		err := st.EnqueueCallback(ctx, method, payload)
		if err == nil {
			worker.Notify()
		}
		return err
	}
	manager := session.NewManager(st, enqueue, session.NewWhatsmeowClient, cfg, log)
	restoreWhenClaimed(ctx, cs, manager, log)

	srv := &http.Server{
		Addr:              net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		Handler:           api.NewHandler(manager, st, cs, log, version),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		shutdown(log, srv, manager, cancelWorker, workerDone)
		return fmt.Errorf("listen: %w", err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
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

// restorer is the part of *session.Manager that brings sessions back at boot.
type restorer interface {
	RestoreAll(ctx context.Context) error
	FailLostSendsAfter(grace time.Duration)
}

// restoreWhenClaimed restores sessions now when the relay has an identity
// (before it serves, as always), or in the background once POST /claim succeeds.
func restoreWhenClaimed(ctx context.Context, cs *claim.State, m restorer, log *slog.Logger) {
	if cs.Mode() != claim.ModeUnclaimed {
		restoreSessions(ctx, m, log)
		return
	}
	log.Info("relay unclaimed: no sessions or callbacks until POST /claim")
	cs.OnClaimed(func() { go restoreSessions(ctx, m, log) })
}

// restoreSessions restarts every paired session and arms the lost-send
// failover, whose grace gives the restored sessions time to reconnect.
// A claim that lands during shutdown restores nothing.
func restoreSessions(ctx context.Context, m restorer, log *slog.Logger) {
	if ctx.Err() != nil {
		return
	}
	if err := m.RestoreAll(ctx); err != nil {
		log.Error("restore sessions", "error", err)
	}
	m.FailLostSendsAfter(lostSendGrace)
}

// claimedSignal is closed once the relay has an identity: at once in env or
// claimed mode, else after POST /claim. Registered before the server listens,
// so no claim can slip between the mode check and the hook.
func claimedSignal(cs *claim.State) <-chan struct{} {
	ch := make(chan struct{})
	if cs.Mode() != claim.ModeUnclaimed {
		close(ch)
		return ch
	}
	cs.OnClaimed(func() { close(ch) })
	return ch
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
