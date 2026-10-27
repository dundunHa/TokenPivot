package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dundunHa/TokenPivot/db/migrations"
	"github.com/dundunHa/TokenPivot/internal/config"
	"github.com/dundunHa/TokenPivot/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

const operationTimeout = 30 * time.Second

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], logger); err != nil {
		logger.Error("failed", "stage", "migration", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, logger *slog.Logger) (retErr error) {
	if len(args) != 1 || (args[0] != "up" && args[0] != "status" && args[0] != "down-one") {
		return errors.New("usage: tokenpivot-migrate up|status|down-one")
	}
	cfg, err := config.LoadMigration()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	db, err := openDB(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, database.SafeError("close", db.Close())) }()
	provider, err := newProvider(db, migrations.Files)
	if err != nil {
		return err
	}
	return execute(ctx, provider, args[0], logger)
}

func openDB(ctx context.Context, value string) (*sql.DB, error) {
	if err := database.ValidateURL(value); err != nil {
		return nil, err
	}
	cfg, err := pgx.ParseConfig(value)
	if err != nil {
		return nil, database.SafeError("parse", err)
	}
	cfg.ConnectTimeout = database.ConnectTimeout
	cfg.RuntimeParams["lock_timeout"] = "5s"
	cfg.RuntimeParams["statement_timeout"] = "15s"
	db := stdlib.OpenDB(*cfg)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	pingCtx, cancel := context.WithTimeout(ctx, database.ConnectTimeout)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		return nil, errors.Join(database.SafeError("ping", err), database.SafeError("close", db.Close()))
	}
	return db, nil
}

func newProvider(db *sql.DB, files fs.FS) (*goose.Provider, error) {
	locker, err := lock.NewPostgresSessionLocker(lock.WithLockTimeout(1, 1), lock.WithUnlockTimeout(1, 1))
	if err != nil {
		return nil, database.SafeError("locker", err)
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, files,
		goose.WithSessionLocker(locker), goose.WithDisableGlobalRegistry(true),
		goose.WithSlog(slog.New(slog.NewTextHandler(io.Discard, nil))))
	return p, database.SafeError("provider", err)
}

func execute(ctx context.Context, p *goose.Provider, command string, logger *slog.Logger) error {
	switch command {
	case "status":
		statuses, err := p.Status(ctx)
		if err != nil {
			return database.SafeError("status or unlock", err)
		}
		for _, status := range statuses {
			logger.Info("status", "version", status.Source.Version, "state", string(status.State))
		}
	case "up":
		results, err := p.Up(ctx)
		if err != nil {
			return database.SafeError("up or unlock", err)
		}
		if len(results) == 0 {
			logger.Info("no change", "stage", "up")
		}
		for _, result := range results {
			logger.Info("applied", "version", result.Source.Version)
		}
	case "down-one":
		result, err := p.Down(ctx)
		if err != nil {
			return database.SafeError("down-one or unlock", err)
		}
		if result != nil {
			logger.Info("reverted", "version", result.Source.Version)
		}
	}
	return nil
}
