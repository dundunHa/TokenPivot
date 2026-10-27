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
)

// HTTP timeouts bound slow connections and the shutdown drain period.
const (
	ReadHeaderTimeout = 5 * time.Second
	ReadTimeout       = 10 * time.Second
	WriteTimeout      = 10 * time.Second
	IdleTimeout       = 60 * time.Second
	ShutdownTimeout   = 10 * time.Second
)

// NewHandler keeps API and probe paths on the backend and optionally serves a frontend.
func NewHandler(frontend http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		// A write failure means the peer disconnected; no request data is logged.
		if _, err := w.Write([]byte("ok\n")); err != nil {
			return
		}
	})
	if frontend == nil {
		return mux
	}
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
