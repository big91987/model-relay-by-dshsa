package config

import (
	"os"
	"path/filepath"
	"testing"
)

// baseEnv isolates a test from ambient environment variables and returns the
// configured DATA_DIR.
func baseEnv(t *testing.T) string {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), "data")
	t.Setenv("DATA_DIR", dataDir)
	t.Setenv("PORT", "8080")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("TEST_MODE", "")
	t.Setenv("SECRET_KEY", "")
	t.Setenv("ADMIN_USERNAME", "")
	t.Setenv("ADMIN_PASSWORD", "")
	t.Setenv("HOST", "")
	t.Setenv("SQLITE_PATH", "")
	return dataDir
}

func TestLoadDefaultsToSQLite(t *testing.T) {
	dataDir := baseEnv(t)
	cfg, err := Load("test")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.UsingPostgres() {
		t.Fatal("SQLite must stay the default when DATABASE_URL is unset")
	}
	if cfg.TestMode {
		t.Fatal("TEST_MODE must default to off")
	}
	if cfg.AdminUsername != "admin" || cfg.AdminPassword != "admin123" {
		t.Fatalf("production defaults changed: %q/%q", cfg.AdminUsername, cfg.AdminPassword)
	}
	if !cfg.UsingDefaultPassword {
		t.Fatal("default password must be flagged")
	}
	if cfg.DBPath != filepath.Join(dataDir, "gateway.db") {
		t.Fatalf("default DBPath = %q, want DATA_DIR/gateway.db", cfg.DBPath)
	}
	if cfg.Host != "" || cfg.ListenAddress() != ":8080" {
		t.Fatalf("normal deployment must keep the all-interface listener, got host=%q addr=%q", cfg.Host, cfg.ListenAddress())
	}
	if cfg.BindsLoopback() {
		t.Fatal("an empty HOST must not report loopback-only binding")
	}
}

func TestLoadTestModeDefaultsToLoopback(t *testing.T) {
	baseEnv(t)
	t.Setenv("TEST_MODE", "1")
	cfg, err := Load("test")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.TestMode {
		t.Fatal("TEST_MODE=1 must enable test mode")
	}
	if cfg.AdminUsername != "admin" || cfg.AdminPassword != "admin" {
		t.Fatalf("test mode should default to admin/admin, got %q/%q", cfg.AdminUsername, cfg.AdminPassword)
	}
	if cfg.Host != "127.0.0.1" || cfg.ListenAddress() != "127.0.0.1:8080" {
		t.Fatalf("test mode must default to loopback, got host=%q addr=%q", cfg.Host, cfg.ListenAddress())
	}
	if !cfg.BindsLoopback() {
		t.Fatal("test mode default must report loopback-only binding")
	}
}

func TestLoadExplicitHostWins(t *testing.T) {
	baseEnv(t)
	t.Setenv("TEST_MODE", "1")
	t.Setenv("HOST", "0.0.0.0")
	cfg, err := Load("test")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Host != "0.0.0.0" {
		t.Fatalf("explicit HOST was ignored: %q", cfg.Host)
	}
	if cfg.BindsLoopback() {
		t.Fatal("0.0.0.0 must not report loopback-only binding")
	}
}

func TestLoadRejectsHostWithPort(t *testing.T) {
	baseEnv(t)
	t.Setenv("HOST", "127.0.0.1:8080")
	if _, err := Load("test"); err == nil {
		t.Fatal("Load accepted HOST with a port; HOST must be the host only")
	}
}

func TestListenAddressHandlesIPv6AndPort(t *testing.T) {
	baseEnv(t)
	t.Setenv("HOST", "::1")
	t.Setenv("PORT", "9090")
	cfg, err := Load("test")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddress() != "[::1]:9090" {
		t.Fatalf("IPv6 listen address = %q", cfg.ListenAddress())
	}
	if !cfg.BindsLoopback() {
		t.Fatal("::1 must report loopback-only binding")
	}
}

func TestLoadExplicitAdminPasswordBeatsTestDefaults(t *testing.T) {
	baseEnv(t)
	t.Setenv("TEST_MODE", "1")
	t.Setenv("ADMIN_USERNAME", "operator")
	t.Setenv("ADMIN_PASSWORD", "strong-password")
	cfg, err := Load("test")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AdminUsername != "operator" || cfg.AdminPassword != "strong-password" {
		t.Fatalf("explicit credentials were ignored: %q/%q", cfg.AdminUsername, cfg.AdminPassword)
	}
	if cfg.UsingDefaultPassword {
		t.Fatal("explicit password must not be flagged as a default")
	}
}

func TestLoadSQLitePath(t *testing.T) {
	baseEnv(t)
	path := filepath.Join(t.TempDir(), "nested", "dir", "relay.db")
	t.Setenv("SQLITE_PATH", path)
	cfg, err := Load("test")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DBPath != path {
		t.Fatalf("DBPath = %q, want the SQLITE_PATH value %q", cfg.DBPath, path)
	}
	info, err := os.Stat(filepath.Dir(path))
	if err != nil || !info.IsDir() {
		t.Fatalf("SQLITE_PATH parent directory was not created: %v", err)
	}
}

func TestLoadAcceptsPostgresURL(t *testing.T) {
	baseEnv(t)
	t.Setenv("DATABASE_URL", "postgres://relay@127.0.0.1:5432/exp_demo?sslmode=disable")
	cfg, err := Load("test")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.UsingPostgres() {
		t.Fatal("DATABASE_URL should select PostgreSQL")
	}
	if cfg.DatabaseURL != "postgres://relay@127.0.0.1:5432/exp_demo?sslmode=disable" {
		t.Fatalf("DATABASE_URL was altered: %q", cfg.DatabaseURL)
	}
}

func TestLoadRejectsNonPostgresDatabaseURL(t *testing.T) {
	for _, raw := range []string{
		"mysql://relay@127.0.0.1/relay",
		"postgres:///no-host",
		"not a url",
	} {
		t.Run(raw, func(t *testing.T) {
			baseEnv(t)
			t.Setenv("DATABASE_URL", raw)
			if _, err := Load("test"); err == nil {
				t.Fatalf("Load accepted invalid DATABASE_URL %q", raw)
			}
		})
	}
}
