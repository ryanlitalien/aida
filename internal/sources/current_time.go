package sources

import (
	"context"
	"fmt"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
)

// CurrentTimeAdapter answers "what time is it" / "what's today's date"
// style questions from the local system clock. It ignores src.Exec
// entirely (there's no external command to run) and builds its single
// artifact directly in Go, mirroring WebSearchAdapter's self-contained
// shape.
type CurrentTimeAdapter struct{}

func (a *CurrentTimeAdapter) Execute(ctx context.Context, command string, src config.Source) (SourceResult, error) {
	loc := time.Local
	if cfg, err := config.LoadConfig(); err == nil && cfg != nil {
		loc = cfg.Location()
	}
	now := time.Now().In(loc)

	name := "current-time"
	snippet := fmt.Sprintf("%s (%s, %s, %s)",
		now.Format("3:04 PM"),
		now.Format("Monday, January 2, 2006"),
		now.Format("MST"),
		loc.String(),
	)

	artifact := Artifact{
		Type:      "page",
		ID:        "local-clock",
		Timestamp: now.Format(time.RFC3339),
		Snippet:   snippet,
	}

	return SourceResult{
		Source:    name,
		Status:    "success",
		Summary:   "Local system clock: " + snippet,
		Artifacts: []Artifact{artifact},
		Command:   command,
	}, nil
}

func (a *CurrentTimeAdapter) ParseOutput(raw []byte, src config.Source) ([]Artifact, error) {
	return []Artifact{{Type: "page", ID: "local-clock", Snippet: string(raw)}}, nil
}

func init() {
	RegisterAdapter("current-time", &CurrentTimeAdapter{})
}
