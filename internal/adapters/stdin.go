package adapters

import (
	"context"
	"fmt"
	"io"
	"os"
)

// StdinAdapter reads the prose blob from os.Stdin. Useful for
// pipelines (`cat transcript.md | aida tasks ingest --from stdin`)
// and for adapter wrappers that want to compose with shell tools
// before handing off to aida.
type StdinAdapter struct{}

// NewStdin returns an IngestSource backed by os.Stdin.
func NewStdin() *StdinAdapter { return &StdinAdapter{} }

// Name implements IngestSource.
func (StdinAdapter) Name() string { return "stdin" }

// Read implements IngestSource. Reads to EOF on stdin. Honors ctx
// cancellation by closing stdin reads asynchronously when the
// context is done - best-effort, since os.Stdin doesn't expose a
// SetReadDeadline equivalent.
func (StdinAdapter) Read(ctx context.Context) (string, error) {
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(os.Stdin)
		ch <- result{data: data, err: err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return "", fmt.Errorf("read stdin: %w", r.err)
		}
		return string(r.data), nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
