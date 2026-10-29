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

	"github.com/dundunHa/TokenPivot/internal/auth"
	"github.com/dundunHa/TokenPivot/internal/config"
	"github.com/dundunHa/TokenPivot/internal/credentials"
	"github.com/dundunHa/TokenPivot/internal/database"
	"github.com/dundunHa/TokenPivot/internal/ingestion"
	"github.com/dundunHa/TokenPivot/internal/projects"
	"github.com/dundunHa/TokenPivot/internal/server"
	"github.com/dundunHa/TokenPivot/internal/sources"
	"github.com/dundunHa/TokenPivot/internal/usage"
	"github.com/dundunHa/TokenPivot/internal/workspaces"
	"github.com/dundunHa/TokenPivot/web"
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
	frontend, err := web.Handler()
	if err != nil {
		return fmt.Errorf("frontend: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	authStore := auth.NewPostgres(pool)
	authService := auth.New(authStore, authStore, auth.NewLimiter(nil), cfg.RegistrationOpen)
	credentialService := credentials.New(credentials.NewPostgres(pool))
	workspaceService := workspaces.New(workspaces.NewPostgres(pool))
	usageService := usage.New(usage.NewPostgres(pool))
	ingestionService := ingestion.New(ingestion.NewPostgres(pool), credentialService, logger)
	srv := &http.Server{
		Handler: server.NewHandler(server.Options{
			CheckReady:  pool.Ping,
			Frontend:    frontend,
			Auth:        server.NewAuth(authService, cfg.WebOrigin),
			Credentials: server.NewCredentials(credentialService, workspaceService),
			Usage:       server.NewUsage(usageService, workspaceService),
			Projects:    server.NewProjects(projects.New(projects.NewPostgres(pool)), workspaceService),
			Sources:     server.NewSources(sources.New(sources.NewPostgres(pool)), workspaceService),
			Ingestion:   ingestion.NewHandler(ingestionService),
		}),
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
