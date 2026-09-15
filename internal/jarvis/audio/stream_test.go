package audio

import (
	"bytes"
	"os"
	"testing"
)

// TestPCMStreamFlush verifies Flush drains the pipe backlog (the mic audio
// that piles up while the listener is blocked playing Jarvis's reply) and
// leaves the stream sample-aligned so the next chunk read is uncorrupted.
func TestPCMStreamFlush(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer pr.Close()
	defer pw.Close()

	s := &PCMStream{r: pr, stop: func() error { return nil }}

	// Simulate a backlog of captured self-audio sitting in the pipe.
	backlog := bytes.Repeat([]byte{0x11, 0x22}, 4000) // 8000 bytes, even
	if _, err := pw.Write(backlog); err != nil {
		t.Fatalf("seed backlog: %v", err)
	}

	dropped := s.Flush()
	if dropped != len(backlog) {
		t.Fatalf("Flush dropped %d bytes, want %d", dropped, len(backlog))
	}

	// After the flush, a fresh write must be read back byte-for-byte: the
	// drain must not have left the stream misaligned or consumed live data.
	fresh := []byte{0xAA, 0xBB, 0xCC, 0xDD}
	if _, err := pw.Write(fresh); err != nil {
		t.Fatalf("write fresh: %v", err)
	}
	got := make([]byte, len(fresh))
	if _, err := s.Read(got); err != nil {
		t.Fatalf("read after flush: %v", err)
	}
	if !bytes.Equal(got, fresh) {
		t.Fatalf("post-flush read = %x, want %x (stream misaligned)", got, fresh)
	}
}

// TestPCMStreamFlushEmpty confirms Flush on an idle pipe drops nothing and
// returns promptly (bounded by flushReadDeadline), so calling it on every
// spoken turn is cheap even when there's no backlog.
func TestPCMStreamFlushEmpty(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer pr.Close()
	defer pw.Close()

	s := &PCMStream{r: pr, stop: func() error { return nil }}
	if dropped := s.Flush(); dropped != 0 {
		t.Fatalf("Flush on empty pipe dropped %d bytes, want 0", dropped)
	}
}
