package store

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/big91987/model-relay-by-dshsa/internal/auth"
)

// testStore returns an isolated SQLite database, or an isolated PostgreSQL
// schema when TEST_DATABASE_URL points at a disposable database. The same
// CRUD and accounting tests then exercise both backends.
func testStore(t *testing.T) *Store {
	t.Helper()
	if base := os.Getenv("TEST_DATABASE_URL"); base != "" {
		s, cleanup := openTestPostgres(t, base)
		t.Cleanup(cleanup)
		return s
	}
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

func TestEnsureTestAdminAllowsShortTestPassword(t *testing.T) {
	s := testStore(t)
	if _, err := s.EnsureAdmin("admin", "admin"); err == nil {
		t.Fatal("production EnsureAdmin must reject a password shorter than 6 characters")
	}
	created, err := s.EnsureTestAdmin("admin", "admin")
	if err != nil || !created {
		t.Fatalf("EnsureTestAdmin: created=%v err=%v", created, err)
	}
	user, err := s.GetUserByUsername("admin")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	if !auth.VerifyPassword(user.PasswordHash, "admin") {
		t.Fatal("admin/admin hash does not verify")
	}
}

func TestSQLitePathCreatesFileAndReopens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom", "state", "relay.db")
	first, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	channel := &Channel{Name: "custom path", Type: "openai", BaseURL: "https://example.com/v1", Status: "enabled"}
	if err := first.CreateChannel(channel); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		t.Fatalf("SQLITE_PATH file was not created at %q: %v", path, err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopen %q: %v", path, err)
	}
	defer second.Close()
	got, err := second.GetChannel(channel.ID)
	if err != nil {
		t.Fatalf("GetChannel after restart: %v", err)
	}
	if got.Name != "custom path" {
		t.Fatalf("channel did not persist across restart: %+v", got)
	}
}

func TestVoiceCloneAuthorizedRoundTrip(t *testing.T) {
	s := testStore(t)
	authorized := &VoiceClone{
		VoiceID: "voice-authorized", Name: "authorized clone", Model: "speech-2.8-hd",
		SampleFormat: "wav", SampleBytes: 1024, SourceFilename: "sample.wav",
		PreviewText: "hello", PreviewPath: "voice-clones/preview.mp3", PreviewFormat: "mp3",
		Authorized: true,
	}
	if err := s.CreateVoiceClone(authorized); err != nil {
		t.Fatalf("CreateVoiceClone(authorized): %v", err)
	}
	got, err := s.GetVoiceClone(authorized.ID)
	if err != nil {
		t.Fatalf("GetVoiceClone: %v", err)
	}
	if !got.Authorized {
		t.Fatalf("authorized flag did not survive the write/read round trip: %+v", got)
	}
	byVoiceID, err := s.GetVoiceCloneByVoiceID("voice-authorized")
	if err != nil || !byVoiceID.Authorized {
		t.Fatalf("GetVoiceCloneByVoiceID authorized=%v err=%v", byVoiceID != nil && byVoiceID.Authorized, err)
	}

	unauthorized := &VoiceClone{VoiceID: "voice-unauthorized", Name: "old row", Model: "speech-2.8-hd", SampleFormat: "wav", Authorized: false}
	if err := s.CreateVoiceClone(unauthorized); err != nil {
		t.Fatalf("CreateVoiceClone(unauthorized): %v", err)
	}
	got, err = s.GetVoiceClone(unauthorized.ID)
	if err != nil {
		t.Fatalf("GetVoiceClone(unauthorized): %v", err)
	}
	if got.Authorized {
		t.Fatalf("unauthorized row read back as true: %+v", got)
	}
	clones, err := s.ListVoiceClones()
	if err != nil {
		t.Fatalf("ListVoiceClones: %v", err)
	}
	var seen int
	for _, clone := range clones {
		if clone.VoiceID == "voice-authorized" {
			seen++
			if !clone.Authorized {
				t.Fatalf("ListVoiceClones lost the authorized flag: %+v", clone)
			}
		}
	}
	if seen != 1 {
		t.Fatalf("authorized clone not listed once: %d", seen)
	}
}

func TestMusicTaskAndAsyncSpeechColumns(t *testing.T) {
	s := testStore(t)
	speech := &SpeechClip{
		Model: "speech-test", VoiceID: "voice-1", Text: "hello", Speed: 1,
		Status: "running", UpstreamTaskID: "task-1", UpstreamFileID: 42, Format: "mp3",
	}
	if err := s.CreateSpeechClip(speech); err != nil {
		t.Fatalf("CreateSpeechClip: %v", err)
	}
	gotSpeech, err := s.GetSpeechClip(speech.ID)
	if err != nil {
		t.Fatalf("GetSpeechClip: %v", err)
	}
	if gotSpeech.UpstreamTaskID != "task-1" || gotSpeech.UpstreamFileID != 42 {
		t.Fatalf("async speech fields: %+v", gotSpeech)
	}
	music := &MusicTask{
		Model: "music-3.0", Prompt: "warm synth", Lyrics: "la la",
		Instrumental: true, Format: "mp3", Status: "queued",
	}
	if err := s.CreateMusicTask(music); err != nil {
		t.Fatalf("CreateMusicTask: %v", err)
	}
	gotMusic, err := s.GetMusicTask(music.ID)
	if err != nil {
		t.Fatalf("GetMusicTask: %v", err)
	}
	if !gotMusic.Instrumental || gotMusic.Prompt != "warm synth" || gotMusic.Lyrics != "la la" {
		t.Fatalf("music task: %+v", gotMusic)
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

func TestSpeechClipBackgroundJobMigration(t *testing.T) {
	s := testStore(t)
	if s.dialect != dialectSQLite {
		t.Skip("SQLite schema-migration test")
	}
	if _, err := s.db.Exec(`DROP TABLE speech_clips`); err != nil {
		t.Fatalf("drop speech_clips: %v", err)
	}
	if _, err := s.db.Exec(`CREATE TABLE speech_clips (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		channel_id INTEGER NOT NULL DEFAULT 0,
		channel_name TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '',
		upstream_model TEXT NOT NULL DEFAULT '',
		voice_id TEXT NOT NULL DEFAULT '',
		voice_name TEXT NOT NULL DEFAULT '',
		text TEXT NOT NULL DEFAULT '',
		format TEXT NOT NULL DEFAULT 'mp3',
		audio_path TEXT NOT NULL DEFAULT '',
		duration_ms INTEGER NOT NULL DEFAULT 0,
		characters INTEGER NOT NULL DEFAULT 0,
		size_bytes INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL
	)`); err != nil {
		t.Fatalf("create legacy speech_clips: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO speech_clips (channel_name, model, voice_id, text, format, audio_path, duration_ms, characters, size_bytes, created_at)
		VALUES ('old', 'speech-2.8-hd', 'voice', 'legacy clip', 'mp3', 'speech/old.mp3', 1000, 11, 42, 1)`); err != nil {
		t.Fatalf("insert legacy clip: %v", err)
	}

	if err := s.migrate(); err != nil {
		t.Fatalf("migrate legacy speech_clips: %v", err)
	}
	legacy, err := s.GetSpeechClip(1)
	if err != nil {
		t.Fatalf("get legacy clip: %v", err)
	}
	if legacy.Status != "succeeded" || legacy.Speed != 1 || legacy.TotalChunks != 0 || legacy.DoneChunks != 0 ||
		legacy.UpstreamTaskID != "" || legacy.UpstreamFileID != 0 {
		t.Fatalf("legacy clip was not upgraded: %+v", legacy)
	}

	job := &SpeechClip{
		ChannelID: 2, ChannelName: "minimax", Model: "speech-2.8-hd", VoiceID: "voice",
		Text: "long text", Speed: 1.25, Status: "queued", TotalChunks: 3,
		UpstreamTaskID: "910001", UpstreamFileID: 920001,
	}
	if err := s.CreateSpeechClip(job); err != nil {
		t.Fatalf("create job: %v", err)
	}
	job.Status = "running"
	job.DoneChunks = 1
	job.DurationMS = 500
	job.SizeBytes = 64
	if err := s.UpdateSpeechClip(job); err != nil {
		t.Fatalf("update job: %v", err)
	}
	stored, err := s.GetSpeechClip(job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if stored.Status != "running" || stored.DoneChunks != 1 || stored.TotalChunks != 3 || stored.Speed != 1.25 ||
		stored.UpstreamTaskID != "910001" || stored.UpstreamFileID != 920001 {
		t.Fatalf("unexpected stored job: %+v", stored)
	}
	clips, err := s.ListResumableSpeechClips()
	if err != nil {
		t.Fatalf("list resumable: %v", err)
	}
	if len(clips) != 1 || clips[0].ID != job.ID {
		t.Fatalf("resumable clips = %+v", clips)
	}

	job.Status = "succeeded"
	job.DoneChunks = 3
	job.AudioPath = "speech/done.mp3"
	if err := s.UpdateSpeechClip(job); err != nil {
		t.Fatalf("finish job: %v", err)
	}
	clips, err = s.ListResumableSpeechClips()
	if err != nil {
		t.Fatalf("list resumable after finish: %v", err)
	}
	if len(clips) != 0 {
		t.Fatalf("finished job was still resumable: %+v", clips)
	}
}

func TestExportLogs(t *testing.T) {
	s := testStore(t)
	base := time.Now().Unix()
	entries := []struct {
		requestID string
		status    string
		createdAt int64
	}{
		{"old-success", "success", base - 30},
		{"new-success", "success", base - 10},
		{"mid-error", "error", base - 20},
		{"outside-range", "success", base - 100},
	}
	for _, entry := range entries {
		log := &Log{RequestID: entry.requestID, Model: "m", Status: entry.status, CreatedAt: entry.createdAt}
		if err := s.InsertLog(log); err != nil {
			t.Fatalf("InsertLog: %v", err)
		}
	}

	var got []string
	err := s.ExportLogs(LogFilter{Status: "success", Start: base - 40, End: base}, func(l *Log) error {
		got = append(got, l.RequestID)
		return nil
	})
	if err != nil {
		t.Fatalf("ExportLogs: %v", err)
	}
	if len(got) != 2 || got[0] != "old-success" || got[1] != "new-success" {
		t.Fatalf("unexpected export order: %v", got)
	}

	stop := errors.New("stop")
	if err := s.ExportLogs(LogFilter{}, func(*Log) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("callback error not returned: %v", err)
	}
}

func TestExportLogsBatchesEveryRow(t *testing.T) {
	s := testStore(t)
	const count = 501
	for i := 0; i < count; i++ {
		if err := s.InsertLog(&Log{RequestID: "req", Model: "m", Status: "success"}); err != nil {
			t.Fatalf("InsertLog: %v", err)
		}
	}
	seen := 0
	var lastID int64
	if err := s.ExportLogs(LogFilter{}, func(l *Log) error {
		if l.ID <= lastID {
			t.Fatalf("export is not ordered by id: %d after %d", l.ID, lastID)
		}
		lastID = l.ID
		seen++
		return nil
	}); err != nil {
		t.Fatalf("ExportLogs: %v", err)
	}
	if seen != count {
		t.Fatalf("exported %d rows, want %d", seen, count)
	}
}

func TestExportLogsIgnoresRowsWrittenDuringExport(t *testing.T) {
	s := testStore(t)
	for i := 0; i < 500; i++ {
		if err := s.InsertLog(&Log{RequestID: "initial", Model: "m", Status: "success"}); err != nil {
			t.Fatalf("InsertLog: %v", err)
		}
	}
	seen := 0
	err := s.ExportLogs(LogFilter{}, func(*Log) error {
		seen++
		if seen == 1 {
			return s.InsertLog(&Log{RequestID: "late-arrival", Model: "m", Status: "success"})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ExportLogs: %v", err)
	}
	if seen != 500 {
		t.Fatalf("export included rows written after it started: exported %d, want 500", seen)
	}
	_, total, err := s.ListLogs(LogFilter{Page: 1, PageSize: 10, RequestID: "late-arrival"})
	if err != nil || total != 1 {
		t.Fatalf("late row should still be stored: total=%d err=%v", total, err)
	}
}
