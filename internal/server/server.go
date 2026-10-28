// Package server provides the liveness handler and HTTP shutdown lifecycle.
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/dundunHa/TokenPivot/internal/auth"
	"github.com/dundunHa/TokenPivot/internal/config"
	"github.com/dundunHa/TokenPivot/internal/ingestion"
)

// HTTP timeouts bound slow connections and the shutdown drain period.
const (
	ReadHeaderTimeout = 5 * time.Second
	ReadTimeout       = 10 * time.Second
	WriteTimeout      = 10 * time.Second
	IdleTimeout       = 60 * time.Second
	ShutdownTimeout   = 10 * time.Second
	ReadyTimeout      = time.Second
)

// Options carries the optional dependencies of the HTTP handler. A zero value
// keeps the probe-only, no-frontend behavior.
type Options struct {
	// CheckReady probes the database for /readyz.
	CheckReady func(context.Context) error
	// Frontend serves every non-API, non-probe path; nil disables it.
	Frontend http.Handler
	// Auth enables the browser authentication routes; nil leaves them absent.
	Auth *Auth
	// Credentials enables the owner-only credential management routes; nil
	// leaves them absent. They also need Options.Auth for the session and
	// Origin gates.
	Credentials *Credentials
	// Ingestion enables the Bearer-authenticated batch ingestion route; nil
	// leaves it absent.
	Ingestion *ingestion.Handler
}

// NewHandler exposes liveness and bounded, uncached readiness, keeping API and
// probe paths on the backend while optionally serving a frontend. Browser
// authentication routes are registered only when Options.Auth is set, the
// owner-only credential routes only when both Options.Auth and
// Options.Credentials are set, and the batch ingestion route only when
// Options.Ingestion is set.
func NewHandler(options Options) http.Handler {
	checkReady := options.CheckReady
	if checkReady == nil {
		checkReady = func(context.Context) error { return nil }
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		// A write failure means the peer disconnected; no request data is logged.
		if _, err := w.Write([]byte("ok\n")); err != nil {
			return
		}
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		ctx, cancel := context.WithTimeout(r.Context(), ReadyTimeout)
		defer cancel()
		status, body := http.StatusOK, "ok\n"
		if err := checkReady(ctx); err != nil {
			status, body = http.StatusServiceUnavailable, "not ready\n"
		}
		w.WriteHeader(status)
		if r.Method != http.MethodHead {
			if _, err := w.Write([]byte(body)); err != nil {
				return
			}
		}
	})
	options.Auth.routes(mux)
	options.Credentials.routes(mux, options.Auth)
	options.Ingestion.Register(mux)
	if options.Frontend == nil {
		return mux
	}
	frontend := options.Frontend
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, prefix := range []string{"/api", "/healthz", "/readyz"} {
			if r.URL.Path == prefix || strings.HasPrefix(r.URL.Path, prefix+"/") {
				mux.ServeHTTP(w, r)
				return
			}
		}
		frontend.ServeHTTP(w, r)
	})
}

// NewAuth returns the HTTP authentication adapter for a service and the trusted
// origin resolved from configuration.
func NewAuth(service *auth.Service, origin config.Origin) *Auth {
	return &Auth{Service: service, Origin: origin}
}

// Run serves listener until cancellation or a serving error. Cancellation drains
// active requests using a fresh context; failed shutdown force-closes connections.
// Run owns and joins the Serve goroutine before returning.
func Run(ctx context.Context, srv *http.Server, listener net.Listener, shutdownTimeout time.Duration) error {
	served := make(chan error, 1)
	go func() { served <- srv.Serve(listener) }()

	var serveErr error
	select {
	case serveErr = <-served:
		if errors.Is(serveErr, http.ErrServerClosed) {
			return nil
		}
		return errors.Join(fmt.Errorf("serve: %w", serveErr), srv.Close())
	case <-ctx.Done():
	}

	// Signal cancellation must not cancel requests that are still draining.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	shutdownErr := srv.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		shutdownErr = errors.Join(fmt.Errorf("shutdown: %w", shutdownErr), srv.Close())
	}
	serveErr = <-served
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	return errors.Join(shutdownErr, serveErr)
}
