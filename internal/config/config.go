// Package config loads application settings from environment variables.
//
// Why environment variables? The same Docker image can run on a laptop,
// in CI and in production; only the environment changes. Secrets (database
// passwords, JWT keys) never live in the source code.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds every setting the application needs.
// It is loaded once at startup and then passed to the parts that need it
// (dependency injection) instead of being read from a global variable.
type Config struct {
	// HTTPAddr is the address the API server listens on, e.g. ":8080".
	HTTPAddr string

	// DatabaseURL is the PostgreSQL connection string, e.g.
	// postgres://user:password@localhost:5432/inventory?sslmode=disable
	DatabaseURL string

	// DBMaxConns limits how many connections the pool may open.
	DBMaxConns int32

	// LogLevel is one of: debug, info, warn, error.
	LogLevel slog.Level

	// ShutdownTimeout is how long we wait for in-flight requests to finish
	// when the process is asked to stop.
	ShutdownTimeout time.Duration
}

// Load reads the configuration from environment variables.
// It returns an error (instead of panicking) so main() decides what to do.
func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:        getEnv("HTTP_ADDR", ":8080"),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		DBMaxConns:      10,
		LogLevel:        slog.LevelInfo,
		ShutdownTimeout: 10 * time.Second,
	}

	// Required values: there is no safe default for a database password.
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}

	if v := os.Getenv("DB_MAX_CONNS"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n < 1 {
			return Config{}, fmt.Errorf("DB_MAX_CONNS must be a positive integer, got %q", v)
		}
		cfg.DBMaxConns = int32(n)
	}

	if v := os.Getenv("LOG_LEVEL"); v != "" {
		var level slog.Level
		if err := level.UnmarshalText([]byte(strings.ToUpper(v))); err != nil {
			return Config{}, fmt.Errorf("LOG_LEVEL must be debug, info, warn or error, got %q", v)
		}
		cfg.LogLevel = level
	}

	if v := os.Getenv("SHUTDOWN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("SHUTDOWN_TIMEOUT must be a positive duration like 10s, got %q", v)
		}
		cfg.ShutdownTimeout = d
	}

	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
