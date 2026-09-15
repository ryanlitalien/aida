package roster

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// sourceBackendTimeout bounds an `aida --source <name> <task>` call. A
// pinned source can still fan out to a real backend (a SQL warehouse, grep,
// an exec source, ...), so this is generous compared to sub-second local
// lookups, mirroring subagent's DefaultSubagentTimeout order of magnitude.
const sourceBackendTimeout = 240 * time.Second

// ansiEscapePattern matches ANSI color/style escape codes so a source's
// terminal-formatted output can be shown or logged as plain text.
var ansiEscapePattern = regexp.MustCompile("\x1b\\[[0-9;]*m")

// stripANSI removes ANSI escape codes from s. Pure and separately testable
// from the process-execution plumbing in Ask.
func stripANSI(s string) string {
	return ansiEscapePattern.ReplaceAllString(s, "")
}

// sourceArgs builds the `aida` argv that pins a request to one library
// source via the existing `--source NAME` flag. Pure and separately
// testable from Ask's process plumbing.
func sourceArgs(source, task string) []string {
	return []string{"--source", source, task}
}

// sourceBackend delegates a Request to `aida --source <name> <task>`,
// reusing the existing top-level --source pin rather than a new code path.
type sourceBackend struct {
	name   string
	source string
}

// newSourceBackend builds the sourceBackend for e, requiring a populated
// source: spec block.
func newSourceBackend(e *Entry, _ Deps) (Backend, error) {
	if e.Source == nil || e.Source.Source == "" {
		return nil, fmt.Errorf("entry %q has kind %q but no source.source", e.Name, KindSource)
	}
	return &sourceBackend{name: e.Name, source: e.Source.Source}, nil
}

// Kind implements Backend.
func (b *sourceBackend) Kind() string { return KindSource }

// Ask implements Backend by shelling `aida --source <name> <task>` and
// scrubbing the terminal formatting out of its combined output.
//
// Backend problems -- a timeout, a non-zero exit, empty output -- come back
// as a Result status, never a Go error: only exec.LookPath failing to find
// the `aida` binary at all is treated as a hard error path here, and even
// that is reported as a Result so callers get uniform handling.
func (b *sourceBackend) Ask(ctx context.Context, req Request) (Result, error) {
	start := time.Now()

	aidaBin, err := exec.LookPath("aida")
	if err != nil {
		return Result{
			Entry:  b.name,
			Status: StatusError,
			Text:   err.Error(),
			TookMS: time.Since(start).Milliseconds(),
		}, nil
	}

	runCtx, cancel := context.WithTimeout(ctx, sourceBackendTimeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, aidaBin, sourceArgs(b.source, req.Task)...)
	out, runErr := cmd.CombinedOutput()
	took := time.Since(start).Milliseconds()
	cleaned := strings.TrimSpace(stripANSI(string(out)))

	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return Result{
			Entry:  b.name,
			Status: StatusTimeout,
			Text:   "timed out after " + sourceBackendTimeout.String(),
			TookMS: took,
		}, nil
	}
	if runErr != nil {
		text := cleaned
		if text == "" {
			text = runErr.Error()
		}
		return Result{
			Entry:  b.name,
			Status: StatusError,
			Text:   text,
			TookMS: took,
		}, nil
	}
	if cleaned == "" {
		return Result{
			Entry:  b.name,
			Status: StatusEmpty,
			Text:   "source returned no output",
			TookMS: took,
		}, nil
	}

	return Result{
		Entry:  b.name,
		Status: StatusSuccess,
		Text:   cleaned,
		TookMS: took,
	}, nil
}
