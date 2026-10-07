// Command server runs the FixLab Phase 1 backend: the REST API, the
// session manager (QuickFIX/Go acceptors behind the TCP guard), and the
// session expiry worker.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"fixlab.dev/fixlab/backend/internal/api"
	"fixlab.dev/fixlab/backend/internal/common"
	"fixlab.dev/fixlab/backend/internal/session"
	"fixlab.dev/fixlab/backend/internal/storage"
	"fixlab.dev/fixlab/backend/internal/websocket"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fixlab:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := common.LoadConfig()
	log := common.NewLogger()
	metrics := common.DefaultMetrics()

	log.Info("fixlab starting",
		"http_addr", cfg.HTTPAddr,
		"port_pool", fmt.Sprintf("%d-%d", cfg.PortPoolMin, cfg.PortPoolMax),
		"session_ttl", cfg.SessionTTL.String(),
		"app_msg_limit", cfg.AppMessageLimit)

	hub := websocket.NewHub()

	mgr, err := session.NewManager(cfg, log, metrics, storage.NewMemoryStore(), hub)
	if err != nil {
		return fmt.Errorf("session manager: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.RunCleanupLoop(ctx)

	srv := api.NewServer(cfg, log, metrics, mgr, hub)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start() }()

	log.Info("fixlab ready",
		"note", "SIMULATION ENVIRONMENT — NOT A LIVE TRADING VENUE")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-sigCh:
		log.Info("fixlab shutting down", "signal", sig.String())
	case err := <-errCh:
		return fmt.Errorf("http server: %w", err)
	}

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	_ = srv.Shutdown(shutCtx)
	cancel()
	mgr.Shutdown()
	return nil
}
