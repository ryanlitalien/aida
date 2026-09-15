package sources

import (
	"context"
	"testing"

	"github.com/ryanlitalien/aida/internal/config"
)

func TestCurrentTimeAdapterExecute(t *testing.T) {
	a := &CurrentTimeAdapter{}
	result, err := a.Execute(context.Background(), "", config.Source{})
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result.Status != "success" {
		t.Errorf("expected status 'success', got %q", result.Status)
	}
	if len(result.Artifacts) != 1 {
		t.Fatalf("expected 1 artifact, got %d", len(result.Artifacts))
	}
	art := result.Artifacts[0]
	if art.Snippet == "" {
		t.Error("expected non-empty snippet")
	}
	if art.Timestamp == "" {
		t.Error("expected non-empty timestamp")
	}
	if result.Summary == "" {
		t.Error("expected non-empty summary")
	}
}

func TestCurrentTimeAdapterRegistered(t *testing.T) {
	if GetAdapter("current-time") == nil {
		t.Error("expected 'current-time' adapter to be registered")
	}
}
