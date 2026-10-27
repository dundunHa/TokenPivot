package config

import (
	"os"
	"strings"
	"testing"
)

func TestLoadDefault(t *testing.T) {
	// Setenv arranges restoration without printing the original environment value.
	t.Setenv("TOKENPIVOT_HTTP_ADDR", "")
	if err := os.Unsetenv("TOKENPIVOT_HTTP_ADDR"); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil || cfg.HTTPAddr != "127.0.0.1:8080" {
		t.Fatalf("default config = %+v, err = %v", cfg, err)
	}
}

func TestLoadAddresses(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8080", "[::1]:8080", "localhost:8080", ":8080", "localhost:1", "localhost:65535"} {
		t.Run(addr, func(t *testing.T) {
			t.Setenv("TOKENPIVOT_HTTP_ADDR", addr)
			t.Setenv("TOKENPIVOT_UNKNOWN", "synthetic-unused-value")
			cfg, err := Load()
			if err != nil || cfg.HTTPAddr != addr {
				t.Fatalf("config = %+v, err = %v", cfg, err)
			}
		})
	}
}

func TestLoadRejectsInvalidAddresses(t *testing.T) {
	for _, addr := range []string{"", "synthetic-secret-marker", "localhost:", "localhost:http", "localhost:0", "localhost:65536", "localhost:-1", "localhost:+80", "localhost: 80", "localhost:999999999999999999999", "::1:8080", "synthetic-secret-marker:bad"} {
		t.Run(addr, func(t *testing.T) {
			t.Setenv("TOKENPIVOT_HTTP_ADDR", addr)
			_, err := Load()
			if err == nil {
				t.Fatal("expected validation error")
			}
			if !strings.Contains(err.Error(), "TOKENPIVOT_HTTP_ADDR") || strings.Contains(err.Error(), "synthetic-secret-marker") {
				t.Fatalf("unsafe or unhelpful error: %v", err)
			}
		})
	}
}
