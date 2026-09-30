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

	// JWTSecret signs access tokens. Anyone who knows it can forge a token
	// for any user, so it must be long, random and kept out of git.
	JWTSecret string

	// JWTTTL is how long an access token stays valid after login.
	JWTTTL time.Duration

	// ReservationTTL is how long stock stays reserved for an unpaid order
	// before the expiry worker (Stage 10) may release it.
	ReservationTTL time.Duration

	// PaymentTimeout is how long we wait for the payment provider before
	// treating the outcome as unknown.
	PaymentTimeout time.Duration

	// MockPaymentOutcome is the mock provider's default behaviour:
	// SUCCESS, FAILURE or TIMEOUT.
	MockPaymentOutcome string

	// PaymentReconcileAfter is how long after a reservation expired the
	// worker waits before resolving a PAYMENT_PENDING order with the
	// provider. Must be longer than PaymentTimeout.
	PaymentReconcileAfter time.Duration

	// WorkerInterval is how often the worker runs its jobs.
	WorkerInterval time.Duration

	// WorkerBatchSize caps how many orders one job run handles.
	WorkerBatchSize int
}

// minJWTSecretLen: HMAC-SHA256 keys shorter than 32 bytes (256 bits) are
// weaker than the algorithm itself and easier to brute-force.
const minJWTSecretLen = 32

// Load reads the configuration from environment variables.
// It returns an error (instead of panicking) so main() decides what to do.
func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:              getEnv("HTTP_ADDR", ":8080"),
		DatabaseURL:           os.Getenv("DATABASE_URL"),
		DBMaxConns:            10,
		LogLevel:              slog.LevelInfo,
		ShutdownTimeout:       10 * time.Second,
		JWTSecret:             os.Getenv("JWT_SECRET"),
		JWTTTL:                time.Hour,
		ReservationTTL:        15 * time.Minute,
		PaymentTimeout:        5 * time.Second,
		MockPaymentOutcome:    "SUCCESS",
		PaymentReconcileAfter: time.Minute,
		WorkerInterval:        10 * time.Second,
		WorkerBatchSize:       100,
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

	if v := os.Getenv("RESERVATION_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Second {
			return Config{}, fmt.Errorf("RESERVATION_TTL must be a duration of at least 1s like 15m, got %q", v)
		}
		cfg.ReservationTTL = d
	}

	if v := os.Getenv("PAYMENT_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("PAYMENT_TIMEOUT must be a positive duration like 5s, got %q", v)
		}
		cfg.PaymentTimeout = d
	}

	if v := os.Getenv("MOCK_PAYMENT_OUTCOME"); v != "" {
		v = strings.ToUpper(v)
		if v != "SUCCESS" && v != "FAILURE" && v != "TIMEOUT" {
			return Config{}, fmt.Errorf("MOCK_PAYMENT_OUTCOME must be SUCCESS, FAILURE or TIMEOUT, got %q", v)
		}
		cfg.MockPaymentOutcome = v
	}

	if v := os.Getenv("PAYMENT_RECONCILE_AFTER"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("PAYMENT_RECONCILE_AFTER must be a positive duration like 1m, got %q", v)
		}
		cfg.PaymentReconcileAfter = d
	}
	// Safety rule, not just validation: if the worker resolved a payment
	// while a charge could still be in flight, it might expire an order
	// the customer is being charged for right now.
	if cfg.PaymentReconcileAfter <= cfg.PaymentTimeout {
		return Config{}, fmt.Errorf("PAYMENT_RECONCILE_AFTER (%s) must be longer than PAYMENT_TIMEOUT (%s)",
			cfg.PaymentReconcileAfter, cfg.PaymentTimeout)
	}

	if v := os.Getenv("WORKER_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 100*time.Millisecond {
			return Config{}, fmt.Errorf("WORKER_INTERVAL must be a duration of at least 100ms like 10s, got %q", v)
		}
		cfg.WorkerInterval = d
	}

	if v := os.Getenv("WORKER_BATCH_SIZE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 10000 {
			return Config{}, fmt.Errorf("WORKER_BATCH_SIZE must be between 1 and 10000, got %q", v)
		}
		cfg.WorkerBatchSize = n
	}

	return cfg, nil
}

// LoadAPI is Load plus the settings only the API server needs (the migrate
// command has no use for a JWT secret, so it must not be forced to set one).
func LoadAPI() (Config, error) {
	cfg, err := Load()
	if err != nil {
		return Config{}, err
	}

	if len(cfg.JWTSecret) < minJWTSecretLen {
		return Config{}, fmt.Errorf("JWT_SECRET is required and must be at least %d characters", minJWTSecretLen)
	}

	if v := os.Getenv("JWT_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("JWT_TTL must be a positive duration like 1h, got %q", v)
		}
		cfg.JWTTTL = d
	}

	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
