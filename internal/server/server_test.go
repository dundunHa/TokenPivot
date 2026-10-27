package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestHandler(t *testing.T) {
	srv := httptest.NewServer(NewHandler(nil))
	defer srv.Close()
	client := srv.Client()
	client.Timeout = 3 * time.Second
	for _, tt := range []struct {
		method, path string
		status       int
		body         string
	}{
		{"GET", "/healthz", 200, "ok\n"},
		{"HEAD", "/healthz", 200, ""},
		{"POST", "/healthz", 405, "Method Not Allowed\n"},
		{"PUT", "/healthz", 405, "Method Not Allowed\n"},
		{"GET", "/", 404, "404 page not found\n"},
		{"GET", "/api/example", 404, "404 page not found\n"},
		{"GET", "/readyz", 404, "404 page not found\n"},
		{"GET", "/healthz/", 404, "404 page not found\n"},
	} {
		t.Run(tt.method+tt.path, func(t *testing.T) {
			req, err := http.NewRequest(tt.method, srv.URL+tt.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			if closeErr := resp.Body.Close(); closeErr != nil {
				t.Error(closeErr)
			}
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tt.status || string(body) != tt.body {
				t.Fatalf("status/body = %d %q, want %d %q", resp.StatusCode, body, tt.status, tt.body)
			}
			if got := resp.Header.Get("Content-Type"); got != "text/plain; charset=utf-8" {
				t.Errorf("Content-Type = %q", got)
			}
		})
	}
}

func TestFrontendDispatch(t *testing.T) {
	frontend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frontend", "yes")
		w.WriteHeader(http.StatusNoContent)
	})
	h := NewHandler(frontend)
	for _, method := range []string{"GET", "HEAD", "POST", "PUT", "DELETE", "OPTIONS", "PATCH"} {
		for _, path := range []string{"/api", "/api/", "/api/not-implemented", "/api?x=1", "/healthz", "/healthz/", "/healthz/unknown", "/readyz", "/readyz/", "/readyz/unknown"} {
			t.Run(method+path, func(t *testing.T) {
				r := httptest.NewRequest(method, path, nil)
				r.Header.Set("Accept", "text/html")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				want := http.StatusNotFound
				if path == "/healthz" {
					want = http.StatusMethodNotAllowed
					if method == "GET" || method == "HEAD" {
						want = http.StatusOK
					}
				}
				if w.Code != want || w.Header().Get("X-Frontend") != "" {
					t.Fatalf("status/frontend = %d %q, want %d backend", w.Code, w.Header().Get("X-Frontend"), want)
				}
			})
		}
		for _, path := range []string{"/", "/route", "/apix", "/healthzx", "/readyzx"} {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(method, path, nil))
			if w.Code != http.StatusNoContent || w.Header().Get("X-Frontend") != "yes" {
				t.Errorf("%s %s did not reach frontend", method, path)
			}
		}
	}
}

// observedListener tells tests when Shutdown has closed the listening socket.
type observedListener struct {
	net.Listener
	closed chan struct{}
	once   sync.Once
}

func (l *observedListener) Close() error {
	err := l.Listener.Close()
	l.once.Do(func() { close(l.closed) })
	return err
}

func listen(t *testing.T) *observedListener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return &observedListener{Listener: l, closed: make(chan struct{})}
}

func await[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for test event")
		var zero T
		return zero
	}
}

func assertNotListening(t *testing.T, addr string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err == nil {
		if closeErr := conn.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal("listener remains open")
	}
}

func TestRunCancellation(t *testing.T) {
	l := listen(t)
	srv := &http.Server{Handler: NewHandler(nil)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, srv, l, time.Second) }()
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + l.Addr().String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	cancel()
	if err := await(t, done); err != nil {
		t.Fatal(err)
	}
	assertNotListening(t, l.Addr().String())
}

func TestRunDrainsRequest(t *testing.T) {
	l := listen(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	requestCanceled := make(chan bool, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		requestCanceled <- r.Context().Err() != nil
		w.WriteHeader(http.StatusNoContent)
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, srv, l, 3*time.Second) }()
	response := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 4 * time.Second}
		resp, err := client.Get("http://" + l.Addr().String())
		if err == nil {
			err = resp.Body.Close()
		}
		response <- err
	}()
	await(t, entered)
	cancel()
	await(t, l.closed)
	select {
	case err := <-done:
		t.Fatalf("Run returned before request drained: %v", err)
	default:
	}
	once.Do(func() { close(release) })
	if canceled := await(t, requestCanceled); canceled {
		t.Error("shutdown canceled request context")
	}
	if err := await(t, response); err != nil {
		t.Error(err)
	}
	if err := await(t, done); err != nil {
		t.Error(err)
	}
	assertNotListening(t, l.Addr().String())
}

func TestRunShutdownTimeoutClosesConnection(t *testing.T) {
	l := listen(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		close(finished)
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, srv, l, 20*time.Millisecond) }()
	response := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 4 * time.Second}
		resp, err := client.Get("http://" + l.Addr().String())
		if err == nil {
			err = resp.Body.Close()
		}
		response <- err
	}()
	await(t, entered)
	cancel()
	if err := await(t, done); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run error = %v, want deadline exceeded", err)
	}
	// The handler is still blocked: only Close can terminate this connection.
	if err := await(t, response); err == nil {
		t.Error("active connection was not forcibly closed")
	} else {
		var networkErr net.Error
		if errors.As(err, &networkErr) && networkErr.Timeout() {
			t.Errorf("client timed out instead of observing forced connection closure: %v", err)
		}
	}
	once.Do(func() { close(release) })
	await(t, finished)
	assertNotListening(t, l.Addr().String())
}

func TestRunServeFailure(t *testing.T) {
	l := listen(t)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	err := Run(context.Background(), &http.Server{Handler: NewHandler(nil)}, l, time.Second)
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Run error = %v, want closed listener", err)
	}
}

func TestRunExpectedServerClosed(t *testing.T) {
	l := listen(t)
	srv := &http.Server{Handler: NewHandler(nil)}
	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), srv, l, time.Second); err != nil {
		t.Fatal(err)
	}
	assertNotListening(t, l.Addr().String())
}
