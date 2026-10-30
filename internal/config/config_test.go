package config

import (
	"os"
	"strings"
	"testing"
)

// setLoadableEnv provides the smallest valid environment, so a test can vary one
// variable at a time.
func setLoadableEnv(t *testing.T) {
	t.Helper()
	t.Setenv("TOKENPIVOT_HTTP_ADDR", "127.0.0.1:8080")
	t.Setenv("TOKENPIVOT_DATABASE_URL", "synthetic-sensitive-marker")
	t.Setenv("TOKENPIVOT_WEB_ORIGIN", "http://127.0.0.1:5173")
}

// TestRegistrationOpenDefault covers the deployment default: an unset variable
// keeps public registration closed.
func TestRegistrationOpenDefault(t *testing.T) {
	setLoadableEnv(t)
	t.Setenv("TOKENPIVOT_REGISTRATION_OPEN", "")
	if err := os.Unsetenv("TOKENPIVOT_REGISTRATION_OPEN"); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RegistrationOpen {
		t.Fatal("registration is open without the variable being set")
	}
}

// TestRegistrationOpenAccepted pins the two accepted spellings.
func TestRegistrationOpenAccepted(t *testing.T) {
	for value, want := range map[string]bool{"true": true, "false": false} {
		t.Run(value, func(t *testing.T) {
			setLoadableEnv(t)
			t.Setenv("TOKENPIVOT_REGISTRATION_OPEN", value)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("accepted value failed: %v", err)
			}
			if cfg.RegistrationOpen != want {
				t.Fatalf("RegistrationOpen = %t, want %t", cfg.RegistrationOpen, want)
			}
		})
	}
}

// TestRegistrationOpenRejected pins the strict spellings and proves the failure
// names the variable and its allowed values without echoing what was supplied.
func TestRegistrationOpenRejected(t *testing.T) {
	for name, value := range map[string]string{
		"empty":             "",
		"blank":             " ",
		"surrounding space": " true ",
		"uppercase":         "TRUE",
		"title case":        "True",
		"numeric":           "1",
		"yes":               "yes",
		"off":               "off",
		"true with suffix":  "true1",
	} {
		t.Run(name, func(t *testing.T) {
			setLoadableEnv(t)
			t.Setenv("TOKENPIVOT_REGISTRATION_OPEN", value)
			_, err := Load()
			if err == nil {
				t.Fatalf("value %q was accepted", value)
			}
			message := err.Error()
			if !strings.Contains(message, "TOKENPIVOT_REGISTRATION_OPEN") {
				t.Fatalf("error does not name the variable: %v", err)
			}
			if !strings.Contains(message, `"true"`) || !strings.Contains(message, `"false"`) {
				t.Fatalf("error does not list the allowed values: %v", err)
			}
			if strings.Contains(message, "synthetic-sensitive-marker") {
				t.Fatalf("error echoed another variable's value: %v", err)
			}
			// A whitespace-only value cannot be searched for meaningfully (the
			// message itself contains spaces), so it is only checked above for
			// being refused.
			if strings.TrimSpace(value) != "" && strings.Contains(message, value) {
				t.Fatalf("error echoed the rejected value %q: %v", value, err)
			}
		})
	}
}

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
	origin, err := ParseOrigin("https://app.example.com", false)
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
	origin, err := ParseOrigin("http://127.0.0.1:5173", false)
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

// TestAllowInsecureHTTPSetting pins the explicit non-loopback http switch: it is
// off by default, accepts only exact true/false and never echoes a bad value.
func TestAllowInsecureHTTPSetting(t *testing.T) {
	const origin = "http://DESKTOP-C28DFS7:18080"
	t.Run("default rejects non-loopback http", func(t *testing.T) {
		setLoadableEnv(t)
		t.Setenv("TOKENPIVOT_WEB_ORIGIN", origin)
		if _, err := Load(); err == nil {
			t.Fatal("non-loopback http accepted without the switch")
		}
	})
	t.Run("false rejects non-loopback http", func(t *testing.T) {
		setLoadableEnv(t)
		t.Setenv("TOKENPIVOT_WEB_ORIGIN", origin)
		t.Setenv("TOKENPIVOT_ALLOW_INSECURE_HTTP", "false")
		if _, err := Load(); err == nil {
			t.Fatal("non-loopback http accepted with false")
		}
	})
	t.Run("true allows it with a non-Secure cookie", func(t *testing.T) {
		setLoadableEnv(t)
		t.Setenv("TOKENPIVOT_WEB_ORIGIN", origin)
		t.Setenv("TOKENPIVOT_ALLOW_INSECURE_HTTP", "true")
		cfg, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.WebOrigin.String() != "http://desktop-c28dfs7:18080" || cfg.WebOrigin.Secure() {
			t.Fatalf("origin = %q secure = %t", cfg.WebOrigin.String(), cfg.WebOrigin.Secure())
		}
		if cfg.RegistrationOpen {
			t.Fatal("the switch opened registration")
		}
	})
	t.Run("true keeps the other origin rules", func(t *testing.T) {
		for _, value := range []string{"http://host:18080/", "http://host:18080/path", "http://user@host:18080", "http://host:0", "ftp://host", "http://host:18080?q=1", " http://host:18080"} {
			setLoadableEnv(t)
			t.Setenv("TOKENPIVOT_ALLOW_INSECURE_HTTP", "true")
			t.Setenv("TOKENPIVOT_WEB_ORIGIN", value)
			if _, err := Load(); err == nil {
				t.Fatalf("invalid origin %q accepted", value)
			}
		}
	})
	t.Run("invalid values fail without echo", func(t *testing.T) {
		for _, value := range []string{"", "TRUE", " true", "true ", "1", "yes"} {
			setLoadableEnv(t)
			t.Setenv("TOKENPIVOT_ALLOW_INSECURE_HTTP", value)
			_, err := Load()
			if err == nil {
				t.Fatalf("value %q accepted", value)
			}
			if !strings.Contains(err.Error(), "TOKENPIVOT_ALLOW_INSECURE_HTTP") || (value != "" && strings.Contains(err.Error(), value)) {
				t.Fatalf("unsafe error: %v", err)
			}
		}
	})
}

func TestDisableOriginCheckSetting(t *testing.T) {
	load := func() (Config, error) {
		t.Setenv("TOKENPIVOT_DATABASE_URL", "postgres://synthetic")
		t.Setenv("TOKENPIVOT_WEB_ORIGIN", "http://127.0.0.1:5173")
		return Load()
	}
	cfg, err := load()
	if err != nil || cfg.DisableOriginCheck {
		t.Fatalf("default: %+v %v", cfg, err)
	}
	for value, want := range map[string]bool{"true": true, "false": false} {
		t.Setenv("TOKENPIVOT_DISABLE_ORIGIN_CHECK", value)
		cfg, err := load()
		if err != nil || cfg.DisableOriginCheck != want {
			t.Fatalf("%q: %+v %v", value, cfg, err)
		}
	}
	for _, value := range []string{"", "TRUE", "1", "yes", " true", "synthetic-sensitive-marker"} {
		t.Setenv("TOKENPIVOT_DISABLE_ORIGIN_CHECK", value)
		_, err := load()
		if err == nil || !strings.Contains(err.Error(), "TOKENPIVOT_DISABLE_ORIGIN_CHECK") || strings.Contains(err.Error(), "synthetic-sensitive-marker") {
			t.Fatalf("%q: err = %v", value, err)
		}
	}
}
