// Package config loads runtime configuration from the environment.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Config holds every runtime setting of the gateway.
type Config struct {
	Port              int
	DataDir           string
	DBPath            string
	SecretKey         string
	AdminUsername     string
	AdminPassword     string
	SessionTTL        time.Duration
	RequestTimeout    time.Duration
	VideoPollInterval time.Duration
	Version           string

	// UsingDefaults reports settings still on insecure development defaults.
	UsingDefaultSecret   bool
	UsingDefaultPassword bool
}

// Load reads configuration from environment variables, applying safe defaults.
func Load(version string) (*Config, error) {
	cfg := &Config{
		Port:              envInt("PORT", 8080),
		DataDir:           env("DATA_DIR", "./data"),
		SecretKey:         env("SECRET_KEY", ""),
		AdminUsername:     env("ADMIN_USERNAME", "admin"),
		AdminPassword:     env("ADMIN_PASSWORD", ""),
		SessionTTL:        time.Duration(envInt("SESSION_TTL_HOURS", 24)) * time.Hour,
		RequestTimeout:    time.Duration(envInt("REQUEST_TIMEOUT_SECONDS", 300)) * time.Second,
		VideoPollInterval: time.Duration(envInt("VIDEO_POLL_SECONDS", 8)) * time.Second,
		Version:           version,
	}
	if cfg.Port <= 0 || cfg.Port > 65535 {
		return nil, fmt.Errorf("invalid PORT %d", cfg.Port)
	}
	if cfg.UsingDefaultSecret = cfg.SecretKey == ""; cfg.UsingDefaultSecret {
		cfg.SecretKey = "dev-secret-change-me"
	}
	if cfg.UsingDefaultPassword = cfg.AdminPassword == ""; cfg.UsingDefaultPassword {
		cfg.AdminPassword = "admin123"
	}
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	cfg.DBPath = filepath.Join(cfg.DataDir, "gateway.db")
	return cfg, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}
