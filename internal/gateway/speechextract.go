package gateway

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/big91987/model-relay-by-dshsa/internal/store"
)

const (
	speechAudioProbeSize = 512
	// maxSpeechAsyncArchive bounds the provider response before extraction.
	// The real MiniMax file is a POSIX tar that contains the audio plus
	// subtitle and extra metadata; it is slightly larger than the audio alone.
	maxSpeechAsyncArchive = maxSpeechAsyncAudio + (128 << 20)
)

var speechAudioFormatByExtension = map[string]string{
	".mp3":  "mp3",
	".wav":  "wav",
	".flac": "flac",
	".m4a":  "m4a",
	".aac":  "m4a",
	".ogg":  "ogg",
	".opus": "opus",
}

// detectSpeechAudioFormat recognizes the container formats MiniMax can return
// for a speech task. It only inspects the container header, which is enough to
// reject the tar archive that the provider returns for asynchronous tasks.
func sniffSpeechAudioContainer(head []byte) string {
	switch {
	case len(head) >= 3 && string(head[:3]) == "ID3":
		return "mp3"
	case len(head) >= 2 && head[0] == 0xFF && head[1]&0xE0 == 0xE0:
		return "mp3"
	case len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "WAVE":
		return "wav"
	case len(head) >= 4 && string(head[:4]) == "fLaC":
		return "flac"
	case len(head) >= 4 && string(head[:4]) == "OggS":
		return "ogg"
	case len(head) >= 12 && string(head[4:8]) == "ftyp":
		return "m4a"
	}
	return ""
}

// isSpeechTarArchive detects the POSIX/ustar archive MiniMax returns for the
// asynchronous long-text endpoint.
func isSpeechTarArchive(head []byte) bool {
	return len(head) >= 262 && string(head[257:262]) == "ustar"
}

func readSpeechFileHead(name string, size int) ([]byte, error) {
	file, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	buf := make([]byte, size)
	n, err := io.ReadFull(file, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:n], nil
}

// materializeSpeechAudio turns a downloaded provider response into a standalone
// audio file. It accepts both the raw audio used by older tests/mocks and the
// POSIX tar archive returned by the real MiniMax asynchronous API.
func materializeSpeechAudio(sourcePath, dir string) (string, int64, string, error) {
	head, err := readSpeechFileHead(sourcePath, speechAudioProbeSize)
	if err != nil {
		return "", 0, "", err
	}
	if isSpeechTarArchive(head) {
		return extractSpeechAudioFromTar(sourcePath, dir)
	}
	format := sniffSpeechAudioContainer(head)
	if format == "" {
		return "", 0, "", errors.New("the provider returned neither a supported audio file nor a POSIX tar archive")
	}
	if info, err := os.Stat(sourcePath); err != nil {
		return "", 0, "", err
	} else if info.Size() > maxSpeechAsyncAudio {
		return "", 0, "", fmt.Errorf("the generated audio exceeds the %d MiB limit", maxSpeechAsyncAudio>>20)
	}
	return moveSpeechAudioFile(sourcePath, dir, format)
}

func moveSpeechAudioFile(sourcePath, dir, format string) (string, int64, string, error) {
	target, err := os.CreateTemp(dir, "speech-async-*."+format)
	if err != nil {
		return "", 0, "", err
	}
	targetPath := target.Name()
	if err := target.Close(); err != nil {
		_ = os.Remove(targetPath)
		return "", 0, "", err
	}
	_ = os.Remove(targetPath)
	if err := os.Rename(sourcePath, targetPath); err != nil {
		_ = os.Remove(targetPath)
		return "", 0, "", err
	}
	info, err := os.Stat(targetPath)
	if err != nil {
		_ = os.Remove(targetPath)
		return "", 0, "", err
	}
	if info.Size() == 0 {
		_ = os.Remove(targetPath)
		return "", 0, "", errors.New("the provider returned an empty audio file")
	}
	return targetPath, info.Size(), format, nil
}

// extractSpeechAudioFromTar streams the regular-file entries of a provider tar
// and extracts exactly one supported audio member. It never writes an entry
// using its archive name, and it skips metadata entries such as .titles and
// .extra.
func extractSpeechAudioFromTar(sourcePath, dir string) (string, int64, string, error) {
	file, err := os.Open(sourcePath)
	if err != nil {
		return "", 0, "", err
	}
	defer file.Close()

	reader := tar.NewReader(file)
	var outPath string
	var outSize int64
	var outFormat string
	names := make([]string, 0, 6)
	cleanup := func() {
		if outPath != "" {
			_ = os.Remove(outPath)
			outPath = ""
		}
	}
	for {
		header, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			cleanup()
			return "", 0, "", fmt.Errorf("read provider archive: %w", nextErr)
		}
		name := strings.TrimSpace(header.Name)
		if len(names) < 6 {
			names = append(names, name)
		}
		if err := validateSpeechTarEntryName(name); err != nil {
			cleanup()
			return "", 0, "", fmt.Errorf("provider archive entry %q: %w", name, err)
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			cleanup()
			return "", 0, "", fmt.Errorf("provider archive entry %q has unsupported type", name)
		}
		if header.Size < 0 {
			cleanup()
			return "", 0, "", fmt.Errorf("provider archive entry %q has an invalid size", name)
		}
		extensionFormat := speechAudioFormatByExtension[strings.ToLower(filepath.Ext(name))]
		probe := make([]byte, speechAudioProbeSize)
		n, readErr := io.ReadFull(reader, probe)
		if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) && !errors.Is(readErr, io.EOF) {
			cleanup()
			return "", 0, "", fmt.Errorf("read provider archive entry %q: %w", name, readErr)
		}
		probe = probe[:n]
		format := sniffSpeechAudioContainer(probe)
		if format == "" {
			if extensionFormat != "" {
				cleanup()
				return "", 0, "", fmt.Errorf("provider archive entry %q is not a supported audio file", name)
			}
			if _, err := io.Copy(io.Discard, reader); err != nil {
				cleanup()
				return "", 0, "", fmt.Errorf("read provider archive entry %q: %w", name, err)
			}
			continue
		}
		if header.Size > maxSpeechAsyncAudio {
			cleanup()
			return "", 0, "", fmt.Errorf("the extracted audio exceeds the %d MiB limit", maxSpeechAsyncAudio>>20)
		}
		if outPath != "" {
			cleanup()
			return "", 0, "", errors.New("the provider archive contains more than one audio file")
		}
		out, createErr := os.CreateTemp(dir, "speech-async-*."+format)
		if createErr != nil {
			return "", 0, "", createErr
		}
		outPath = out.Name()
		written := int64(0)
		if _, writeErr := out.Write(probe); writeErr != nil {
			_ = out.Close()
			cleanup()
			return "", 0, "", writeErr
		}
		written = int64(len(probe))
		copied, copyErr := io.Copy(out, io.LimitReader(reader, maxSpeechAsyncAudio-written+1))
		closeErr := out.Close()
		if copyErr != nil || closeErr != nil {
			cleanup()
			if copyErr != nil {
				return "", 0, "", copyErr
			}
			return "", 0, "", closeErr
		}
		outSize = written + copied
		if outSize > maxSpeechAsyncAudio {
			cleanup()
			return "", 0, "", fmt.Errorf("the extracted audio exceeds the %d MiB limit", maxSpeechAsyncAudio>>20)
		}
		if outSize == 0 {
			cleanup()
			return "", 0, "", errors.New("the provider archive contained an empty audio file")
		}
		outFormat = format
	}
	if outPath == "" {
		return "", 0, "", fmt.Errorf("the provider archive contains no supported audio file (entries: %s)", strings.Join(names, ", "))
	}
	return outPath, outSize, outFormat, nil
}

func validateSpeechTarEntryName(name string) error {
	if name == "" {
		return errors.New("empty entry name")
	}
	if strings.ContainsRune(name, 0) {
		return errors.New("invalid entry name")
	}
	normalized := strings.ReplaceAll(name, "\\", "/")
	if strings.HasPrefix(normalized, "/") {
		return errors.New("absolute entry path")
	}
	for _, part := range strings.Split(normalized, "/") {
		if part == ".." {
			return errors.New("entry path escapes the archive")
		}
	}
	cleaned := path.Clean(normalized)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return errors.New("entry path escapes the archive")
	}
	return nil
}

// prepareStoredSpeechAudio validates a stored file and, for the provider tar
// archive, extracts its audio member. It updates the clip fields in memory and
// returns the old relative path when the file was replaced; persistence is the
// caller's job so a failed database write cannot lose the previous file.
func (g *Gateway) prepareStoredSpeechAudio(clip *store.SpeechClip) (bool, string, error) {
	if clip == nil || clip.AudioPath == "" {
		return false, "", nil
	}
	source := filepath.Join(g.cfg.DataDir, clip.AudioPath)
	head, err := readSpeechFileHead(source, speechAudioProbeSize)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, "", nil
		}
		return false, "", err
	}
	if format := sniffSpeechAudioContainer(head); format != "" {
		if info, statErr := os.Stat(source); statErr == nil {
			clip.SizeBytes = info.Size()
		}
		if clip.Format == "" || !strings.EqualFold(clip.Format, format) {
			clip.Format = format
		}
		return true, "", nil
	}
	if !isSpeechTarArchive(head) {
		return false, "", nil
	}
	oldPath := clip.AudioPath
	dir := filepath.Dir(source)
	audioPath, size, format, err := extractSpeechAudioFromTar(source, dir)
	if err != nil {
		return false, "", err
	}
	rel, err := filepath.Rel(g.cfg.DataDir, audioPath)
	if err != nil {
		_ = os.Remove(audioPath)
		return false, "", err
	}
	clip.AudioPath = rel
	clip.SizeBytes = size
	clip.Format = format
	if clip.DurationMS == 0 && format == "mp3" {
		clip.DurationMS = size * 8 * 1000 / 128000
	}
	return true, oldPath, nil
}

// EnsureSpeechClipAudio makes a completed clip downloadable. Older releases
// stored the provider tar verbatim under a .mp3 path; this method extracts the
// real audio in place. Missing or invalid stored audio for an asynchronous task
// is re-downloaded from the provider once, without touching the old file until
// the replacement is safely stored.
func (g *Gateway) EnsureSpeechClipAudio(clip *store.SpeechClip) (*store.SpeechClip, error) {
	if clip == nil || clip.Status != "succeeded" {
		return clip, nil
	}
	lockValue, _ := g.speechRepairs.LoadOrStore(clip.ID, &sync.Mutex{})
	lock := lockValue.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()

	fresh, err := g.store.GetSpeechClip(clip.ID)
	if err == nil {
		clip = fresh
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if clip.Status != "succeeded" {
		return clip, nil
	}

	previousFormat, previousSize := clip.Format, clip.SizeBytes
	ok, oldPath, prepareErr := g.prepareStoredSpeechAudio(clip)
	if ok {
		if oldPath != "" || clip.Format != previousFormat || clip.SizeBytes != previousSize {
			if err := g.store.UpdateSpeechClip(clip); err != nil {
				return nil, err
			}
		}
		if oldPath != "" && oldPath != clip.AudioPath {
			_ = os.Remove(filepath.Join(g.cfg.DataDir, oldPath))
		}
		return clip, nil
	}
	if prepareErr != nil && !isRetryableSpeechAudioError(prepareErr) {
		return nil, prepareErr
	}

	if clip.UpstreamTaskID == "" || clip.UpstreamFileID == 0 {
		if prepareErr != nil {
			return nil, prepareErr
		}
		return nil, errors.New("the stored speech audio is missing or is not a supported audio file")
	}
	return g.redownloadSpeechClipAudio(clip)
}

// isRetryableSpeechAudioError reports whether re-downloading the provider file
// may fix the stored audio. Corrupt or unsafe provider archives are terminal.
func isRetryableSpeechAudioError(err error) bool {
	if err == nil {
		return true
	}
	message := err.Error()
	return !strings.Contains(message, "entry") && !strings.Contains(message, "archive")
}

func (g *Gateway) redownloadSpeechClipAudio(clip *store.SpeechClip) (*store.SpeechClip, error) {
	ch, err := g.store.GetChannel(clip.ChannelID)
	if err != nil {
		return nil, fmt.Errorf("the channel that created this speech task is unavailable: %w", err)
	}
	apiKey, err := g.secrets.Decrypt(ch.APIKeyEncrypted)
	if err != nil {
		return nil, fmt.Errorf("could not read the channel API key: %w", err)
	}
	oldPath := clip.AudioPath
	rel, size, format, err := g.downloadSpeechAsyncAudio(ch, apiKey, clip.UpstreamFileID)
	if err != nil {
		return nil, fmt.Errorf("could not re-download the speech audio: %w", err)
	}
	clip.AudioPath = rel
	clip.SizeBytes = size
	clip.Format = format
	if clip.DurationMS == 0 && format == "mp3" {
		clip.DurationMS = size * 8 * 1000 / 128000
	}
	if err := g.store.UpdateSpeechClip(clip); err != nil {
		_ = os.Remove(filepath.Join(g.cfg.DataDir, rel))
		return nil, err
	}
	if oldPath != "" && oldPath != rel {
		_ = os.Remove(filepath.Join(g.cfg.DataDir, oldPath))
	}
	return clip, nil
}
