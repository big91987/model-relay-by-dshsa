// Package config loads runtime configuration from the environment.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config holds every runtime setting of the gateway.
type Config struct {
	Port int
	// Host is the HTTP listen address host. Empty means all interfaces for a
	// normal deployment; TEST_MODE defaults it to loopback.
	Host    string
	DataDir string
	// DBPath is the SQLite file used when DatabaseURL is empty.
	DBPath string
	// DatabaseURL selects PostgreSQL when non-empty. SQLite remains the default
	// so existing single-file deployments keep working unchanged.
	DatabaseURL    string
	SecretKey      string
	AdminUsername  string
	AdminPassword  string
	SessionTTL     time.Duration
	RequestTimeout time.Duration
	// StreamTimeout caps how long a streaming request may hold a quota
	// reservation. It is a hard limit, not an idle timeout.
	StreamTimeout time.Duration
	// QuotaDefaultOutputTokens is the output-token cap applied to finite-quota
	// keys when a chat request omits max_tokens/max_completion_tokens. It gives
	// the reservation a trustworthy upper bound.
	QuotaDefaultOutputTokens int
	VideoPollInterval        time.Duration
	SpeechPollInterval       time.Duration
	// MusicTimeout bounds a synchronous MiniMax Music generation call. Songs
	// can take several minutes, so it is much longer than RequestTimeout.
	MusicTimeout time.Duration
	Version      string

	// TestMode explicitly enables the local test environment: the initial admin
	// account defaults to admin/admin and the console defaults to Chinese. It
	// must never be enabled for a production deployment.
	TestMode bool

	// UsingDefaults reports settings still on insecure development defaults.
	UsingDefaultSecret   bool
	UsingDefaultPassword bool
}

// UsingPostgres reports whether PostgreSQL was selected through DATABASE_URL.
func (c *Config) UsingPostgres() bool { return c.DatabaseURL != "" }

// ListenAddress returns the host:port the HTTP server binds.
func (c *Config) ListenAddress() string {
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}

// BindsLoopback reports whether Host restricts the listener to this machine.
func (c *Config) BindsLoopback() bool {
	host := strings.TrimSpace(strings.ToLower(c.Host))
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Load reads configuration from environment variables, applying safe defaults.
func Load(version string) (*Config, error) {
	cfg := &Config{
		Port:                     envInt("PORT", 8080),
		Host:                     strings.Trim(strings.TrimSpace(env("HOST", "")), "[]"),
		DataDir:                  env("DATA_DIR", "./data"),
		SecretKey:                env("SECRET_KEY", ""),
		AdminUsername:            env("ADMIN_USERNAME", "admin"),
		AdminPassword:            env("ADMIN_PASSWORD", ""),
		SessionTTL:               time.Duration(envInt("SESSION_TTL_HOURS", 24)) * time.Hour,
		RequestTimeout:           time.Duration(envInt("REQUEST_TIMEOUT_SECONDS", 300)) * time.Second,
		StreamTimeout:            time.Duration(envInt("STREAM_TIMEOUT_SECONDS", 1800)) * time.Second,
		QuotaDefaultOutputTokens: envInt("QUOTA_DEFAULT_OUTPUT_TOKENS", 4096),
		VideoPollInterval:        time.Duration(envInt("VIDEO_POLL_SECONDS", 8)) * time.Second,
		SpeechPollInterval:       time.Duration(envInt("SPEECH_POLL_SECONDS", 5)) * time.Second,
		MusicTimeout:             time.Duration(envInt("MUSIC_TIMEOUT_SECONDS", 600)) * time.Second,
		Version:                  version,
		TestMode:                 envBool("TEST_MODE", false),
	}
	if cfg.Port <= 0 || cfg.Port > 65535 {
		return nil, fmt.Errorf("invalid PORT %d", cfg.Port)
	}
	if cfg.QuotaDefaultOutputTokens < 1 {
		return nil, fmt.Errorf("QUOTA_DEFAULT_OUTPUT_TOKENS must be greater than zero")
	}
	if cfg.MusicTimeout <= 0 {
		return nil, fmt.Errorf("MUSIC_TIMEOUT_SECONDS must be greater than zero")
	}
	if strings.Contains(cfg.Host, ":") && net.ParseIP(cfg.Host) == nil {
		return nil, fmt.Errorf("HOST must be a hostname or IP address without a port")
	}
	// The explicitly enabled local test environment must not expose the
	// admin/admin login on every interface by default. An explicit HOST still
	// wins so a test instance can be reached from another machine on purpose.
	if cfg.Host == "" && cfg.TestMode {
		cfg.Host = "127.0.0.1"
	}
	if raw := os.Getenv("DATABASE_URL"); raw != "" {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
			return nil, fmt.Errorf("DATABASE_URL must be a postgres:// or postgresql:// URL with a host")
		}
		cfg.DatabaseURL = raw
	}
	if cfg.UsingDefaultSecret = cfg.SecretKey == ""; cfg.UsingDefaultSecret {
		cfg.SecretKey = "dev-secret-change-me"
	}
	if cfg.AdminPassword == "" {
		cfg.UsingDefaultPassword = true
		if cfg.TestMode {
			// Only the explicitly enabled local test environment gets admin/admin.
			cfg.AdminUsername = env("ADMIN_USERNAME", "admin")
			cfg.AdminPassword = "admin"
		} else {
			// Preserve the original production default and its startup warning;
			// existing single-file installs keep working without new variables.
			cfg.AdminPassword = "admin123"
		}
	}
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	if sqlitePath := strings.TrimSpace(os.Getenv("SQLITE_PATH")); sqlitePath != "" {
		cfg.DBPath = sqlitePath
		if dir := filepath.Dir(sqlitePath); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o750); err != nil {
				return nil, fmt.Errorf("create sqlite path dir: %w", err)
			}
		}
	} else {
		// Preserve the original DATA_DIR/gateway.db location when SQLITE_PATH is
		// unset, so existing single-file installs keep their data.
		cfg.DBPath = filepath.Join(cfg.DataDir, "gateway.db")
	}
	return cfg, nil
}

func envBool(key string, fallback bool) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if v == "" {
		return fallback
	}
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
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
