// Package config loads syncd's configuration from environment variables —
// never from source or flags, so the bearer token can live in a root-only
// env file (deploy/README.md) and nothing secret is ever committed.
package config

import (
	"fmt"
	"os"
	"strconv"
)

// Config is everything syncd needs to run.
type Config struct {
	// Addr is the listen address (SYNCD_ADDR, default ":8787"). On the home
	// server this can stay a wildcard bind — Tailscale is the only network
	// path in (deploy notes) — or be pinned to the tailnet IP.
	Addr string
	// Token is the required bearer token (SYNCD_TOKEN).
	Token string
	// DataDir is where archives are stored (SYNCD_DATA_DIR, default "./data").
	DataDir string
	// RetentionDailyDays is the ring-buffer daily window
	// (SYNCD_RETENTION_DAYS, default 30 — the T20 rule; change only if the
	// app-side rule changes too).
	RetentionDailyDays int
	// MaxBodyBytes caps PUT /backup bodies (SYNCD_MAX_BODY_MB, default 64).
	MaxBodyBytes int64
	// DatabaseURL is the Postgres connection string (SYNCD_DATABASE_URL);
	// required only when the /v1 sync API is enabled.
	DatabaseURL string
	// FirebaseProjectID enables the /v1 sync API (SYNCD_FIREBASE_PROJECT_ID).
	// Empty means /v1 is disabled and its routes 404 (the old deploy).
	FirebaseProjectID string
	// MaxPushBytes caps POST /v1/sync/push bodies (SYNCD_MAX_PUSH_BYTES,
	// default 5 MiB).
	MaxPushBytes int64
}

// SyncEnabled reports whether the /v1 sync API is configured.
func (c Config) SyncEnabled() bool { return c.FirebaseProjectID != "" }

// MinTokenLen guards against an accidentally empty or trivial token; the
// deploy notes generate a 64-hex-char one.
const MinTokenLen = 16

// FromEnv reads and validates configuration from the environment.
func FromEnv() (Config, error) {
	cfg := Config{
		Addr:               envOr("SYNCD_ADDR", ":8787"),
		Token:              os.Getenv("SYNCD_TOKEN"),
		DataDir:            envOr("SYNCD_DATA_DIR", "./data"),
		RetentionDailyDays: 30,
		MaxBodyBytes:       64 << 20,
		DatabaseURL:        os.Getenv("SYNCD_DATABASE_URL"),
		FirebaseProjectID:  os.Getenv("SYNCD_FIREBASE_PROJECT_ID"),
		MaxPushBytes:       5 << 20,
	}
	if len(cfg.Token) < MinTokenLen {
		return Config{}, fmt.Errorf("SYNCD_TOKEN must be set (>= %d chars)", MinTokenLen)
	}
	if v := os.Getenv("SYNCD_RETENTION_DAYS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return Config{}, fmt.Errorf("SYNCD_RETENTION_DAYS must be a positive integer, got %q", v)
		}
		cfg.RetentionDailyDays = n
	}
	if v := os.Getenv("SYNCD_MAX_BODY_MB"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return Config{}, fmt.Errorf("SYNCD_MAX_BODY_MB must be a positive integer, got %q", v)
		}
		cfg.MaxBodyBytes = int64(n) << 20
	}
	if v := os.Getenv("SYNCD_MAX_PUSH_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			return Config{}, fmt.Errorf("SYNCD_MAX_PUSH_BYTES must be a positive integer, got %q", v)
		}
		cfg.MaxPushBytes = n
	}
	if cfg.SyncEnabled() && cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("SYNCD_DATABASE_URL must be set when SYNCD_FIREBASE_PROJECT_ID is set")
	}
	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
