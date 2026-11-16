package gateway

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/big91987/model-relay-by-dshsa/internal/store"
)

type tarTestEntry struct {
	name     string
	body     []byte
	typeflag byte
}

func buildProviderTar(t *testing.T, entries ...tarTestEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	for _, entry := range entries {
		typeflag := entry.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		header := &tar.Header{
			Name:     entry.name,
			Mode:     0o600,
			Size:     int64(len(entry.body)),
			Typeflag: typeflag,
		}
		if typeflag == tar.TypeDir {
			header.Mode = 0o700
			header.Size = 0
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatalf("write tar header %q: %v", entry.name, err)
		}
		if typeflag != tar.TypeDir {
			if _, err := writer.Write(entry.body); err != nil {
				t.Fatalf("write tar body %q: %v", entry.name, err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	return buf.Bytes()
}

func realSpeechMP3(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "scripts", "media", "demo.mp3"))
	if err != nil {
		t.Fatalf("read demo mp3 fixture: %v", err)
	}
	if !bytes.HasPrefix(data, []byte("ID3")) && !(len(data) >= 2 && data[0] == 0xFF && data[1]&0xE0 == 0xE0) {
		t.Fatalf("demo fixture is not recognized as MP3")
	}
	return data
}

func providerSpeechTar(t *testing.T) []byte {
	t.Helper()
	return buildProviderTar(t,
		tarTestEntry{name: "output", typeflag: tar.TypeDir},
		tarTestEntry{name: "output/speech.titles", body: []byte(`{"titles":[{"text":"hello","start":0}]}`)},
		tarTestEntry{name: "output/speech.extra", body: []byte(`{"audio_length":7440,"audio_format":"mp3"}`)},
		tarTestEntry{name: "output/speech.mp3", body: realSpeechMP3(t)},
	)
}

func TestSniffSpeechAudioContainer(t *testing.T) {
	mp3 := realSpeechMP3(t)
	if got := sniffSpeechAudioContainer(mp3); got != "mp3" {
		t.Fatalf("mp3 format = %q", got)
	}
	if got := sniffSpeechAudioContainer([]byte("RIFF\x00\x00\x00\x00WAVEfmt ")); got != "wav" {
		t.Fatalf("wav format = %q", got)
	}
	if got := sniffSpeechAudioContainer([]byte("fLaC\x00\x00\x00\x00")); got != "flac" {
		t.Fatalf("flac format = %q", got)
	}
	if got := sniffSpeechAudioContainer([]byte("not audio")); got != "" {
		t.Fatalf("unexpected format for junk: %q", got)
	}
}

func TestValidateSpeechTarEntryName(t *testing.T) {
	cases := map[string]bool{
		"output/speech.mp3": true,
		"speech.mp3":        true,
		"./speech.mp3":      true,
		"":                  false,
		"/etc/passwd":       false,
		"../speech.mp3":     false,
		"output/../../x":    false,
		"output\\..\\x":     false,
	}
	for name, wantOK := range cases {
		err := validateSpeechTarEntryName(name)
		if (err == nil) != wantOK {
			t.Fatalf("validateSpeechTarEntryName(%q) = %v, want ok=%v", name, err, wantOK)
		}
	}
}

func TestSpeechAsyncDownloadExtractsProviderTar(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockSpeechUpstream(t)
	env.addChannel(t, "minimax", upstream.server.URL, "minimax", 0, []string{"speech-2.8-hd"}, nil, 0, 0)
	archive := providerSpeechTar(t)
	upstream.async.audio = archive
	upstream.async.downloadContentType = "application/x-tar"

	text := strings.Repeat("这是一段真实归档格式的长文本配音测试。", 150)
	clip, err := env.gw.CreateSpeech(context.Background(), SpeechRequest{
		Model: "speech-2.8-hd", Text: text, VoiceID: "English_expressive_narrator",
	})
	if err != nil {
		t.Fatalf("create long speech: %v", err)
	}
	stored := waitSpeechClip(t, env, clip.ID)
	if stored.Status != "succeeded" {
		t.Fatalf("job failed: %s", stored.Error)
	}
	if stored.Format != "mp3" || stored.AudioPath == "" {
		t.Fatalf("unexpected stored clip: %+v", stored)
	}
	audio, err := os.ReadFile(filepath.Join(env.gw.cfg.DataDir, stored.AudioPath))
	if err != nil {
		t.Fatalf("read extracted audio: %v", err)
	}
	want := realSpeechMP3(t)
	if !bytes.Equal(audio, want) {
		t.Fatalf("extracted %d bytes, want the %d-byte MP3 member", len(audio), len(want))
	}
	if bytes.Contains(audio, []byte("ustar")) || bytes.HasSuffix(audio, []byte("speech.titles")) {
		t.Fatal("stored file still contains tar data")
	}
	if got := http.DetectContentType(audio); !strings.HasPrefix(got, "audio/") {
		t.Fatalf("strict content sniffing returned %q", got)
	}
	if stored.SizeBytes != int64(len(want)) {
		t.Fatalf("size = %d, want %d", stored.SizeBytes, len(want))
	}
}

func TestSpeechAsyncDownloadKeepsRawAudio(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockSpeechUpstream(t)
	env.addChannel(t, "minimax", upstream.server.URL, "minimax", 0, []string{"speech-2.8-hd"}, nil, 0, 0)
	raw := realSpeechMP3(t)
	upstream.async.audio = raw

	clip, err := env.gw.CreateSpeech(context.Background(), SpeechRequest{
		Model: "speech-2.8-hd", Text: strings.Repeat("裸音频仍然要兼容。", 300), VoiceID: "English_expressive_narrator",
	})
	if err != nil {
		t.Fatalf("create long speech: %v", err)
	}
	stored := waitSpeechClip(t, env, clip.ID)
	if stored.Status != "succeeded" {
		t.Fatalf("job failed: %s", stored.Error)
	}
	audio, err := os.ReadFile(filepath.Join(env.gw.cfg.DataDir, stored.AudioPath))
	if err != nil {
		t.Fatalf("read raw audio: %v", err)
	}
	if !bytes.Equal(audio, raw) || stored.Format != "mp3" || stored.SizeBytes != int64(len(raw)) {
		t.Fatalf("raw audio was not preserved: %+v", stored)
	}
}

func TestSpeechAsyncDownloadRejectsInvalidArchiveAndAudio(t *testing.T) {
	cases := []struct {
		name    string
		body    []byte
		ctype   string
		wantErr string
	}{
		{
			name:    "archive without audio",
			body:    buildProviderTar(t, tarTestEntry{name: "speech.titles", body: []byte("{}")}),
			ctype:   "application/x-tar",
			wantErr: "no supported audio file",
		},
		{
			name:    "invalid audio member",
			body:    buildProviderTar(t, tarTestEntry{name: "speech.mp3", body: []byte("this is not mp3")}),
			ctype:   "application/x-tar",
			wantErr: "not a supported audio file",
		},
		{
			name: "multiple audio members",
			body: buildProviderTar(t,
				tarTestEntry{name: "a.mp3", body: realSpeechMP3(t)},
				tarTestEntry{name: "b.mp3", body: realSpeechMP3(t)},
			),
			ctype:   "application/x-tar",
			wantErr: "more than one audio file",
		},
		{
			name:    "raw junk",
			body:    []byte("not an audio file"),
			ctype:   "application/octet-stream",
			wantErr: "neither a supported audio file",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			upstream := newMockSpeechUpstream(t)
			env.addChannel(t, "minimax", upstream.server.URL, "minimax", 0, []string{"speech-2.8-hd"}, nil, 0, 0)
			upstream.async.audio = tc.body
			upstream.async.downloadContentType = tc.ctype
			clip, err := env.gw.CreateSpeech(context.Background(), SpeechRequest{
				Model: "speech-2.8-hd", Text: strings.Repeat("无效音频测试。", 300), VoiceID: "English_expressive_narrator",
			})
			if err != nil {
				t.Fatalf("create long speech: %v", err)
			}
			stored := waitSpeechClip(t, env, clip.ID)
			if stored.Status != "failed" || !strings.Contains(stored.Error, tc.wantErr) {
				t.Fatalf("failed job = %+v, want error containing %q", stored, tc.wantErr)
			}
			if stored.AudioPath != "" {
				t.Fatalf("failed job kept an audio path: %q", stored.AudioPath)
			}
		})
	}
}

func TestSpeechAsyncDownloadRetriesAfterBadArchive(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockSpeechUpstream(t)
	env.addChannel(t, "minimax", upstream.server.URL, "minimax", 0, []string{"speech-2.8-hd"}, nil, 0, 0)
	bad := buildProviderTar(t, tarTestEntry{name: "speech.titles", body: []byte("{}")})
	good := providerSpeechTar(t)
	upstream.async.audio = good
	upstream.async.downloadContentType = "application/x-tar"
	upstream.async.downloadBodies = [][]byte{bad}

	clip, err := env.gw.CreateSpeech(context.Background(), SpeechRequest{
		Model: "speech-2.8-hd", Text: strings.Repeat("下载失败后重试归档。", 300), VoiceID: "English_expressive_narrator",
	})
	if err != nil {
		t.Fatalf("create long speech: %v", err)
	}
	stored := waitSpeechClip(t, env, clip.ID)
	if stored.Status != "succeeded" {
		t.Fatalf("retry job failed: %s", stored.Error)
	}
	upstream.async.mu.Lock()
	downloads := upstream.async.downloads
	upstream.async.mu.Unlock()
	if downloads != 2 {
		t.Fatalf("downloads = %d, want 2", downloads)
	}
	audio, err := os.ReadFile(filepath.Join(env.gw.cfg.DataDir, stored.AudioPath))
	if err != nil {
		t.Fatalf("read retried audio: %v", err)
	}
	if !bytes.Equal(audio, realSpeechMP3(t)) {
		t.Fatal("retry did not store the extracted MP3")
	}
}

func TestEnsureSpeechClipAudioRepairsExistingTar(t *testing.T) {
	env := newTestEnv(t)
	dir := filepath.Join(env.gw.cfg.DataDir, "speech")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("create speech dir: %v", err)
	}
	legacyPath := filepath.Join(dir, "legacy-async.mp3")
	if err := os.WriteFile(legacyPath, providerSpeechTar(t), 0o600); err != nil {
		t.Fatalf("write legacy tar: %v", err)
	}
	clip := &store.SpeechClip{
		ChannelName: "minimax", Model: "speech-2.8-hd", UpstreamModel: "speech-2.8-hd",
		VoiceID: "English_expressive_narrator", Text: "legacy", Status: "succeeded",
		Format: "mp3", AudioPath: "speech/legacy-async.mp3", UpstreamTaskID: "task-1",
		UpstreamFileID: 123,
	}
	if err := env.st.CreateSpeechClip(clip); err != nil {
		t.Fatalf("store legacy clip: %v", err)
	}

	updated, err := env.gw.EnsureSpeechClipAudio(clip)
	if err != nil {
		t.Fatalf("repair legacy clip: %v", err)
	}
	if updated.AudioPath == "speech/legacy-async.mp3" || updated.Format != "mp3" {
		t.Fatalf("legacy clip was not repaired: %+v", updated)
	}
	audio, err := os.ReadFile(filepath.Join(env.gw.cfg.DataDir, updated.AudioPath))
	if err != nil {
		t.Fatalf("read repaired audio: %v", err)
	}
	if !bytes.Equal(audio, realSpeechMP3(t)) {
		t.Fatalf("repaired audio is %d bytes, want the MP3 member", len(audio))
	}
	if _, err := os.Stat(legacyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy tar was not removed after repair: %v", err)
	}
	stored, err := env.st.GetSpeechClip(clip.ID)
	if err != nil {
		t.Fatalf("reload repaired clip: %v", err)
	}
	if stored.AudioPath != updated.AudioPath || stored.Format != "mp3" || stored.SizeBytes != int64(len(audio)) {
		t.Fatalf("repair was not persisted: %+v", stored)
	}
}

func TestEnsureSpeechClipAudioRetriesMissingProviderFile(t *testing.T) {
	env := newTestEnv(t)
	upstream := newMockSpeechUpstream(t)
	channel := env.addChannel(t, "minimax", upstream.server.URL, "minimax", 0, []string{"speech-2.8-hd"}, nil, 0, 0)
	upstream.async.audio = realSpeechMP3(t)
	clip := &store.SpeechClip{
		ChannelID: channel.ID, ChannelName: channel.Name, Model: "speech-2.8-hd",
		VoiceID: "English_expressive_narrator", Text: "missing", Status: "succeeded",
		Format: "mp3", AudioPath: "speech/missing.mp3", UpstreamTaskID: "task-2", UpstreamFileID: 920001,
		Characters: int64(utf8.RuneCountInString("missing")),
	}
	if err := env.st.CreateSpeechClip(clip); err != nil {
		t.Fatalf("store clip: %v", err)
	}
	updated, err := env.gw.EnsureSpeechClipAudio(clip)
	if err != nil {
		t.Fatalf("retry missing clip: %v", err)
	}
	audio, err := os.ReadFile(filepath.Join(env.gw.cfg.DataDir, updated.AudioPath))
	if err != nil {
		t.Fatalf("read retried audio: %v", err)
	}
	if !bytes.Equal(audio, realSpeechMP3(t)) {
		t.Fatal("retry did not store the provider audio")
	}
	upstream.async.mu.Lock()
	downloads := upstream.async.downloads
	upstream.async.mu.Unlock()
	if downloads != 1 {
		t.Fatalf("downloads = %d, want 1", downloads)
	}
}
