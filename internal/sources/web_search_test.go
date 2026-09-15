package sources

import (
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
)

// Web-search snippets carry no inherent retrieval time (unlike, say, a
// sqlite row's CREATED_AT column) - without a stamped timestamp,
// nothing downstream can reason about how stale a web result is. This
// test asserts artifacts built by the adapter carry a Timestamp that
// parses as RFC3339, matching the convention used elsewhere (e.g.
// CurrentTimeAdapter).
func TestWebSearchAdapterParseOutput_StampsParseableTimestamp(t *testing.T) {
	a := &WebSearchAdapter{}
	artifacts, err := a.ParseOutput([]byte("some raw output"), config.Source{})
	if err != nil {
		t.Fatalf("ParseOutput returned error: %v", err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("expected 1 artifact, got %d", len(artifacts))
	}
	ts := artifacts[0].Timestamp
	if ts == "" {
		t.Fatal("expected non-empty timestamp")
	}
	if _, err := time.Parse(time.RFC3339, ts); err != nil {
		t.Errorf("Timestamp %q is not valid RFC3339: %v", ts, err)
	}
}
