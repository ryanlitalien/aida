package jarvis

import (
	"fmt"
	"testing"

	"github.com/ryanlitalien/aida/internal/jarvis/audit"
)

func TestActivity_CurrentTool(t *testing.T) {
	a := newActivity()
	if s := a.Snapshot(); s.CurrentTool != "" {
		t.Errorf("fresh activity should be idle, got %q", s.CurrentTool)
	}
	a.SetCurrentTool("minecraft_ask")
	if s := a.Snapshot(); s.CurrentTool != "minecraft_ask" {
		t.Errorf("current tool = %q, want minecraft_ask", s.CurrentTool)
	}
	a.ClearCurrentTool()
	if s := a.Snapshot(); s.CurrentTool != "" {
		t.Errorf("after clear should be idle, got %q", s.CurrentTool)
	}
}

func TestActivity_RingNewestFirstAndCapped(t *testing.T) {
	a := newActivity() // cap 10
	for i := 0; i < 13; i++ {
		a.PushTurn(audit.Record{Query: fmt.Sprintf("q%d", i), Reply: "ok"})
	}
	s := a.Snapshot()
	if len(s.Recent) != 10 {
		t.Fatalf("ring should cap at 10, got %d", len(s.Recent))
	}
	if s.Recent[0].Query != "q12" {
		t.Errorf("newest-first: want q12 first, got %q", s.Recent[0].Query)
	}
	if s.Recent[9].Query != "q3" {
		t.Errorf("oldest kept should be q3, got %q", s.Recent[9].Query)
	}
}

func TestActivity_PushTurnCapturesToolsAndError(t *testing.T) {
	a := newActivity()
	a.PushTurn(audit.Record{
		Query:     "make the fifth layer",
		Reply:     "done, sir",
		ToolCalls: []audit.ToolCall{{Name: "minecraft_ask"}, {Name: "jarvis_thumbs_up"}},
	})
	a.PushTurn(audit.Record{Query: "boom", Error: "tts failed"})
	s := a.Snapshot()
	if s.Recent[0].Error != "tts failed" { // newest first
		t.Errorf("error not captured: %+v", s.Recent[0])
	}
	if len(s.Recent[1].Tools) != 2 || s.Recent[1].Tools[0] != "minecraft_ask" {
		t.Errorf("tools not captured: %+v", s.Recent[1].Tools)
	}
}
