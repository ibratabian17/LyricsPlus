package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"lyricsplus/backend/internal/api"
	"lyricsplus/backend/internal/config"
	"lyricsplus/backend/internal/logger"
)

func main() {
	configPath := flag.String("config", "", "Path to configuration file (.json or .env)")
	flag.Parse()

	cfg := config.Load(*configPath)
	logger := logger.New(logger.Config{
		Enabled: cfg.Logger.Enabled,
		Level:   cfg.Logger.Level,
		Format:  cfg.Logger.Format,
	})

	srv, err := api.New(cfg, logger)
	if err != nil {
		logger.Fatalf("failed to init server: %v", err)
	}
	defer func() {
		if err := srv.Close(); err != nil {
			logger.Errorf("close: %v", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Start()
	}()

	select {
	case err := <-errCh:
		if err != nil && err.Error() != "http: Server closed" {
			logger.Fatalf("server error: %v", err)
		}
	case <-ctx.Done():
		logger.Info("shutting down...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Errorf("shutdown error: %v", err)
		}
	}
}
