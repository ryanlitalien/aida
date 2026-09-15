package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ElevenLabs synthesizes speech via the ElevenLabs HTTP API. It writes the
// returned MP3 to a temp file and returns the path. Caller deletes when done.
// afplay handles MP3 natively on macOS, so the existing Play() works.
type ElevenLabs struct {
	APIKey  string
	VoiceID string
	ModelID string
	TempDir string

	// Voice settings - all 0-1 except Speed (0.7-1.2). Zero means
	// "use ElevenLabs default" for stability/similarity/style; Speed of
	// 0 means "omit and use default 1.0".
	Stability  float64
	Similarity float64
	Style      float64
	Speed      float64

	// Volume is the playback gain applied on top of the package default;
	// 0 means use the package default (see afplayVolumeFor).
	Volume float64

	HTTP *http.Client
}

func (e *ElevenLabs) Name() string { return "elevenlabs" }

func (e *ElevenLabs) Synthesize(ctx context.Context, text string) (string, error) {
	if e.APIKey == "" {
		return "", fmt.Errorf("elevenlabs: no API key")
	}
	if e.VoiceID == "" {
		return "", fmt.Errorf("elevenlabs: no voice id")
	}
	body, _ := e.requestBody(text)

	url := fmt.Sprintf("https://api.elevenlabs.io/v1/text-to-speech/%s", e.VoiceID)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("xi-api-key", e.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "audio/mpeg")

	client := e.HTTP
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("elevenlabs: http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("elevenlabs: status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	dir := e.TempDir
	if dir == "" {
		dir = os.TempDir()
	}
	out := filepath.Join(dir, fmt.Sprintf("jarvis-%d.mp3", time.Now().UnixNano()))
	f, err := os.Create(out)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(out)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(out)
		return "", err
	}
	return out, nil
}

// requestBody builds the JSON payload shared by Synthesize and SpeakStream.
func (e *ElevenLabs) requestBody(text string) ([]byte, error) {
	modelID := e.ModelID
	if modelID == "" {
		modelID = "eleven_turbo_v2_5"
	}
	stability := e.Stability
	if stability == 0 {
		stability = 0.5
	}
	similarity := e.Similarity
	if similarity == 0 {
		similarity = 0.75
	}
	voiceSettings := map[string]any{
		"stability":        stability,
		"similarity_boost": similarity,
		"style":            e.Style,
	}
	if e.Speed > 0 {
		voiceSettings["speed"] = e.Speed
	}
	return json.Marshal(map[string]any{
		"text":           text,
		"model_id":       modelID,
		"voice_settings": voiceSettings,
	})
}

// SpeakStream hits ElevenLabs's /stream endpoint and pipes the MP3 straight
// into ffplay as bytes arrive, so playback starts at the first chunk instead
// of after the whole clip is downloaded. The same loudnorm + volume as the
// file path (Play) are applied via ffplay's filter chain so streamed and
// pre-synthesized audio sound identical. Falls back to Synthesize + Play if
// ffplay isn't installed. Blocks until playback completes; ctx cancellation
// (barge-in) is treated as success.
func (e *ElevenLabs) SpeakStream(ctx context.Context, text string) (StreamStats, error) {
	var stats StreamStats
	if e.APIKey == "" {
		return stats, fmt.Errorf("elevenlabs: no API key")
	}
	if e.VoiceID == "" {
		return stats, fmt.Errorf("elevenlabs: no voice id")
	}

	ffplay, err := exec.LookPath("ffplay")
	if err != nil {
		// No streaming player - fall back to the file path.
		start := time.Now()
		wav, serr := e.Synthesize(ctx, text)
		if serr != nil {
			return stats, serr
		}
		defer os.Remove(wav)
		stats.TTFA = time.Since(start)
		if perr := PlayAt(ctx, wav, e.Volume); perr != nil {
			return stats, perr
		}
		stats.Total = time.Since(start)
		return stats, nil
	}

	body, err := e.requestBody(text)
	if err != nil {
		return stats, err
	}
	url := fmt.Sprintf("https://api.elevenlabs.io/v1/text-to-speech/%s/stream", e.VoiceID)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return stats, err
	}
	req.Header.Set("xi-api-key", e.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "audio/mpeg")

	client := e.HTTP
	if client == nil {
		client = &http.Client{Timeout: 120 * time.Second}
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return stats, fmt.Errorf("elevenlabs: http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return stats, fmt.Errorf("elevenlabs: status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	args := []string{"-hide_banner", "-loglevel", "error", "-nodisp", "-autoexit"}
	if af := streamFilter(e.Volume); af != "" {
		args = append(args, "-af", af)
	}
	args = append(args, "-i", "pipe:0")
	cmd := exec.CommandContext(ctx, ffplay, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return stats, err
	}
	if err := cmd.Start(); err != nil {
		return stats, err
	}

	// Pump the streaming body into ffplay, recording time-to-first-byte.
	firstByte := true
	buf := make([]byte, 16*1024)
	var pumpErr error
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if firstByte {
				stats.TTFA = time.Since(start)
				firstByte = false
			}
			if _, werr := stdin.Write(buf[:n]); werr != nil {
				pumpErr = werr
				break
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				pumpErr = rerr
			}
			break
		}
	}
	stdin.Close()
	waitErr := cmd.Wait()
	stats.Total = time.Since(start)

	if ctx.Err() != nil {
		return stats, nil // barge-in / cancellation is a normal flow
	}
	if pumpErr != nil {
		return stats, fmt.Errorf("elevenlabs: stream: %w", pumpErr)
	}
	if waitErr != nil {
		return stats, fmt.Errorf("ffplay: %w", waitErr)
	}
	return stats, nil
}

// streamFilter builds ffplay's -af chain for streamed playback. It applies a
// volume gain only, deliberately NOT loudnorm.
//
// loudnorm (and dynaudnorm) buffer ~3s of lookahead. ffplay's -autoexit quits
// when the *input* stream ends without draining that buffer, so it lops the
// last ~3s off every reply (measured: a 14.6s clip played only 11.8s, words
// got cut mid-sentence). The file path (Play -> normalizeLoudness) still
// loudnorm-normalizes the pre-synthesized ack/wake clips, where it's safe
// because afplay plays a complete on-disk file rather than a live stream.
func streamFilter(v float64) string {
	af := afplayVolumeFor(v)
	if af == "" || af == "1" || af == "1.0" {
		return ""
	}
	return "volume=" + af
}

// ResolveElevenLabsKey returns the ElevenLabs API key from
// $ELEVENLABS_API_KEY, falling back to `op read <opRef>` when opRef is
// non-empty and the 1Password CLI is on PATH. Empty return means
// "no key available" - caller should fall back to Piper.
func ResolveElevenLabsKey(opRef string) string {
	if k := strings.TrimSpace(os.Getenv("ELEVENLABS_API_KEY")); k != "" {
		return k
	}
	if opRef == "" {
		return ""
	}
	if _, err := exec.LookPath("op"); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "op", "read", opRef).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
