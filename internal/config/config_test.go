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
