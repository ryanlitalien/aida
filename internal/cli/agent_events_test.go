package cli

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readEvents parses events.ndjson into a slice of envelopes, one per
// line. Each envelope has the {ts, type, data} shape; type is the
// stable contract - tests assert against type sequence + data fields.
func readEvents(t *testing.T, dir string) []map[string]any {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "events.ndjson"))
	if err != nil {
		t.Fatalf("open events: %v", err)
	}
	defer f.Close()

	var out []map[string]any
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var env map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &env); err != nil {
			t.Fatalf("unmarshal line %q: %v", scanner.Text(), err)
		}
		out = append(out, env)
	}
	return out
}

func TestRunDirSinkEmitsStartImmediately(t *testing.T) {
	dir := t.TempDir()
	s, err := openRunDirSink(dir, "20260506-test", "what is the meaning of life?")
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	defer s.Close()

	evs := readEvents(t, dir)
	if len(evs) != 1 || evs[0]["type"] != "start" {
		t.Fatalf("expected single start event, got %+v", evs)
	}
	data := evs[0]["data"].(map[string]any)
	if data["run_id"] != "20260506-test" {
		t.Errorf("run_id wrong: %v", data["run_id"])
	}
	if data["question"] != "what is the meaning of life?" {
		t.Errorf("question wrong: %v", data["question"])
	}
}

func TestRunDirSinkVerboseAndToolCall(t *testing.T) {
	dir := t.TempDir()
	s, _ := openRunDirSink(dir, "rid", "q")
	defer s.Close()

	s.emitVerbose("Agent tools", "7 tools available")
	s.emitToolCall(1, "grep", `{"pattern":"foo"}`, "matched 3 files", "")
	s.emitToolCall(2, "read", `{"path":"x"}`, "", "permission denied")

	evs := readEvents(t, dir)
	if len(evs) != 4 {
		t.Fatalf("expected 4 events (start+verbose+2 tool_call), got %d", len(evs))
	}
	if evs[1]["type"] != "verbose" {
		t.Errorf("evs[1] type = %v", evs[1]["type"])
	}
	if evs[2]["type"] != "tool_call" {
		t.Errorf("evs[2] type = %v", evs[2]["type"])
	}
	d2 := evs[2]["data"].(map[string]any)
	if d2["status"] != "ok" {
		t.Errorf("evs[2] status = %v", d2["status"])
	}
	d3 := evs[3]["data"].(map[string]any)
	if d3["status"] != "error" || d3["error"] != "permission denied" {
		t.Errorf("evs[3] error fields wrong: %v", d3)
	}
}

func TestRunDirSinkCompleteAndOutput(t *testing.T) {
	dir := t.TempDir()
	s, _ := openRunDirSink(dir, "rid", "q")

	if err := s.writeOutput("the answer is 42\n"); err != nil {
		t.Fatalf("writeOutput: %v", err)
	}
	s.emitComplete(3, 0.0123, 17)
	s.Close()

	// output.md exists with expected bytes.
	out, err := os.ReadFile(filepath.Join(dir, "output.md"))
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(out) != "the answer is 42\n" {
		t.Errorf("output.md = %q", string(out))
	}

	// No ANSI escape bytes in output.md - the run-dir contract.
	if strings.ContainsRune(string(out), '\x1b') {
		t.Error("output.md contains ANSI escape; run-dir mode must be ANSI-free")
	}

	evs := readEvents(t, dir)
	last := evs[len(evs)-1]
	if last["type"] != "complete" {
		t.Errorf("last event type = %v, want complete", last["type"])
	}
	d := last["data"].(map[string]any)
	if int(d["turns"].(float64)) != 3 {
		t.Errorf("turns = %v", d["turns"])
	}
}

func TestRunDirSinkErrorEvent(t *testing.T) {
	dir := t.TempDir()
	s, _ := openRunDirSink(dir, "rid", "q")
	s.emitError("model returned 500")
	s.Close()

	evs := readEvents(t, dir)
	last := evs[len(evs)-1]
	if last["type"] != "error" {
		t.Errorf("last event type = %v, want error", last["type"])
	}
	if last["data"].(map[string]any)["message"] != "model returned 500" {
		t.Errorf("error message wrong: %+v", last["data"])
	}
}

func TestRunDirSinkConcurrentEmits(t *testing.T) {
	// Stream-sink goroutine and main goroutine race on emit.
	// Verify writes don't interleave (every line is valid JSON).
	dir := t.TempDir()
	s, _ := openRunDirSink(dir, "rid", "q")
	defer s.Close()

	const n = 100
	done := make(chan struct{}, 2)
	go func() {
		for i := 0; i < n; i++ {
			s.emitVerbose("a", "msg from goroutine 1")
		}
		done <- struct{}{}
	}()
	go func() {
		for i := 0; i < n; i++ {
			s.emitToolCall(i, "tool", `{"k":"v"}`, "out", "")
		}
		done <- struct{}{}
	}()
	<-done
	<-done

	// Read every line back; if any was interleaved the JSON parse
	// would fail. readEvents fatally errors on a parse failure so a
	// pass here is a sufficient race check.
	evs := readEvents(t, dir)
	if len(evs) != 1+2*n {
		t.Errorf("expected %d events, got %d", 1+2*n, len(evs))
	}
}
