// Package adapters defines the generic source-ingestion shape that
// upstream adapters (stdin, file, Notion meeting, Slack thread,
// GitHub issue, cron, webhook) all conform to. Downstream pipeline
// code consumes prose through this interface and never knows where
// it came from - adding a new origin means writing a new adapter,
// not changing the ingestion / extraction / planning pipeline.
//
// Per the "design for the platform, not the use case" feedback
// memory: a Notion-meeting integration is one adapter among many.
// The platform primitive is "blob of prose → structured tasks → exec
// plans"; how the prose is acquired is the adapter's problem alone.
package adapters

import "context"

// IngestSource is anything that can produce a single prose blob
// for the task-extraction LLM call to operate on. Adapters return
// the *raw* content; downstream layers handle prompting, parsing,
// and persistence.
//
// The interface intentionally has no notion of "tasks" - that's
// the extraction layer's vocabulary. An adapter could yield a
// meeting transcript, a Slack thread, a file's contents, or
// anything else; the contract is just "give me the words."
type IngestSource interface {
	// Name is a stable identifier surfaced in run logs, exec-plan
	// origins, and verbose output. Conventional values:
	// "stdin", "file:<path>", "notion:<page-id>", "slack:<thread-ts>".
	Name() string

	// Read returns the prose content for downstream extraction.
	// Long-running adapters honor ctx for cancellation. Empty
	// output is valid - the extraction layer treats it as
	// "no tasks to ingest" rather than an error.
	Read(ctx context.Context) (string, error)
}
