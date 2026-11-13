package store

import (
	"math"
	"path/filepath"
	"reflect"
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

func TestListLogsPageSummarizesAllMatchingLogs(t *testing.T) {
	s := testStore(t)
	now := time.Now().Unix()
	for i, cost := range []float64{0.5, 1.25, 2} {
		costStatus := "actual"
		if i == 2 {
			costStatus = "unverified"
		}
		if err := s.InsertLog(&Log{
			RequestID: "req", Model: "m", Status: "success", Cost: cost,
			CostStatus: costStatus, CreatedAt: now - int64(i)*3600,
		}); err != nil {
			t.Fatalf("InsertLog: %v", err)
		}
	}

	first, err := s.ListLogsPage(LogFilter{Page: 1, PageSize: 2})
	if err != nil {
		t.Fatalf("ListLogsPage: %v", err)
	}
	if len(first.Items) != 2 {
		t.Fatalf("expected 2 items on the first page, got %d", len(first.Items))
	}
	if first.Summary.Requests != 3 || math.Abs(first.Summary.Cost-3.75) > 1e-9 {
		t.Fatalf("unexpected summary: %+v", first.Summary)
	}
	if math.Abs(first.Summary.UnverifiedCost-2) > 1e-9 {
		t.Fatalf("unexpected unverified cost: %+v", first.Summary)
	}
	if len(first.Summary.ByModel) != 1 || first.Summary.ByModel[0] != (ModelCost{
		Model: "m", Requests: 3, Cost: 3.75, UnverifiedCost: 2,
	}) {
		t.Fatalf("unexpected per-model cost: %+v", first.Summary.ByModel)
	}

	second, err := s.ListLogsPage(LogFilter{Page: 2, PageSize: 2})
	if err != nil {
		t.Fatalf("ListLogsPage page 2: %v", err)
	}
	if len(second.Items) != 1 {
		t.Fatalf("expected 1 item on the second page, got %d", len(second.Items))
	}
	if !reflect.DeepEqual(second.Summary, first.Summary) {
		t.Fatalf("summary must cover every match, not one page: page1=%+v page2=%+v", first.Summary, second.Summary)
	}

	filtered, err := s.ListLogsPage(LogFilter{Page: 1, PageSize: 10, Start: now - 3600, End: now})
	if err != nil {
		t.Fatalf("ListLogsPage with range: %v", err)
	}
	if filtered.Summary.Requests != 2 || math.Abs(filtered.Summary.Cost-1.75) > 1e-9 || filtered.Summary.UnverifiedCost != 0 {
		t.Fatalf("start/end filter must narrow the summary: %+v", filtered.Summary)
	}
	if len(filtered.Summary.ByModel) != 1 || filtered.Summary.ByModel[0] != (ModelCost{
		Model: "m", Requests: 2, Cost: 1.75,
	}) {
		t.Fatalf("start/end filter must narrow the per-model cost: %+v", filtered.Summary.ByModel)
	}
}

func TestListLogsPageGroupsCostByModel(t *testing.T) {
	s := testStore(t)
	now := time.Now().Unix()
	seed := []Log{
		{Model: "m-small", Cost: 0.25, CostStatus: "actual", CreatedAt: now},
		{Model: "m-large", Cost: 2, CostStatus: "actual", CreatedAt: now - 1},
		{Model: "m-large", Cost: 0.5, CostStatus: "unverified", CreatedAt: now - 2},
		{Model: "m-free", Cost: 0, CostStatus: "free", CreatedAt: now - 3},
	}
	for i := range seed {
		seed[i].RequestID = "req"
		seed[i].Status = "success"
		if err := s.InsertLog(&seed[i]); err != nil {
			t.Fatalf("InsertLog: %v", err)
		}
	}

	page, err := s.ListLogsPage(LogFilter{Page: 1, PageSize: 2})
	if err != nil {
		t.Fatalf("ListLogsPage: %v", err)
	}
	want := []ModelCost{
		{Model: "m-large", Requests: 2, Cost: 2.5, UnverifiedCost: 0.5},
		{Model: "m-small", Requests: 1, Cost: 0.25},
		{Model: "m-free", Requests: 1, Cost: 0},
	}
	if !reflect.DeepEqual(page.Summary.ByModel, want) {
		t.Fatalf("models must be aggregated across all matches and ordered by cost: got=%+v want=%+v",
			page.Summary.ByModel, want)
	}
	if page.Summary.Requests != 4 || math.Abs(page.Summary.Cost-2.75) > 1e-9 {
		t.Fatalf("per-model rows must sum to the summary total: %+v", page.Summary)
	}
	var groupedCost float64
	for _, item := range page.Summary.ByModel {
		groupedCost += item.Cost
	}
	if math.Abs(groupedCost-page.Summary.Cost) > 1e-9 {
		t.Fatalf("per-model cost %v does not add up to total %v", groupedCost, page.Summary.Cost)
	}

	byModel, err := s.ListLogsPage(LogFilter{Page: 1, PageSize: 10, Model: "m-large"})
	if err != nil {
		t.Fatalf("ListLogsPage by model: %v", err)
	}
	if len(byModel.Summary.ByModel) != 1 || byModel.Summary.ByModel[0] != want[0] {
		t.Fatalf("model filter must narrow the breakdown: %+v", byModel.Summary.ByModel)
	}
}

func TestListLogsByRequestID(t *testing.T) {
	s := testStore(t)
	for _, requestID := range []string{"req-a", "req-b"} {
		if err := s.InsertLog(&Log{RequestID: requestID, Model: "m", Status: "success"}); err != nil {
			t.Fatalf("InsertLog: %v", err)
		}
	}
	logs, total, err := s.ListLogs(LogFilter{Page: 1, PageSize: 10, RequestID: "req-b"})
	if err != nil || total != 1 || len(logs) != 1 || logs[0].RequestID != "req-b" {
		t.Fatalf("ListLogs by request id: total=%d logs=%+v err=%v", total, logs, err)
	}
}
