package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// syntheticWebOrigin is the loopback development origin every child process
// needs; it carries no credential and no real deployment value.
const syntheticWebOrigin = "http://127.0.0.1:5173"

func TestMainProcess(t *testing.T) {
	if os.Getenv("TOKENPIVOT_TEST_PROCESS") != "1" {
		return
	}
	main()
	os.Exit(0)
}

type processLogs struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	started chan struct{}
	once    sync.Once
}

func (l *processLogs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, err := l.buf.Write(p)
	if strings.Contains(l.buf.String(), `"msg":"started"`) {
		l.once.Do(func() { close(l.started) })
	}
	return n, err
}

func (l *processLogs) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func child(t *testing.T, addr string) (*exec.Cmd, *processLogs) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMainProcess$")
	// Deliberately do not inherit the parent's environment or private config.
	cmd.Env = []string{"TOKENPIVOT_TEST_PROCESS=1", "TOKENPIVOT_HTTP_ADDR=" + addr, "TOKENPIVOT_WEB_ORIGIN=" + syntheticWebOrigin, "TOKENPIVOT_UNUSED=synthetic-sensitive-marker"}
	logs := &processLogs{started: make(chan struct{})}
	cmd.Stderr = logs
	cmd.Stdout = logs
	return cmd, logs
}

func testListener(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func checkLogs(t *testing.T, text string, wantStarted, wantStopped bool) {
	t.Helper()
	if strings.Contains(text, "synthetic-sensitive-marker") {
		t.Error("logs leaked synthetic sensitive marker")
	}
	var started, stopped int
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("invalid JSON log: %v", err)
		}
		switch event["msg"] {
		case "started":
			started++
		case "stopped":
			stopped++
		case "failed":
		default:
			t.Errorf("unexpected event: %v", event["msg"])
		}
	}
	if (started == 1) != wantStarted || (stopped == 1) != wantStopped {
		t.Errorf("started=%d stopped=%d", started, stopped)
	}
}

func TestStartupFailures(t *testing.T) {
	l := testListener(t)
	defer func() {
		if err := l.Close(); err != nil {
			t.Error(err)
		}
	}()
	for _, tt := range []struct{ name, addr string }{
		{"missing database", l.Addr().String()},
		{"invalid", "synthetic-sensitive-marker"},
		{"empty", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cmd, logs := child(t, tt.addr)
			err := cmd.Run()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 {
				t.Fatalf("exit = %v, want code 1", err)
			}
			checkLogs(t, logs.text(), false, false)
			if strings.Count(logs.text(), `"msg":"failed"`) != 1 || strings.Count(strings.TrimSpace(logs.text()), "\n") != 0 {
				t.Errorf("expected exactly one failure log: %s", logs.text())
			}
		})
	}
}

// TestWebOriginFailureIsRedacted proves a missing or rejected trusted origin
// stops startup before listening and that the configured value never reaches the
// log. The address is free, so only configuration can decide the outcome.
func TestWebOriginFailureIsRedacted(t *testing.T) {
	l := testListener(t)
	defer func() {
		if err := l.Close(); err != nil {
			t.Error(err)
		}
	}()
	for name, origin := range map[string]string{
		"missing origin":    "",
		"not an origin":     "synthetic-sensitive-marker",
		"non-loopback http": "http://synthetic-sensitive-marker",
	} {
		t.Run(name, func(t *testing.T) {
			cmd, logs := child(t, l.Addr().String())
			env := make([]string, 0, len(cmd.Env))
			for _, entry := range cmd.Env {
				if !strings.HasPrefix(entry, "TOKENPIVOT_WEB_ORIGIN=") {
					env = append(env, entry)
				}
			}
			if origin != "" {
				env = append(env, "TOKENPIVOT_WEB_ORIGIN="+origin)
			}
			cmd.Env = env
			err := cmd.Run()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 {
				t.Fatalf("exit = %v, want code 1", err)
			}
			checkLogs(t, logs.text(), false, false)
			if strings.Count(logs.text(), `"msg":"failed"`) != 1 {
				t.Errorf("expected exactly one failure log: %s", logs.text())
			}
		})
	}
}
