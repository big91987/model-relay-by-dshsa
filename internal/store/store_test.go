package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/big91987/model-relay-by-dshsa/internal/auth"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestEnsureAdmin(t *testing.T) {
	s := testStore(t)
	created, err := s.EnsureAdmin("admin", "admin-pass")
	if err != nil || !created {
		t.Fatalf("EnsureAdmin: created=%v err=%v", created, err)
	}
	created, err = s.EnsureAdmin("admin", "other-pass")
	if err != nil || created {
		t.Fatalf("second EnsureAdmin should be a no-op: created=%v err=%v", created, err)
	}
	user, err := s.GetUserByUsername("admin")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	if !auth.VerifyPassword(user.PasswordHash, "admin-pass") {
		t.Fatal("stored password hash does not verify")
	}
}

func TestChannelsAndTokens(t *testing.T) {
	s := testStore(t)
	channel := &Channel{
		Name: "primary", Type: "openai", BaseURL: "https://example.com/v1",
		APIKeyEncrypted: "enc", APIKeyHint: "••••abcd",
		Models: []string{"gpt-test"}, ModelMapping: map[string]string{"public": "upstream"},
		Priority: 5, Weight: 2, Status: "enabled", InputPrice: 1.5, OutputPrice: 3,
	}
	if err := s.CreateChannel(channel); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	got, err := s.GetChannel(channel.ID)
	if err != nil {
		t.Fatalf("GetChannel: %v", err)
	}
	if len(got.Models) != 1 || got.ModelMapping["public"] != "upstream" || got.Priority != 5 {
		t.Fatalf("unexpected channel: %+v", got)
	}
	got.Status = "disabled"
	if err := s.UpdateChannel(got); err != nil {
		t.Fatalf("UpdateChannel: %v", err)
	}
	channels, err := s.ListChannels()
	if err != nil || len(channels) != 1 || channels[0].Status != "disabled" {
		t.Fatalf("ListChannels: %v %+v", err, channels)
	}

	plain, prefix, hash, _ := auth.GenerateAPIKey()
	token := &Token{UserID: 1, Name: "ci", KeyHash: hash, KeyPrefix: prefix, Status: "active", Quota: 5, AllowedModels: []string{"public"}, RPMLimit: 60}
	if err := s.CreateToken(token); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	found, err := s.GetTokenByHash(auth.HashAPIKey(plain))
	if err != nil || found.ID != token.ID {
		t.Fatalf("GetTokenByHash: %v", err)
	}
	if err := s.AddTokenUsage(token.ID, 1.25); err != nil {
		t.Fatalf("AddTokenUsage: %v", err)
	}
	tokens, err := s.ListTokens()
	if err != nil || len(tokens) != 1 || tokens[0].UsedQuota != 1.25 || tokens[0].LastUsedAt == nil {
		t.Fatalf("ListTokens: %v %+v", err, tokens)
	}
	if err := s.DeleteToken(token.ID); err != nil {
		t.Fatalf("DeleteToken: %v", err)
	}
	if _, err := s.GetTokenByID(token.ID); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestLogsAndDashboard(t *testing.T) {
	s := testStore(t)
	now := time.Now().Unix()
	for i, status := range []string{"success", "success", "error"} {
		log := &Log{
			RequestID: "req", TokenID: 1, TokenName: "t", ChannelID: 1, ChannelName: "c",
			Model: "m", PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
			Cost: 0.5, Status: status, StatusCode: 200, CreatedAt: now - int64(i)*10,
		}
		if err := s.InsertLog(log); err != nil {
			t.Fatalf("InsertLog: %v", err)
		}
	}
	logs, total, err := s.ListLogs(LogFilter{Page: 1, PageSize: 2, Status: "success"})
	if err != nil || total != 2 || len(logs) != 2 {
		t.Fatalf("ListLogs: total=%d len=%d err=%v", total, len(logs), err)
	}
	dashboard, err := s.GetDashboard()
	if err != nil {
		t.Fatalf("GetDashboard: %v", err)
	}
	if dashboard.Totals.Requests != 3 || dashboard.Totals.Errors != 1 {
		t.Fatalf("unexpected totals: %+v", dashboard.Totals)
	}
	if len(dashboard.Series) != 7 {
		t.Fatalf("expected 7 day series, got %d", len(dashboard.Series))
	}
	if len(dashboard.TopModels) != 1 || dashboard.TopModels[0].Requests != 2 {
		t.Fatalf("unexpected top models: %+v", dashboard.TopModels)
	}
}
