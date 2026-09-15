package audio

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Flush tunables. flushReadDeadline is how long a single drain read waits
// for more data before concluding the pipe is momentarily empty: shorter
// than the gap between live device callbacks so we stop at the boundary
// between the instantly-available backlog and the real-time stream.
// flushBudget is a hard wall-clock ceiling so a pathologically chatty
// capture (live PCM trickling in faster than flushReadDeadline) can't pin
// the listener - worst case we discard flushBudget of audio and move on.
const (
	flushReadDeadline = 15 * time.Millisecond
	flushBudget       = 200 * time.Millisecond
)

// PCMStream is an open ffmpeg subprocess piping 16kHz s16le mono PCM from the
// default macOS microphone to stdout. The reader yields raw PCM bytes that
// the caller chunks however it likes (typically 10–30ms windows).
type PCMStream struct {
	cmd    *exec.Cmd
	r      io.ReadCloser
	stderr *bytes.Buffer
	stop   func() error
}

// StartPCMStream launches ffmpeg in continuous capture mode. Default device
// is :0 (system default mic). 16000 Hz, mono, signed 16-bit little-endian.
//
// Cancel ctx (or call Stop) to terminate the subprocess.
func StartPCMStream(ctx context.Context, device string) (*PCMStream, error) {
	if device == "" {
		device = ":0"
	}
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner",
		"-loglevel", "warning",
		"-f", "avfoundation",
		"-i", device,
		"-f", "s16le",
		"-ar", "16000",
		"-ac", "1",
		"-flush_packets", "1",
		"-",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg stdout pipe: %w", err)
	}
	stderrBuf := &bytes.Buffer{}
	cmd.Stderr = stderrBuf
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ffmpeg start: %w", err)
	}
	s := &PCMStream{
		cmd:    cmd,
		r:      stdout,
		stderr: stderrBuf,
		stop: func() error {
			_ = cmd.Process.Kill()
			return cmd.Wait()
		},
	}
	return s, nil
}

func (s *PCMStream) Read(p []byte) (int, error) {
	n, err := io.ReadFull(s.r, p)
	if err != nil && s.stderr != nil {
		// Filter benign avfoundation warnings (camera Continuity, etc.)
		// before deciding whether to attach stderr to the error. If
		// nothing real remains, return the bare EOF.
		filtered := filterBenignFFmpegStderr(s.stderr.String())
		if filtered != "" {
			return n, fmt.Errorf("%w (ffmpeg stderr: %s)", err, filtered)
		}
	}
	return n, err
}

func filterBenignFFmpegStderr(s string) string {
	if s == "" {
		return ""
	}
	var keep []string
	for _, line := range strings.Split(s, "\n") {
		l := strings.TrimSpace(line)
		if l == "" {
			continue
		}
		// macOS Continuity Camera warning fires whenever avfoundation
		// is initialized; nothing we do affects it.
		if strings.Contains(l, "NSCameraUseContinuityCameraDeviceType") {
			continue
		}
		keep = append(keep, l)
	}
	return strings.Join(keep, " | ")
}

// Flush discards any PCM sitting in the pipe and returns the number of
// bytes dropped. The listener calls this the instant Jarvis stops
// speaking: while the single-threaded loop was blocked in playback the
// mic kept capturing Jarvis's own (afplay-boosted) voice into the OS
// pipe buffer. Reading that backlog back through VAD is what makes the
// assistant transcribe - and, when armed, act on - its own speech (the
// self-echo / barge-in misfire). Draining it before resuming the loop
// closes that loop.
//
// It reads with a short per-read deadline so it stops at the boundary
// between the backlog (available instantly) and the live stream (which
// arrives one device callback at a time, with gaps longer than the
// deadline), bounded by flushBudget so trickle can't pin it. Sample
// (2-byte) alignment is preserved across the drain.
func (s *PCMStream) Flush() int {
	f, ok := s.r.(*os.File)
	if !ok {
		return 0 // not a pollable pipe; nothing we can do
	}
	hardStop := time.Now().Add(flushBudget)
	buf := make([]byte, 64*1024)
	var dropped int
	for time.Now().Before(hardStop) {
		_ = f.SetReadDeadline(time.Now().Add(flushReadDeadline))
		n, err := f.Read(buf)
		dropped += n
		if err != nil || n == 0 { // deadline (pipe drained) or EOF
			break
		}
	}
	// A timed read can split a 2-byte sample; one more byte resyncs the
	// stream to a sample boundary so subsequent fixed-size chunk reads
	// stay aligned. Best effort - a stray byte of skew is harmless to STT.
	if dropped%2 == 1 {
		_ = f.SetReadDeadline(time.Now().Add(flushReadDeadline))
		var one [1]byte
		if n, _ := f.Read(one[:]); n == 1 {
			dropped++
		}
	}
	_ = f.SetReadDeadline(time.Time{}) // restore blocking reads
	return dropped
}

func (s *PCMStream) Stop() error { return s.stop() }
