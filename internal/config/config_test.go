package config

import (
	"os"
	"strings"
	"testing"
)

func TestLoadDefault(t *testing.T) {
	t.Setenv("TOKENPIVOT_DATABASE_URL", "synthetic-sensitive-marker")
	t.Setenv("TOKENPIVOT_WEB_ORIGIN", "http://127.0.0.1:5173")
	// Setenv arranges restoration without printing the original environment value.
	t.Setenv("TOKENPIVOT_HTTP_ADDR", "")
	if err := os.Unsetenv("TOKENPIVOT_HTTP_ADDR"); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil || cfg.HTTPAddr != "127.0.0.1:8080" {
		t.Fatalf("default HTTPAddr = %s, err = %v", cfg.HTTPAddr, err)
	}
}

func TestLoadAddresses(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8080", "[::1]:8080", "localhost:8080", ":8080", "localhost:1", "localhost:65535"} {
		t.Run(addr, func(t *testing.T) {
			t.Setenv("TOKENPIVOT_DATABASE_URL", "synthetic-sensitive-marker")
			t.Setenv("TOKENPIVOT_HTTP_ADDR", addr)
			t.Setenv("TOKENPIVOT_WEB_ORIGIN", "http://127.0.0.1:5173")
			t.Setenv("TOKENPIVOT_UNKNOWN", "synthetic-unused-value")
			cfg, err := Load()
			if err != nil || cfg.HTTPAddr != addr {
				t.Fatalf("HTTPAddr = %s, err = %v", cfg.HTTPAddr, err)
			}
		})
	}
}

func TestLoadRejectsInvalidAddresses(t *testing.T) {
	for _, addr := range []string{"", "synthetic-secret-marker", "localhost:", "localhost:http", "localhost:0", "localhost:65536", "localhost:-1", "localhost:+80", "localhost: 80", "localhost:999999999999999999999", "::1:8080", "synthetic-secret-marker:bad"} {
		t.Run(addr, func(t *testing.T) {
			t.Setenv("TOKENPIVOT_DATABASE_URL", "synthetic-sensitive-marker")
			t.Setenv("TOKENPIVOT_HTTP_ADDR", addr)
			t.Setenv("TOKENPIVOT_WEB_ORIGIN", "http://127.0.0.1:5173")
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

func TestDatabaseRequired(t *testing.T) {
	t.Setenv("TOKENPIVOT_HTTP_ADDR", "127.0.0.1:8080")
	t.Setenv("TOKENPIVOT_WEB_ORIGIN", "http://127.0.0.1:5173")
	for _, value := range []string{"", "  "} {
		t.Setenv("TOKENPIVOT_DATABASE_URL", value)
		if _, err := Load(); err == nil {
			t.Fatal("missing database accepted")
		}
	}
}
func TestMigrationIndependent(t *testing.T) {
	t.Setenv("TOKENPIVOT_HTTP_ADDR", "invalid")
	t.Setenv("TOKENPIVOT_DATABASE_URL", "synthetic-sensitive-marker")
	t.Setenv("TOKENPIVOT_MIGRATION_DATABASE_URL", "")
	if _, err := LoadMigration(); err == nil {
		t.Fatal("service fallback accepted")
	}
	t.Setenv("TOKENPIVOT_MIGRATION_DATABASE_URL", "synthetic-migration-marker")
	cfg, err := LoadMigration()
	if err != nil || cfg.DatabaseURL != "synthetic-migration-marker" {
		t.Fatal("independent migration configuration failed")
	}
}

func TestWebOriginRequired(t *testing.T) {
	t.Setenv("TOKENPIVOT_HTTP_ADDR", "127.0.0.1:8080")
	t.Setenv("TOKENPIVOT_DATABASE_URL", "synthetic-sensitive-marker")
	for _, value := range []string{"", "   "} {
		t.Setenv("TOKENPIVOT_WEB_ORIGIN", value)
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "TOKENPIVOT_WEB_ORIGIN") {
			t.Fatalf("missing web origin accepted: %v", err)
		}
	}
}

func TestWebOriginAccepted(t *testing.T) {
	for _, tt := range []struct {
		value  string
		want   string
		secure bool
	}{
		{"http://127.0.0.1:5173", "http://127.0.0.1:5173", false},
		{"http://localhost:5173", "http://localhost:5173", false},
		{"http://LOCALHOST:5173", "http://localhost:5173", false},
		{"http://[::1]:5173", "http://[::1]:5173", false},
		{"http://127.0.0.1:80", "http://127.0.0.1", false},
		{"HTTP://127.0.0.1:8080", "http://127.0.0.1:8080", false},
		{"https://app.example.com", "https://app.example.com", true},
		{"https://App.Example.COM:443", "https://app.example.com", true},
		{"https://app.example.com:8443", "https://app.example.com:8443", true},
	} {
		t.Run(tt.value, func(t *testing.T) {
			t.Setenv("TOKENPIVOT_HTTP_ADDR", "127.0.0.1:8080")
			t.Setenv("TOKENPIVOT_DATABASE_URL", "synthetic-sensitive-marker")
			t.Setenv("TOKENPIVOT_WEB_ORIGIN", tt.value)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("accepted origin failed: %v", err)
			}
			if got := cfg.WebOrigin.String(); got != tt.want {
				t.Fatalf("normalized origin = %q, want %q", got, tt.want)
			}
			if cfg.WebOrigin.Secure() != tt.secure {
				t.Fatalf("Secure = %t, want %t", cfg.WebOrigin.Secure(), tt.secure)
			}
		})
	}
}

func TestWebOriginRejected(t *testing.T) {
	for _, value := range []string{
		"http://127.0.0.1:5173/",
		"http://127.0.0.1:5173/path",
		"http://127.0.0.1:5173?query=1",
		"http://127.0.0.1:5173#fragment",
		"http://user:pass@127.0.0.1:5173",
		"ftp://127.0.0.1:5173",
		"http://example.com",
		"http://192.168.0.1:5173",
		"http://127.0.0.1:0",
		"http://127.0.0.1:70000",
		"http://127.0.0.1:http",
		"http://",
		"127.0.0.1:5173",
		" http://127.0.0.1:5173",
		"http://127.0.0.1:5173 ",
		"null",
	} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("TOKENPIVOT_HTTP_ADDR", "127.0.0.1:8080")
			t.Setenv("TOKENPIVOT_DATABASE_URL", "synthetic-sensitive-marker")
			t.Setenv("TOKENPIVOT_WEB_ORIGIN", value)
			_, err := Load()
			if err == nil {
				t.Fatal("invalid origin accepted")
			}
			if !strings.Contains(err.Error(), "TOKENPIVOT_WEB_ORIGIN") || strings.Contains(err.Error(), "synthetic-sensitive-marker") {
				t.Fatalf("unsafe error: %v", err)
			}
		})
	}
}

func TestOriginHeaderMatches(t *testing.T) {
	origin, err := ParseOrigin("https://app.example.com")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name   string
		values []string
		want   bool
	}{
		{"exact", []string{"https://app.example.com"}, true},
		{"case insensitive host", []string{"https://App.Example.com"}, true},
		{"trailing space", []string{"https://app.example.com "}, false},
		{"leading space", []string{" https://app.example.com"}, false},
		{"inner space", []string{"https:// app.example.com"}, false},
		{"tab around", []string{"\thttps://app.example.com\t"}, false},
		{"missing", nil, false},
		{"empty", []string{""}, false},
		{"null", []string{"null"}, false},
		{"multiple", []string{"https://app.example.com", "https://app.example.com"}, false},
		{"scheme differs", []string{"http://app.example.com"}, false},
		{"scheme only", []string{"https"}, false},
		{"host differs", []string{"https://evil.example.com"}, false},
		{"subdomain", []string{"https://api.app.example.com"}, false},
		{"port added", []string{"https://app.example.com:8443"}, false},
		{"path appended", []string{"https://app.example.com/path"}, false},
		{"trailing slash", []string{"https://app.example.com/"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := OriginHeaderMatches(origin, tt.values); got != tt.want {
				t.Fatalf("match = %t, want %t", got, tt.want)
			}
		})
	}
}

// TestOriginHeaderMatchesLoopbackDevelopmentOrigins proves a loopback http origin
// compares scheme, host and port exactly too, and that the explicit port is part
// of the comparison.
func TestOriginHeaderMatchesLoopbackDevelopmentOrigins(t *testing.T) {
	origin, err := ParseOrigin("http://127.0.0.1:5173")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name   string
		values []string
		want   bool
	}{
		{"exact", []string{"http://127.0.0.1:5173"}, true},
		{"localhost is a different host", []string{"http://localhost:5173"}, false},
		{"another loopback address", []string{"http://127.0.0.2:5173"}, false},
		{"port omitted", []string{"http://127.0.0.1"}, false},
		{"other scheme", []string{"https://127.0.0.1:5173"}, false},
		{"whitespace", []string{" http://127.0.0.1:5173"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := OriginHeaderMatches(origin, tt.values); got != tt.want {
				t.Fatalf("match = %t, want %t", got, tt.want)
			}
		})
	}
}

// TestPlainHTTPOriginLimitedToNamedLoopbackHosts pins the development exception:
// only localhost, 127.0.0.1 and ::1 may use plain http. Any other loopback address
// (127.0.0.2, 127.1.2.3, ::2) is refused, while an equivalent spelling of the same
// address still counts as that host.
func TestPlainHTTPOriginLimitedToNamedLoopbackHosts(t *testing.T) {
	for _, value := range []string{
		"http://localhost:5173",
		"http://127.0.0.1:5173",
		"http://[::1]:5173",
		"http://[0:0:0:0:0:0:0:1]:5173",
	} {
		t.Run("accepted "+value, func(t *testing.T) {
			t.Setenv("TOKENPIVOT_HTTP_ADDR", "127.0.0.1:8080")
			t.Setenv("TOKENPIVOT_DATABASE_URL", "synthetic-sensitive-marker")
			t.Setenv("TOKENPIVOT_WEB_ORIGIN", value)
			if _, err := Load(); err != nil {
				t.Fatalf("named loopback origin rejected: %v", err)
			}
		})
	}
	for _, value := range []string{
		"http://127.0.0.2:5173",
		"http://127.1.2.3:5173",
		"http://[::2]:5173",
		"http://LOCALHOST.EXAMPLE.COM:5173",
		"http://localhost.example.com:5173",
	} {
		t.Run("rejected "+value, func(t *testing.T) {
			t.Setenv("TOKENPIVOT_HTTP_ADDR", "127.0.0.1:8080")
			t.Setenv("TOKENPIVOT_DATABASE_URL", "synthetic-sensitive-marker")
			t.Setenv("TOKENPIVOT_WEB_ORIGIN", value)
			_, err := Load()
			if err == nil {
				t.Fatal("non-development plain http origin accepted")
			}
			if !strings.Contains(err.Error(), "TOKENPIVOT_WEB_ORIGIN") || strings.Contains(err.Error(), "synthetic-sensitive-marker") {
				t.Fatalf("unsafe error: %v", err)
			}
		})
	}
	// Production https hosts stay independent of the loopback allowlist.
	for _, value := range []string{"https://app.example.com", "https://127.0.0.2", "https://localhost:8443"} {
		t.Run("accepted https "+value, func(t *testing.T) {
			t.Setenv("TOKENPIVOT_HTTP_ADDR", "127.0.0.1:8080")
			t.Setenv("TOKENPIVOT_DATABASE_URL", "synthetic-sensitive-marker")
			t.Setenv("TOKENPIVOT_WEB_ORIGIN", value)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("https origin rejected: %v", err)
			}
			if !cfg.WebOrigin.Secure() {
				t.Fatal("https origin did not require a Secure cookie")
			}
		})
	}
}
