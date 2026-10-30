// Package database owns service connections and safe database error boundaries.
package database

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ConnectTimeout bounds startup and actual driver connection attempts.
const ConnectTimeout = 5 * time.Second

// SafeError discards driver messages, SQL, credentials and server detail.
// Only cancellation/deadline identity survives; callers supply a static stage.
func SafeError(stage string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return errors.Join(errors.New(stage), context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.Join(errors.New(stage), context.DeadlineExceeded)
	}
	return errors.New(stage + ": database operation failed")
}

// ErrUnavailable marks a failure of the database dependency itself (connection
// failure, closed connection, SQLSTATE class 08, deadline), not a statement fault.
var ErrUnavailable = errors.New("database unavailable")

// SafeStoreError is SafeError plus ErrUnavailable when the database could not be
// used. A client disconnect (context.Canceled) is not a dependency failure.
func SafeStoreError(stage string, err error) error {
	safe := SafeError(stage, err)
	if safe != nil && isUnavailable(err) {
		return errors.Join(ErrUnavailable, safe)
	}
	return safe
}

// isUnavailable recognizes dependency failures, not statement faults: failed
// connection attempts, a connection the driver or peer already closed (EOF,
// reset, broken pipe, network errors), SQLSTATE class 08 and the
// server shutdown states 57P01/57P02/57P03. A client disconnect stays false.
func isUnavailable(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, pgconn.ErrConnClosed) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var connectErr *pgconn.ConnectError
	if errors.As(err, &connectErr) {
		return true
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case "57P01", "57P02", "57P03":
		return true
	}
	return strings.HasPrefix(pgErr.Code, "08")
}

// ValidateURL requires explicit PostgreSQL connection coordinates and TLS policy.
// Driver parsing remains responsible for the remaining connection options.
func ValidateURL(value string) error {
	u, err := url.Parse(value)
	if err != nil || u == nil {
		return errors.New("database URL is invalid")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Hostname() == "" || u.User == nil || u.User.Username() == "" || strings.Trim(u.Path, "/") == "" || q.Get("sslmode") == "" || u.Fragment != "" {
		return errors.New("database URL requires PostgreSQL scheme, host, database, user and sslmode")
	}
	return nil
}

func poolConfig(value string) (*pgxpool.Config, error) {
	if err := ValidateURL(value); err != nil {
		return nil, err
	}
	cfg, err := pgxpool.ParseConfig(value)
	if err != nil {
		return nil, SafeError("parse", err)
	}
	cfg.MaxConns = 4
	cfg.MinConns = 0
	cfg.MinIdleConns = 0
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.ConnConfig.ConnectTimeout = ConnectTimeout
	return cfg, nil
}

// Open creates the service pool and verifies connectivity before returning.
// Failure closes any created pool; the successful caller owns Close.
func Open(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	ctx, cancel := context.WithTimeout(ctx, ConnectTimeout)
	defer cancel()
	cfg, err := poolConfig(databaseURL)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, SafeError("open", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, SafeError("ping", err)
	}
	return pool, nil
}
