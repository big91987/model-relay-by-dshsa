package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/big91987/model-relay-by-dshsa/internal/auth"
)

func TestTokenLimitsAndDailySpend(t *testing.T) {
	s := testStore(t)
	_, prefix, hash, _ := auth.GenerateAPIKey()
	token := &Token{
		UserID: 1, Name: "limits", KeyHash: hash, KeyPrefix: prefix, Status: "active", Quota: 10,
		DailyQuota: 2.5, RPMLimit: 60, TPMLimit: 1000, MaxConcurrency: 3,
	}
	if err := s.CreateToken(token); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	got, err := s.GetTokenByID(token.ID)
	if err != nil {
		t.Fatalf("GetTokenByID: %v", err)
	}
	if got.DailyQuota != 2.5 || got.TPMLimit != 1000 || got.MaxConcurrency != 3 || got.RPMLimit != 60 {
		t.Fatalf("limits were not persisted: %+v", got)
	}

	now := time.Now().Unix()
	dayStart := startOfDay(now)
	for _, log := range []*Log{
		{RequestID: "today", TokenID: token.ID, Cost: 0.4, Status: "success", CreatedAt: dayStart + 60},
		{RequestID: "today-error", TokenID: token.ID, Cost: 9, Status: "error", CreatedAt: dayStart + 90},
		{RequestID: "yesterday", TokenID: token.ID, Cost: 5, Status: "success", CreatedAt: dayStart - 60},
		{RequestID: "other-key", TokenID: token.ID + 1, Cost: 3, Status: "success", CreatedAt: dayStart + 60},
	} {
		if err := s.InsertLog(log); err != nil {
			t.Fatalf("InsertLog: %v", err)
		}
	}
	spent, err := s.TokenSpendSince(token.ID, dayStart)
	if err != nil {
		t.Fatalf("TokenSpendSince: %v", err)
	}
	if spent != 0.4 {
		t.Fatalf("daily spend = %v, want 0.4", spent)
	}
	tokens, err := s.ListTokens()
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if len(tokens) != 1 || tokens[0].DailyUsedQuota != 0.4 {
		t.Fatalf("ListTokens daily spend = %+v", tokens)
	}
}

func TestMigrateAddsLimitColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open old database: %v", err)
	}
	// The pre-limits schema of a released database.
	if _, err := db.Exec(`CREATE TABLE tokens (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id INTEGER NOT NULL,
		name TEXT NOT NULL,
		key_hash TEXT NOT NULL UNIQUE,
		key_prefix TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'active',
		quota REAL NOT NULL DEFAULT -1,
		used_quota REAL NOT NULL DEFAULT 0,
		allowed_models TEXT NOT NULL DEFAULT '[]',
		rpm_limit INTEGER NOT NULL DEFAULT 0,
		expires_at INTEGER,
		last_used_at INTEGER,
		created_at INTEGER NOT NULL
	)`); err != nil {
		t.Fatalf("create old schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close old database: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open migrated database: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	_, prefix, hash, _ := auth.GenerateAPIKey()
	token := &Token{UserID: 1, Name: "old", KeyHash: hash, KeyPrefix: prefix, Status: "active", Quota: -1}
	if err := s.CreateToken(token); err != nil {
		t.Fatalf("CreateToken after migration: %v", err)
	}
	got, err := s.GetTokenByID(token.ID)
	if err != nil {
		t.Fatalf("GetTokenByID after migration: %v", err)
	}
	if got.TPMLimit != 0 || got.MaxConcurrency != 0 || got.DailyQuota != 0 {
		t.Fatalf("new columns should default to unlimited: %+v", got)
	}
}
