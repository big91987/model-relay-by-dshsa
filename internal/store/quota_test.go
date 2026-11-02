package store

import (
	"database/sql"
	"errors"
	"math"
	"path/filepath"
	"testing"

	"github.com/big91987/model-relay-by-dshsa/internal/auth"
)

func createTestToken(t *testing.T, s *Store, name string, quota float64) *Token {
	t.Helper()
	_, prefix, hash, err := auth.GenerateAPIKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	token := &Token{UserID: 1, Name: name, KeyHash: hash, KeyPrefix: prefix, Status: "active", Quota: quota}
	if err := s.CreateToken(token); err != nil {
		t.Fatalf("create token: %v", err)
	}
	return token
}

func testLog(token *Token, request, model string, cost float64) *Log {
	return &Log{
		RequestID:        request,
		TokenID:          token.ID,
		TokenName:        token.Name,
		Model:            model,
		PromptTokens:     10,
		CompletionTokens: 5,
		TotalTokens:      15,
		Cost:             cost,
		Status:           "success",
		StatusCode:       200,
	}
}

func nearTax(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestReserveQuotaIsAtomicPerKey(t *testing.T) {
	s := testStore(t)
	limited := createTestToken(t, s, "limited", 1)
	unlimited := createTestToken(t, s, "unlimited", -1)

	if _, err := s.ReserveQuota("a1", limited.ID, "a1", "model", 0.9, 0); err != nil {
		t.Fatalf("first reservation: %v", err)
	}
	if _, err := s.ReserveQuota("b1", unlimited.ID, "b1", "model", 10, 0); err != nil {
		t.Fatalf("unlimited reservation: %v", err)
	}
	_, err := s.ReserveQuota("a2", limited.ID, "a2", "model", 0.2, 0)
	var insufficient *InsufficientQuotaError
	if !errors.As(err, &insufficient) {
		t.Fatalf("expected insufficient quota, got %v", err)
	}
	if !nearTax(insufficient.Available, 0.1) || !nearTax(insufficient.Required, 0.2) {
		t.Fatalf("unexpected rejection details: %+v", insufficient)
	}

	got, err := s.GetTokenByID(limited.ID)
	if err != nil || !nearTax(got.ReservedQuota, 0.9) || got.UsedQuota != 0 {
		t.Fatalf("limited token: %+v err=%v", got, err)
	}
	other, err := s.GetTokenByID(unlimited.ID)
	if err != nil || !nearTax(other.ReservedQuota, 10) {
		t.Fatalf("unlimited token: %+v err=%v", other, err)
	}

	// Settle the first request at less than the hold; the remainder returns.
	if ok, err := s.FinalizeReservation("a1", testLog(limited, "a1", "model", 0.5), 0.5, CostActual, false); err != nil || !ok {
		t.Fatalf("settle a1: ok=%v err=%v", ok, err)
	}
	got, _ = s.GetTokenByID(limited.ID)
	if !nearTax(got.UsedQuota, 0.5) || got.ReservedQuota != 0 {
		t.Fatalf("after settlement: used=%v reserved=%v", got.UsedQuota, got.ReservedQuota)
	}
	if _, err := s.ReserveQuota("a2", limited.ID, "a2", "model", 0.2, 0); err != nil {
		t.Fatalf("reservation after release: %v", err)
	}
	if ok, err := s.FinalizeReservation("a2", testLog(limited, "a2", "model", 0), 0, CostFree, true); err != nil || !ok {
		t.Fatalf("release a2: ok=%v err=%v", ok, err)
	}
	got, _ = s.GetTokenByID(limited.ID)
	if !nearTax(got.UsedQuota, 0.5) || got.ReservedQuota != 0 {
		t.Fatalf("after release: used=%v reserved=%v", got.UsedQuota, got.ReservedQuota)
	}
}

func TestFinalizeReservationIsIdempotent(t *testing.T) {
	s := testStore(t)
	token := createTestToken(t, s, "key", 10)
	if _, err := s.ReserveQuota("r1", token.ID, "r1", "model", 1, 0); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	first := testLog(token, "r1", "model", 0.3)
	ok, err := s.FinalizeReservation("r1", first, 0.3, CostActual, false)
	if err != nil || !ok {
		t.Fatalf("first finalize: ok=%v err=%v", ok, err)
	}
	second := testLog(token, "r1", "model", 0.3)
	ok, err = s.FinalizeReservation("r1", second, 0.3, CostActual, false)
	if err != nil {
		t.Fatalf("second finalize: %v", err)
	}
	if ok {
		t.Fatal("duplicate completion notification was settled twice")
	}
	got, _ := s.GetTokenByID(token.ID)
	if !nearTax(got.UsedQuota, 0.3) {
		t.Fatalf("used quota = %v, want 0.3", got.UsedQuota)
	}
	logs, total, err := s.ListLogs(LogFilter{Page: 1, PageSize: 20})
	if err != nil || total != 1 || len(logs) != 1 {
		t.Fatalf("logs: total=%d len=%d err=%v", total, len(logs), err)
	}
}

func TestRecoverPendingReservationsAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quota.db")
	first, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	token := createTestToken(t, first, "key", 2)
	if _, err := first.ReserveQuota("inflight", token.ID, "inflight", "public-model", 0.75, 0); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()

	recovered, err := second.RecoverPendingReservations()
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(recovered) != 1 || recovered[0].ID != "inflight" || !nearTax(recovered[0].SettledCost, 0.75) {
		t.Fatalf("unexpected recovery: %+v", recovered)
	}
	got, err := second.GetTokenByID(token.ID)
	if err != nil {
		t.Fatalf("get token: %v", err)
	}
	if !nearTax(got.UsedQuota, 0.75) || got.ReservedQuota != 0 {
		t.Fatalf("after recovery: used=%v reserved=%v", got.UsedQuota, got.ReservedQuota)
	}
	logs, total, err := second.ListLogs(LogFilter{Page: 1, PageSize: 20})
	if err != nil || total != 1 || logs[0].CostStatus != CostUnverified || !nearTax(logs[0].Cost, 0.75) {
		t.Fatalf("recovery log: total=%d logs=%+v err=%v", total, logs, err)
	}
	again, err := second.RecoverPendingReservations()
	if err != nil || len(again) != 0 {
		t.Fatalf("second recovery: %+v err=%v", again, err)
	}
}

func TestRecoverOnlyPendingReservations(t *testing.T) {
	s := testStore(t)
	token := createTestToken(t, s, "key", 10)
	if _, err := s.ReserveQuota("done", token.ID, "done", "model", 1, 0); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if ok, err := s.FinalizeReservation("done", testLog(token, "done", "model", 0.4), 0.4, CostActual, false); err != nil || !ok {
		t.Fatalf("finalize: ok=%v err=%v", ok, err)
	}
	recovered, err := s.RecoverPendingReservations()
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(recovered) != 0 {
		t.Fatalf("settled reservation was recovered again: %+v", recovered)
	}
	got, _ := s.GetTokenByID(token.ID)
	if !nearTax(got.UsedQuota, 0.4) {
		t.Fatalf("used quota = %v, want 0.4", got.UsedQuota)
	}
}

func TestOpenMigratesExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	oldSchema := []string{
		`CREATE TABLE users (id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT NOT NULL UNIQUE, password_hash TEXT NOT NULL, role TEXT NOT NULL DEFAULT 'admin', status TEXT NOT NULL DEFAULT 'enabled', created_at INTEGER NOT NULL)`,
		`CREATE TABLE channels (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, type TEXT NOT NULL, base_url TEXT NOT NULL, api_key TEXT NOT NULL, api_key_hint TEXT NOT NULL DEFAULT '', models TEXT NOT NULL DEFAULT '[]', model_mapping TEXT NOT NULL DEFAULT '{}', priority INTEGER NOT NULL DEFAULT 0, weight INTEGER NOT NULL DEFAULT 1, status TEXT NOT NULL DEFAULT 'enabled', input_price REAL NOT NULL DEFAULT 0, output_price REAL NOT NULL DEFAULT 0, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
		`CREATE TABLE tokens (id INTEGER PRIMARY KEY AUTOINCREMENT, user_id INTEGER NOT NULL, name TEXT NOT NULL, key_hash TEXT NOT NULL UNIQUE, key_prefix TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'active', quota REAL NOT NULL DEFAULT -1, used_quota REAL NOT NULL DEFAULT 0, allowed_models TEXT NOT NULL DEFAULT '[]', rpm_limit INTEGER NOT NULL DEFAULT 0, expires_at INTEGER, last_used_at INTEGER, created_at INTEGER NOT NULL)`,
		`CREATE TABLE logs (id INTEGER PRIMARY KEY AUTOINCREMENT, request_id TEXT NOT NULL, user_id INTEGER NOT NULL DEFAULT 0, token_id INTEGER NOT NULL DEFAULT 0, channel_id INTEGER NOT NULL DEFAULT 0, token_name TEXT NOT NULL DEFAULT '', channel_name TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '', upstream_model TEXT NOT NULL DEFAULT '', prompt_tokens INTEGER NOT NULL DEFAULT 0, completion_tokens INTEGER NOT NULL DEFAULT 0, total_tokens INTEGER NOT NULL DEFAULT 0, cost REAL NOT NULL DEFAULT 0, status TEXT NOT NULL DEFAULT 'success', status_code INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '', latency_ms INTEGER NOT NULL DEFAULT 0, stream INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL)`,
	}
	for _, stmt := range oldSchema {
		if _, err := raw.Exec(stmt); err != nil {
			raw.Close()
			t.Fatalf("create old schema: %v", err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	defer s.Close()
	// A row written by the old version must remain readable with the new
	// defaults, and new writes must carry the reservation metadata.
	if err := s.InsertLog(&Log{RequestID: "old", Model: "m", Cost: 0.1, Status: "success", CreatedAt: 1}); err != nil {
		t.Fatalf("insert migrated log: %v", err)
	}
	logs, total, err := s.ListLogs(LogFilter{Page: 1, PageSize: 10})
	if err != nil || total != 1 || len(logs) != 1 || logs[0].CostStatus != CostActual || logs[0].ReservedCost != 0 {
		t.Fatalf("migrated logs: total=%d logs=%+v err=%v", total, logs, err)
	}
	token := createTestToken(t, s, "migrated", 5)
	got, err := s.GetTokenByID(token.ID)
	if err != nil || got.ReservedQuota != 0 {
		t.Fatalf("migrated token: %+v err=%v", got, err)
	}
}
