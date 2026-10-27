package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/dundunHa/TokenPivot/internal/config"
	"github.com/dundunHa/TokenPivot/internal/database"
	"github.com/dundunHa/TokenPivot/internal/server"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(logger); err != nil {
		logger.Error("failed", "stage", "service", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           server.NewHandler(pool.Ping),
		ReadHeaderTimeout: server.ReadHeaderTimeout,
		ReadTimeout:       server.ReadTimeout,
		WriteTimeout:      server.WriteTimeout,
		IdleTimeout:       server.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
	listener, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		pool.Close()
		return fmt.Errorf("listen: %w", err)
	}
	logger.Info("started", "stage", "http", "addr", listener.Addr().String())
	err = server.Run(ctx, srv, listener, server.ShutdownTimeout)
	pool.Close()
	if err != nil {
		return err
	}
	logger.Info("stopped", "stage", "http")
	return nil
}
