package config

import (
	"log/slog"
	"testing"
	"time"
)

func TestLoad_Defaults(t *testing.T) {
	// t.Setenv sets the variable for this test only and restores it after.
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/db")
	t.Setenv("HTTP_ADDR", "")
	t.Setenv("PORT", "")
	t.Setenv("DB_MAX_CONNS", "")
	t.Setenv("LOG_LEVEL", "")
	t.Setenv("SHUTDOWN_TIMEOUT", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want :8080", cfg.HTTPAddr)
	}
	if cfg.DBMaxConns != 10 {
		t.Errorf("DBMaxConns = %d, want 10", cfg.DBMaxConns)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want INFO", cfg.LogLevel)
	}
	if cfg.ShutdownTimeout != 10*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 10s", cfg.ShutdownTimeout)
	}
}

func TestLoad_Overrides(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/db")
	t.Setenv("HTTP_ADDR", ":9090")
	t.Setenv("DB_MAX_CONNS", "25")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("SHUTDOWN_TIMEOUT", "3s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if cfg.HTTPAddr != ":9090" || cfg.DBMaxConns != 25 ||
		cfg.LogLevel != slog.LevelDebug || cfg.ShutdownTimeout != 3*time.Second {
		t.Errorf("unexpected config: %+v", cfg)
	}
}

func TestLoadAPI_JWTSettings(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/db")
	t.Setenv("JWT_TTL", "")

	t.Setenv("JWT_SECRET", "")
	if _, err := LoadAPI(); err == nil {
		t.Error("LoadAPI() with no JWT_SECRET: error = nil, want error")
	}

	t.Setenv("JWT_SECRET", "too-short")
	if _, err := LoadAPI(); err == nil {
		t.Error("LoadAPI() with short JWT_SECRET: error = nil, want error")
	}

	t.Setenv("JWT_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("JWT_TTL", "15m")
	cfg, err := LoadAPI()
	if err != nil {
		t.Fatalf("LoadAPI() error = %v", err)
	}
	if cfg.JWTTTL != 15*time.Minute {
		t.Errorf("JWTTTL = %v, want 15m", cfg.JWTTTL)
	}

	t.Setenv("JWT_TTL", "forever")
	if _, err := LoadAPI(); err == nil {
		t.Error("LoadAPI() with bad JWT_TTL: error = nil, want error")
	}
}

// The worker must never resolve a PAYMENT_PENDING order while a charge may
// still be in flight, so the grace period has to outlast the payment timeout.
func TestLoad_ReconcileMustOutlastPaymentTimeout(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/db")
	t.Setenv("PAYMENT_TIMEOUT", "30s")
	t.Setenv("PAYMENT_RECONCILE_AFTER", "10s")
	if _, err := Load(); err == nil {
		t.Error("Load() accepted PAYMENT_RECONCILE_AFTER <= PAYMENT_TIMEOUT")
	}

	t.Setenv("PAYMENT_RECONCILE_AFTER", "2m")
	t.Setenv("WORKER_INTERVAL", "5s")
	t.Setenv("WORKER_BATCH_SIZE", "50")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PaymentReconcileAfter != 2*time.Minute || cfg.WorkerInterval != 5*time.Second || cfg.WorkerBatchSize != 50 {
		t.Errorf("unexpected config: %+v", cfg)
	}
}

func TestLoad_Errors(t *testing.T) {
	// Table-driven test: each row is one scenario. This is the most common
	// testing style in Go.
	tests := []struct {
		name string
		env  map[string]string
	}{
		{"missing DATABASE_URL", map[string]string{"DATABASE_URL": ""}},
		{"DB_MAX_CONNS not a number", map[string]string{"DB_MAX_CONNS": "abc"}},
		{"DB_MAX_CONNS zero", map[string]string{"DB_MAX_CONNS": "0"}},
		{"bad LOG_LEVEL", map[string]string{"LOG_LEVEL": "loud"}},
		{"bad SHUTDOWN_TIMEOUT", map[string]string{"SHUTDOWN_TIMEOUT": "ten"}},
		{"OUTBOX_MAX_ATTEMPTS zero", map[string]string{"OUTBOX_MAX_ATTEMPTS": "0"}},
		{"OUTBOX_FAILURE_RATE above 1", map[string]string{"OUTBOX_FAILURE_RATE": "1.5"}},
		{"RATE_LIMIT_PER_MINUTE negative", map[string]string{"RATE_LIMIT_PER_MINUTE": "-1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/db")
			t.Setenv("DB_MAX_CONNS", "")
			t.Setenv("LOG_LEVEL", "")
			t.Setenv("SHUTDOWN_TIMEOUT", "")
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			if _, err := Load(); err == nil {
				t.Fatal("Load() error = nil, want an error")
			}
		})
	}
}

// Hosting platforms (Render, Heroku, Fly) tell the app which port to use
// through PORT. HTTP_ADDR, when set, still wins.
func TestLoad_PortFallback(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/db")
	t.Setenv("HTTP_ADDR", "")
	t.Setenv("PORT", "10000")
	if cfg, err := Load(); err != nil || cfg.HTTPAddr != ":10000" {
		t.Errorf("PORT=10000: HTTPAddr = %q, err = %v; want :10000", cfg.HTTPAddr, err)
	}

	t.Setenv("HTTP_ADDR", ":9090")
	if cfg, _ := Load(); cfg.HTTPAddr != ":9090" {
		t.Errorf("HTTP_ADDR must win over PORT, got %q", cfg.HTTPAddr)
	}
}

func TestLoad_DeploymentSwitches(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/db")
	for _, k := range []string{"TRUSTED_PROXY_HOPS", "MIGRATE_ON_START", "RUN_WORKER"} {
		t.Setenv(k, "")
	}
	cfg, err := Load()
	if err != nil || cfg.TrustedProxyHops != 0 || cfg.MigrateOnStart || cfg.RunWorker {
		t.Fatalf("defaults: %+v, %v; want all off", cfg, err)
	}

	t.Setenv("TRUSTED_PROXY_HOPS", "1")
	t.Setenv("MIGRATE_ON_START", "true")
	t.Setenv("RUN_WORKER", "1")
	cfg, err = Load()
	if err != nil || cfg.TrustedProxyHops != 1 || !cfg.MigrateOnStart || !cfg.RunWorker {
		t.Errorf("set: hops=%d migrate=%v worker=%v err=%v", cfg.TrustedProxyHops, cfg.MigrateOnStart, cfg.RunWorker, err)
	}

	for k, v := range map[string]string{"TRUSTED_PROXY_HOPS": "-1", "MIGRATE_ON_START": "yes please", "RUN_WORKER": "maybe"} {
		t.Run(k, func(t *testing.T) {
			t.Setenv(k, v)
			if _, err := Load(); err == nil {
				t.Errorf("%s=%q: expected an error", k, v)
			}
		})
	}
}
