// Package config loads the service's startup configuration.
package config

import (
	"errors"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Config contains startup settings; DatabaseURL must never be logged.
type Config struct {
	HTTPAddr    string
	DatabaseURL string
	// WebOrigin is the single trusted browser origin. It decides Origin checks
	// for write requests and whether the session cookie is Secure.
	WebOrigin Origin
	// RegistrationOpen decides whether accounts may be created after the first
	// one. It is read once at startup and never reloaded; the service keeps it as
	// an immutable startup setting.
	RegistrationOpen bool
}

// Load reads the service's environment once and validates it. Invalid values are
// never included in returned errors.
func Load() (Config, error) {
	addr, ok := os.LookupEnv("TOKENPIVOT_HTTP_ADDR")
	if !ok {
		addr = "127.0.0.1:8080"
	}
	_, port, err := net.SplitHostPort(addr)
	validPort := port != ""
	for _, c := range port {
		if c < '0' || c > '9' {
			validPort = false
		}
	}
	n, portErr := strconv.Atoi(port)
	if err != nil || !validPort || portErr != nil || n < 1 || n > 65535 {
		return Config{}, errors.New("TOKENPIVOT_HTTP_ADDR must be host:port with a numeric port from 1 to 65535")
	}
	databaseURL := os.Getenv("TOKENPIVOT_DATABASE_URL")
	if strings.TrimSpace(databaseURL) == "" {
		return Config{}, errors.New("TOKENPIVOT_DATABASE_URL is required")
	}
	allowInsecureHTTP, err := strictBoolSetting("TOKENPIVOT_ALLOW_INSECURE_HTTP")
	if err != nil {
		return Config{}, err
	}
	origin, err := ParseOrigin(os.Getenv("TOKENPIVOT_WEB_ORIGIN"), allowInsecureHTTP)
	if err != nil {
		return Config{}, err
	}
	registrationOpen, err := strictBoolSetting("TOKENPIVOT_REGISTRATION_OPEN")
	if err != nil {
		return Config{}, err
	}
	return Config{HTTPAddr: addr, DatabaseURL: databaseURL, WebOrigin: origin, RegistrationOpen: registrationOpen}, nil
}

// strictBoolSetting reads a boolean environment variable once. Unset means
// false; only the exact spellings "true" and "false" are accepted, so an empty
// value, another case, surrounding whitespace, "1" or "yes" all fail at startup
// instead of being interpreted differently by two deployments. The returned error
// names the variable and its allowed values and never repeats the rejected value.
func strictBoolSetting(name string) (bool, error) {
	value, ok := os.LookupEnv(name)
	if !ok {
		return false, nil
	}
	switch value {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, errors.New(name + ` must be exactly "true" or "false"`)
	}
}

// Origin is one normalized trusted browser origin: a lowercase scheme and a
// lowercase host with the scheme's default port omitted. It never carries user
// information, a path, a query or a fragment.
type Origin struct {
	scheme string
	host   string
}

// String returns the normalized origin, e.g. "http://127.0.0.1:5173".
func (o Origin) String() string { return o.scheme + "://" + o.host }

// Scheme returns the normalized scheme, either "http" or "https".
func (o Origin) Scheme() string { return o.scheme }

// Secure reports whether cookies for this origin must set the Secure attribute:
// every https origin requires it, while plain http origins (loopback development
// or explicitly allowed by TOKENPIVOT_ALLOW_INSECURE_HTTP) cannot.
func (o Origin) Secure() bool { return o.scheme == "https" }

// ParseOrigin validates and normalizes TOKENPIVOT_WEB_ORIGIN. Only one http(s)
// origin is accepted; plain http is limited to loopback development hosts unless
// allowInsecureHTTP is true, which only lifts that restriction. Returned errors name the variable but never echo its value.
func ParseOrigin(value string, allowInsecureHTTP bool) (Origin, error) {
	if value == "" || strings.TrimSpace(value) != value {
		return Origin{}, errors.New("TOKENPIVOT_WEB_ORIGIN is required and must not have surrounding whitespace")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Opaque != "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return Origin{}, errors.New("TOKENPIVOT_WEB_ORIGIN must be one http(s) origin without user information, path, query or fragment")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return Origin{}, errors.New("TOKENPIVOT_WEB_ORIGIN scheme must be http or https")
	}
	hostname := strings.ToLower(parsed.Hostname())
	if hostname == "" || strings.HasSuffix(value, "/") {
		return Origin{}, errors.New("TOKENPIVOT_WEB_ORIGIN must include a host and no trailing slash")
	}
	port := parsed.Port()
	if port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return Origin{}, errors.New("TOKENPIVOT_WEB_ORIGIN port must be numeric from 1 to 65535")
		}
		if (scheme == "http" && number == 80) || (scheme == "https" && number == 443) {
			port = ""
		}
	}
	if scheme == "http" && !allowInsecureHTTP && !isLoopbackHost(hostname) {
		return Origin{}, errors.New("TOKENPIVOT_WEB_ORIGIN may use plain http only for localhost, 127.0.0.1 or ::1 unless TOKENPIVOT_ALLOW_INSECURE_HTTP is set to true")
	}
	host := hostname
	if port != "" {
		host = net.JoinHostPort(hostname, port)
	} else if strings.Contains(hostname, ":") {
		host = "[" + hostname + "]"
	}
	return Origin{scheme: scheme, host: host}, nil
}

// OriginHeaderMatches reports whether a request's single Origin header equals
// the trusted origin exactly, comparing scheme, host and port. The value is never
// trimmed: surrounding whitespace is part of the value a proxy sees, so a padded
// header must not be accepted.
func OriginHeaderMatches(origin Origin, headerValues []string) bool {
	return len(headerValues) == 1 && strings.EqualFold(headerValues[0], origin.String())
}

// isLoopbackHost accepts only the three named development loopback hosts. The
// rest of the 127.0.0.0/8 range and every other loopback address (127.0.0.2,
// ::2, ...) are not development origins and are refused; an equivalent spelling
// of the same address, such as the expanded form of ::1, still means that host.
func isLoopbackHost(hostname string) bool {
	if hostname == "localhost" {
		return true
	}
	ip := net.ParseIP(hostname)
	return ip != nil && (ip.Equal(net.IPv4(127, 0, 0, 1)) || ip.Equal(net.IPv6loopback))
}

// MigrationConfig contains only the independent migration connection setting.
// It must never be logged.
type MigrationConfig struct{ DatabaseURL string }

// LoadMigration never falls back to the service credentials or reads HTTP settings.
func LoadMigration() (MigrationConfig, error) {
	value := os.Getenv("TOKENPIVOT_MIGRATION_DATABASE_URL")
	if strings.TrimSpace(value) == "" {
		return MigrationConfig{}, errors.New("TOKENPIVOT_MIGRATION_DATABASE_URL is required")
	}
	return MigrationConfig{DatabaseURL: value}, nil
}
