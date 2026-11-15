package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"
)

// newTestPostgresSchema creates a throwaway schema in the disposable database
// named by TEST_DATABASE_URL and returns a URL whose search_path targets it.
// Every test gets its own schema, so tests can run against one database
// without truncating shared tables.
func newTestPostgresSchema(t *testing.T, base string) (string, func()) {
	t.Helper()
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	schema := fmt.Sprintf("mr_test_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		_ = admin.Close()
		t.Fatalf("create test schema: %v", err)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	cleanup := func() {
		if _, err := admin.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`); err != nil {
			t.Logf("drop test schema %s: %v", schema, err)
		}
		_ = admin.Close()
	}
	return u.String(), cleanup
}

func openTestPostgres(t *testing.T, base string) (*Store, func()) {
	t.Helper()
	testURL, dropSchema := newTestPostgresSchema(t, base)
	s, err := OpenPostgres(testURL)
	if err != nil {
		dropSchema()
		t.Fatalf("OpenPostgres: %v", err)
	}
	return s, func() {
		_ = s.Close()
		dropSchema()
	}
}

func postgresTestURL(t *testing.T) string {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	return base
}

// TestPostgresMigrationsAndRestart verifies that an empty PostgreSQL database
// is migrated, that reopening it is idempotent, and that records survive a
// gateway restart.
func TestPostgresMigrationsAndRestart(t *testing.T) {
	testURL, dropSchema := newTestPostgresSchema(t, postgresTestURL(t))
	defer dropSchema()

	first, err := OpenPostgres(testURL)
	if err != nil {
		t.Fatalf("open first: %v", err)
	}
	if first.Backend() != "postgres" {
		t.Fatalf("backend = %q", first.Backend())
	}
	if _, err := first.EnsureAdmin("admin", "secret-pass"); err != nil {
		t.Fatalf("EnsureAdmin: %v", err)
	}
	channel := &Channel{
		Name: "pg-primary", Type: "openai", BaseURL: "https://example.com/v1",
		APIKeyEncrypted: "enc", APIKeyHint: "••••1234",
		Models: []string{"gpt-test"}, ModelMapping: map[string]string{"alias": "gpt-test"},
		Priority: 3, Weight: 2, Status: "enabled", InputPrice: 1.25, OutputPrice: 2.5,
	}
	if err := first.CreateChannel(channel); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if channel.ID <= 0 {
		t.Fatalf("CreateChannel did not assign an id: %d", channel.ID)
	}
	token := createTestToken(t, first, "pg-key", 5)
	if err := first.InsertLog(&Log{
		RequestID: "restart-log", TokenID: token.ID, TokenName: token.Name,
		ChannelID: channel.ID, ChannelName: channel.Name, Model: "alias",
		Cost: 0.75, ReservedCost: 1, CostStatus: CostActual, Status: "success",
		StatusCode: 200, Stream: true,
	}); err != nil {
		t.Fatalf("InsertLog: %v", err)
	}
	speech := &SpeechClip{
		ChannelID: channel.ID, ChannelName: channel.Name, Model: "speech-test",
		VoiceID: "voice-1", Text: "hello", Speed: 1, Status: "running",
		TotalChunks: 3, DoneChunks: 1, UpstreamTaskID: "task-1", UpstreamFileID: 42,
		Format: "mp3",
	}
	if err := first.CreateSpeechClip(speech); err != nil {
		t.Fatalf("CreateSpeechClip: %v", err)
	}
	music := &MusicTask{
		ChannelID: channel.ID, ChannelName: channel.Name, Model: "music-3.0",
		Prompt: "warm synth", Lyrics: "la la", Instrumental: true, Format: "mp3",
		Status: "queued",
	}
	if err := first.CreateMusicTask(music); err != nil {
		t.Fatalf("CreateMusicTask: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first: %v", err)
	}

	second, err := OpenPostgres(testURL)
	if err != nil {
		t.Fatalf("open second: %v", err)
	}
	defer second.Close()

	user, err := second.GetUserByUsername("admin")
	if err != nil || user.Username != "admin" {
		t.Fatalf("admin did not persist: %+v err=%v", user, err)
	}
	gotChannel, err := second.GetChannel(channel.ID)
	if err != nil {
		t.Fatalf("GetChannel after restart: %v", err)
	}
	if gotChannel.Name != "pg-primary" || gotChannel.InputPrice != 1.25 || gotChannel.Models[0] != "gpt-test" {
		t.Fatalf("channel did not persist: %+v", gotChannel)
	}
	gotToken, err := second.GetTokenByID(token.ID)
	if err != nil {
		t.Fatalf("GetTokenByID after restart: %v", err)
	}
	if gotToken.Name != "pg-key" || gotToken.Quota != 5 {
		t.Fatalf("token did not persist: %+v", gotToken)
	}
	page, err := second.ListLogsPage(LogFilter{Page: 1, PageSize: 10, RequestID: "restart-log"})
	if err != nil {
		t.Fatalf("ListLogsPage: %v", err)
	}
	if len(page.Items) != 1 || !page.Items[0].Stream || page.Items[0].RequestID != "restart-log" {
		t.Fatalf("log did not persist: %+v", page.Items)
	}
	gotSpeech, err := second.GetSpeechClip(speech.ID)
	if err != nil {
		t.Fatalf("GetSpeechClip after restart: %v", err)
	}
	if gotSpeech.UpstreamTaskID != "task-1" || gotSpeech.UpstreamFileID != 42 || gotSpeech.DoneChunks != 1 {
		t.Fatalf("speech clip did not persist: %+v", gotSpeech)
	}
	gotMusic, err := second.GetMusicTask(music.ID)
	if err != nil {
		t.Fatalf("GetMusicTask after restart: %v", err)
	}
	if !gotMusic.Instrumental || gotMusic.Prompt != "warm synth" || gotMusic.Lyrics != "la la" {
		t.Fatalf("music task did not persist: %+v", gotMusic)
	}
}

// TestPostgresReservationsSerialize verifies that concurrent reservations for
// one key cannot overspend in PostgreSQL. The token row lock in ReserveQuota
// serializes them, matching SQLite's single-writer behavior.
func TestPostgresReservationsSerialize(t *testing.T) {
	postgresTestURL(t)
	s := testStore(t)
	limited := createTestToken(t, s, "pg-limited", 10)
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := s.ReserveQuota(fmt.Sprintf("pg-res-%d", i), limited.ID, "req", "model", 6, 0)
			errs <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)

	var accepted, rejected int
	for err := range errs {
		var insufficient *InsufficientQuotaError
		switch {
		case err == nil:
			accepted++
		case errors.As(err, &insufficient):
			rejected++
		default:
			t.Fatalf("unexpected reservation error: %v", err)
		}
	}
	if accepted != 1 || rejected != 1 {
		t.Fatalf("accepted=%d rejected=%d, want 1/1", accepted, rejected)
	}
}
