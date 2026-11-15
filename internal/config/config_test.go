package config

import (
	"path/filepath"
	"testing"
)

func TestLoadDefaultsToSQLite(t *testing.T) {
	t.Setenv("DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("DATABASE_URL", "")
	t.Setenv("TEST_MODE", "")
	t.Setenv("SECRET_KEY", "")
	t.Setenv("ADMIN_USERNAME", "")
	t.Setenv("ADMIN_PASSWORD", "")
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
}

func TestLoadTestModeUsesAdminAdmin(t *testing.T) {
	t.Setenv("DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("TEST_MODE", "1")
	t.Setenv("ADMIN_USERNAME", "")
	t.Setenv("ADMIN_PASSWORD", "")
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
}

func TestLoadExplicitAdminPasswordBeatsTestDefaults(t *testing.T) {
	t.Setenv("DATA_DIR", filepath.Join(t.TempDir(), "data"))
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

func TestLoadAcceptsPostgresURL(t *testing.T) {
	t.Setenv("DATA_DIR", filepath.Join(t.TempDir(), "data"))
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
			t.Setenv("DATA_DIR", filepath.Join(t.TempDir(), "data"))
			t.Setenv("DATABASE_URL", raw)
			if _, err := Load("test"); err == nil {
				t.Fatalf("Load accepted invalid DATABASE_URL %q", raw)
			}
		})
	}
}
