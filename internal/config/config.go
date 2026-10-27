// Package config loads the service's startup configuration.
package config

import (
	"errors"
	"net"
	"os"
	"strconv"
)

// Config contains the HTTP listener address.
type Config struct {
	HTTPAddr string
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
	return Config{HTTPAddr: addr}, nil
}
