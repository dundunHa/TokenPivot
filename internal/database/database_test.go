package database

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func isolatePG(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "PG") {
			t.Setenv(key, "")
			if err := os.Unsetenv(key); err != nil {
				t.Fatal("unset PG setting failed")
			}
		}
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PGPASSFILE", "/dev/null")
	t.Setenv("PGSERVICEFILE", "/dev/null")
}

func TestPoolConfig(t *testing.T) {
	isolatePG(t)
	cfg, err := poolConfig("postgres://synthetic:synthetic-sensitive-marker@127.0.0.1/test?sslmode=disable&pool_max_conns=99&connect_timeout=99")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxConns != 4 || cfg.MinConns != 0 || cfg.MinIdleConns != 0 || cfg.MaxConnLifetime != time.Hour || cfg.MaxConnIdleTime != 5*time.Minute || cfg.ConnConfig.ConnectTimeout != ConnectTimeout {
		t.Fatal("unexpected connection policy")
	}
}

func TestInvalidURLs(t *testing.T) {
	isolatePG(t)
	for _, value := range []string{"", "synthetic-sensitive-marker", "host=127.0.0.1", "http://u@localhost/test?sslmode=disable", "postgres://u@/test?sslmode=disable", "postgres://localhost/test?sslmode=disable", "postgres://u@localhost/?sslmode=disable", "postgres://u@localhost/test", "postgres://u:synthetic-sensitive-marker@localhost/test?sslmode=invalid", "postgres://u:synthetic-sensitive-marker@localhost/test?sslmode=disable&pool_max_conns=no"} {
		if _, err := poolConfig(value); err == nil {
			t.Fatal("invalid URL accepted")
		} else if strings.Contains(err.Error(), "synthetic-sensitive-marker") {
			t.Fatal("sensitive value leaked")
		}
	}
}

func TestSafeError(t *testing.T) {
	for _, cause := range []error{errors.New("synthetic-sensitive-marker"), context.Canceled, context.DeadlineExceeded} {
		err := SafeError("test", fmt.Errorf("synthetic-sensitive-marker: %w", cause))
		if strings.Contains(err.Error(), "synthetic-sensitive-marker") {
			t.Fatal("leaked error")
		}
		if (cause == context.Canceled || cause == context.DeadlineExceeded) && !errors.Is(err, cause) {
			t.Fatal("lost context error")
		}
	}
	if SafeError("test", nil) != nil {
		t.Fatal("nil error changed")
	}
}
