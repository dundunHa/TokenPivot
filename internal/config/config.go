// Package config loads the service's startup configuration.
package config

import (
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
)

// Config contains startup settings; DatabaseURL must never be logged.
type Config struct {
	HTTPAddr    string
	DatabaseURL string
}

// Load reads TOKENPIVOT_HTTP_ADDR once and validates its host:port format.
// Invalid values are never included in returned errors.
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
	return Config{HTTPAddr: addr, DatabaseURL: databaseURL}, nil
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
