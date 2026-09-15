package adapters

import (
	"context"
	"fmt"
	"os"
)

// FileAdapter reads the prose blob from a single filesystem path.
// Used for things like `aida tasks ingest --from-file
// meeting-notes.md` where the user already has the transcript
// saved locally.
type FileAdapter struct {
	path string
}

// NewFile returns an IngestSource that reads the given path on Read.
// The path is resolved at Read time - caller is free to construct
// FileAdapter values without the file existing yet.
func NewFile(path string) *FileAdapter {
	return &FileAdapter{path: path}
}

// Name implements IngestSource. Returns "file:<path>" so logs and
// exec-plan origins point at the actual ingested file.
func (a FileAdapter) Name() string {
	if a.path == "" {
		return "file:(unset)"
	}
	return "file:" + a.path
}

// Read implements IngestSource. Reads the entire file into memory.
// For ingestion blobs that's fine; we cap practical sizes via the
// extraction prompt rather than here.
//
// Honors ctx cancellation by checking before opening - pure os.ReadFile
// doesn't have a deadline, but task ingestion blobs are usually
// small enough that the read completes within a single ctx tick.
func (a FileAdapter) Read(ctx context.Context) (string, error) {
	if a.path == "" {
		return "", fmt.Errorf("FileAdapter: empty path")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	data, err := os.ReadFile(a.path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", a.path, err)
	}
	return string(data), nil
}
