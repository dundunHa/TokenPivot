//go:build integration

package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"github.com/dundunHa/TokenPivot/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"net/url"
	"sync"

	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSignals(t *testing.T) {
	_, appURL := integrationSchema(t)

	for _, sig := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			l := testListener(t)
			addr := l.Addr().String()
			if err := l.Close(); err != nil {
				t.Fatal(err)
			}
			cmd, logs := child(t, addr)
			cmd.Env = append(cmd.Env, "TOKENPIVOT_DATABASE_URL="+appURL, "PGPASSFILE=/dev/null", "PGSERVICEFILE=/dev/null")
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case <-logs.started:
			case err := <-done:
				t.Fatalf("process exited before startup: %v; %s", err, logs.text())
			case <-time.After(5 * time.Second):
				t.Fatal("startup timeout")
			}
			client := &http.Client{Timeout: 3 * time.Second}
			req, err := http.NewRequest("POST", "http://"+addr+"/healthz?q=synthetic-sensitive-marker", strings.NewReader("synthetic-sensitive-marker"))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "synthetic-sensitive-marker")
			req.Header.Set("Cookie", "test=synthetic-sensitive-marker")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(io.Discard, resp.Body); err != nil {
				t.Error(err)
			}
			if err := resp.Body.Close(); err != nil {
				t.Error(err)
			}
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Errorf("POST status = %d", resp.StatusCode)
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("signal exit: %v; %s", err, logs.text())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("shutdown timeout")
			}
			checkLogs(t, logs.text(), true, true)
			if n := strings.Count(strings.TrimSpace(logs.text()), "\n") + 1; n != 2 {
				t.Errorf("got %d log lines, want only started/stopped", n)
			}
			conn, err := net.DialTimeout("tcp", addr, time.Second)
			if err == nil {
				if err := conn.Close(); err != nil {
					t.Error(err)
				}
				t.Error("process left listener open")
			}
		})
	}
}

func integrationSchema(t *testing.T) (string, string) {
	t.Helper()
	values := []string{os.Getenv("TOKENPIVOT_TEST_DATABASE_URL"), os.Getenv("TOKENPIVOT_TEST_APP_DATABASE_URL")}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "PG") {
			t.Setenv(key, "")
			if err := os.Unsetenv(key); err != nil {
				t.Fatal("unset PG failed")
			}
		}
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PGPASSFILE", "/dev/null")
	t.Setenv("PGSERVICEFILE", "/dev/null")
	urls := make([]*url.URL, 2)
	for i, value := range values {
		if err := database.ValidateURL(value); err != nil {
			t.Fatal("explicit integration database variables required")
		}
		u, err := url.Parse(value)
		if err != nil {
			t.Fatal("invalid test URL")
		}
		ip := net.ParseIP(u.Hostname())
		if ip == nil || !ip.IsLoopback() || !strings.HasSuffix(strings.TrimPrefix(u.Path, "/"), "_test") {
			t.Fatal("test database must use a loopback IP and _test database suffix")
		}
		// Prevent URL query parameters from overriding checked coordinates or loading service files.
		for key := range u.Query() {
			if key != "sslmode" && key != "search_path" {
				t.Fatal("test URL has unsupported query option")
			}
		}
		urls[i] = u
	}
	if urls[0].Host != urls[1].Host || urls[0].Path != urls[1].Path || urls[0].User.Username() == urls[1].User.Username() {
		t.Fatal("test roles must be distinct in the same database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	owner, err := pgx.Connect(ctx, values[0])
	if err != nil {
		t.Fatal(database.SafeError("test owner connect", err))
	}
	t.Cleanup(func() {
		if err := owner.Close(context.Background()); err != nil {
			t.Error("test owner close failed")
		}
	})
	var db, role string
	var super, createRole bool
	if err := owner.QueryRow(ctx, "SELECT current_database(),current_user,rolsuper,rolcreaterole FROM pg_roles WHERE rolname=current_user").Scan(&db, &role, &super, &createRole); err != nil {
		t.Fatal(database.SafeError("identity", err))
	}
	if db != strings.TrimPrefix(urls[0].Path, "/") || role != urls[0].User.Username() || super || createRole {
		t.Fatal("unsafe test migration role")
	}
	app, err := pgx.Connect(ctx, values[1])
	if err != nil {
		t.Fatal(database.SafeError("app connect", err))
	}
	var appRole string
	var unsafe bool
	err = app.QueryRow(ctx, "SELECT current_user, rolsuper OR rolcreaterole OR rolcreatedb OR has_database_privilege(current_user,current_database(),'CREATE') FROM pg_roles WHERE rolname=current_user").Scan(&appRole, &unsafe)
	closeErr := app.Close(ctx)
	if err != nil || closeErr != nil || unsafe || appRole != urls[1].User.Username() {
		t.Fatal("unsafe test service role")
	}
	schema := "t050_" + fmt.Sprintf("%x", rand.Text()[:12])
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := owner.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(database.SafeError("create owned schema", err))
	}
	t.Logf("owned schema=%s database=%s migrator=%s app=%s", schema, db, role, appRole)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var actual string
		err := owner.QueryRow(cleanupCtx, "SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname=$1", schema).Scan(&actual)
		if err != nil || actual != role || !strings.HasPrefix(schema, "t050_") {
			t.Errorf("preserving schema %s: ownership check failed", schema)
			return
		}
		if _, err := owner.Exec(cleanupCtx, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Errorf("preserving schema %s: cleanup failed", schema)
		}
	})
	if _, err := owner.Exec(ctx, "GRANT USAGE ON SCHEMA "+quoted+" TO "+pgx.Identifier{appRole}.Sanitize()); err != nil {
		t.Fatal(database.SafeError("grant usage", err))
	}
	for i, u := range urls {
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		values[i] = u.String()
	}
	return values[0], values[1]
}

// tcpForwarder belongs only to this test. Blocking joins every active relay before returning.
type tcpForwarder struct {
	listener   net.Listener
	target     string
	mu         sync.Mutex
	blocked    bool
	closed     bool
	pairs      map[net.Conn]net.Conn
	active     sync.WaitGroup
	acceptDone chan struct{}
}

func newForwarder(t *testing.T, target string) *tcpForwarder {
	t.Helper()
	p := &tcpForwarder{listener: testListener(t), target: target, pairs: make(map[net.Conn]net.Conn), acceptDone: make(chan struct{})}
	go func() {
		defer close(p.acceptDone)
		for {
			a, err := p.listener.Accept()
			if err != nil {
				return
			}
			p.mu.Lock()
			if p.blocked || p.closed {
				_ = a.Close()
				p.mu.Unlock()
				continue
			}
			b, err := net.DialTimeout("tcp", p.target, time.Second)
			if err != nil {
				_ = a.Close()
				p.mu.Unlock()
				continue
			}
			p.pairs[a] = b
			p.active.Add(1)
			p.mu.Unlock()
			go func() {
				defer p.active.Done()
				done := make(chan struct{})
				go func() { _, _ = io.Copy(b, a); _ = b.Close(); _ = a.Close(); close(done) }()
				_, _ = io.Copy(a, b)
				_ = a.Close()
				_ = b.Close()
				<-done
				p.mu.Lock()
				delete(p.pairs, a)
				p.mu.Unlock()
			}()
		}
	}()
	t.Cleanup(func() {
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
		_ = p.listener.Close()
		<-p.acceptDone
		p.block(true)
	})
	return p
}

func (p *tcpForwarder) block(blocked bool) {
	p.mu.Lock()
	p.blocked = blocked
	if blocked {
		for a, b := range p.pairs {
			_ = a.Close()
			_ = b.Close()
		}
	}
	p.mu.Unlock()
	if blocked {
		p.active.Wait()
	}
}

func probe(t *testing.T, addr, path string, want int) {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + path)
	if err != nil {
		t.Fatal("HTTP probe failed")
	}
	body, err := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if err != nil || closeErr != nil {
		t.Fatal("probe read failed")
	}
	if resp.StatusCode != want || strings.Contains(string(body), "synthetic-sensitive-marker") {
		t.Fatalf("probe %s status=%d want=%d", path, resp.StatusCode, want)
	}
}

func TestDatabaseDisconnectRecovery(t *testing.T) {
	ownerURL, appURL := integrationSchema(t)
	u, err := url.Parse(appURL)
	if err != nil {
		t.Fatal("parse app URL failed")
	}
	proxy := newForwarder(t, u.Host)
	u.Host = proxy.listener.Addr().String()
	l := testListener(t)
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	cmd, logs := child(t, addr)
	cmd.Env = append(cmd.Env, "TOKENPIVOT_DATABASE_URL="+u.String(), "PGPASSFILE=/dev/null", "PGSERVICEFILE=/dev/null")
	if err := cmd.Start(); err != nil {
		t.Fatal("service start failed")
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				t.Error("child cleanup failed")
			}
			<-done
		}
	})
	select {
	case <-logs.started:
	case <-done:
		stopped = true
		t.Fatal("service exited early")
	case <-time.After(5 * time.Second):
		t.Fatal("service startup timed out")
	}
	probe(t, addr, "/readyz", 200)
	proxy.block(true)
	for range 3 {
		probe(t, addr, "/healthz", 200)
		probe(t, addr, "/readyz", 503)
	}
	proxy.block(false)
	deadline := time.Now().Add(5 * time.Second)
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		resp, err := client.Get("http://" + addr + "/readyz")
		if err == nil {
			status := resp.StatusCode
			if err := resp.Body.Close(); err != nil {
				t.Fatal("close probe failed")
			}
			if status == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("readiness did not recover")
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-done:
			stopped = true
			t.Fatal("service exited during outage")
		}
	}
	start := time.Now()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal("signal failed")
	}
	select {
	case err := <-done:
		stopped = true
		if err != nil {
			t.Fatal("service shutdown failed")
		}
	case <-time.After(6 * time.Second):
		t.Fatal("shutdown timeout")
	}
	checkLogs(t, logs.text(), true, true)
	t.Logf("disconnect recovery and shutdown exit=0 close_duration=%s", time.Since(start))
	assertNoServiceConnections(t, ownerURL)
}

func assertNoServiceConnections(t *testing.T, ownerURL string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, ownerURL)
	if err != nil {
		t.Fatal(database.SafeError("observer", err))
	}
	defer func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Error("observer close failed")
		}
	}()
	appURL, err := url.Parse(os.Getenv("TOKENPIVOT_TEST_APP_DATABASE_URL"))
	if err != nil {
		t.Fatal("invalid app URL")
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		var count int
		if err := conn.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND usename=$1", appURL.User.Username()).Scan(&count); err != nil {
			t.Fatal(database.SafeError("observe close", err))
		}
		if count == 0 {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("service connections remain")
		}
	}
}

func TestDatabaseStartupFailures(t *testing.T) {
	ownerURL, appURL := integrationSchema(t)
	occupied := testListener(t)
	defer func() {
		if err := occupied.Close(); err != nil {
			t.Error(err)
		}
	}()
	vacant := testListener(t)
	vacantAddr := vacant.Addr().String()
	if err := vacant.Close(); err != nil {
		t.Fatal(err)
	}
	invalid, err := url.Parse(appURL)
	if err != nil {
		t.Fatal("invalid URL")
	}
	invalid.User = url.UserPassword("synthetic-sensitive-marker", "synthetic-sensitive-marker")
	wrongPassword, err := url.Parse(appURL)
	if err != nil {
		t.Fatal("invalid URL")
	}
	wrongPassword.User = url.UserPassword(wrongPassword.User.Username(), "synthetic-sensitive-marker")
	// The real service identity already connected successfully in integrationSchema.
	// Require password rejection specifically, not an absent role or a trust rule.
	authCtx, authCancel := context.WithTimeout(context.Background(), 5*time.Second)
	authConn, authErr := pgx.Connect(authCtx, wrongPassword.String())
	authCancel()
	if authConn != nil {
		if err := authConn.Close(context.Background()); err != nil {
			t.Error("authentication probe close failed")
		}
	}
	var pgErr *pgconn.PgError
	if !errors.As(authErr, &pgErr) || pgErr.Code != "28P01" {
		t.Fatal("test service role must reject wrong passwords with SQLSTATE 28P01")
	}
	unreachable, err := url.Parse(appURL)
	if err != nil {
		t.Fatal("invalid URL")
	}
	unreachable.Host = vacantAddr
	// A local accepted TCP socket that never speaks PostgreSQL forces real connect timeout.
	stalled := testListener(t)
	defer func() {
		if err := stalled.Close(); err != nil {
			t.Error(err)
		}
	}()
	slow, err := url.Parse(appURL)
	if err != nil {
		t.Fatal("invalid URL")
	}
	slow.Host = stalled.Addr().String()
	for _, tt := range []struct{ name, addr, value, stage string }{
		{"occupied", occupied.Addr().String(), appURL, "listen"},
		{"invalid identity", vacantAddr, invalid.String(), "ping"},
		{"wrong password", vacantAddr, wrongPassword.String(), "ping"},
		{"unreachable", vacantAddr, unreachable.String(), "ping"},
		{"timeout", vacantAddr, slow.String(), "ping"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd, logs := child(t, tt.addr)
			cmd.Env = append(cmd.Env, "TOKENPIVOT_DATABASE_URL="+tt.value, "PGPASSFILE=/dev/null", "PGSERVICEFILE=/dev/null")
			start := time.Now()
			err := cmd.Run()
			if err == nil || cmd.ProcessState.ExitCode() != 1 {
				t.Fatal("startup failure did not exit 1")
			}
			checkLogs(t, logs.text(), false, false)
			if !strings.Contains(logs.text(), tt.stage) {
				t.Fatal("wrong startup failure stage")
			}
			if tt.name != "occupied" {
				conn, err := net.DialTimeout("tcp", tt.addr, 100*time.Millisecond)
				if err == nil {
					_ = conn.Close()
					t.Fatal("HTTP listener opened on database failure")
				}
			}
			assertNoServiceConnections(t, ownerURL)
			t.Logf("startup %s exit=1 duration=%s", tt.name, time.Since(start))
		})
	}
}
