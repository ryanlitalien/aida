package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ryanlitalien/aida/internal/brain"
)

// ─── memory_save ─────────────────────────────────────────────────────────────
//
// Lets Jarvis persist a durable fact into the same captured-memory store that
// claude_memory_recall reads. Closes the write half of the memory loop: before
// this tool, "save my Volkswagen to my profile" could only READ (recall) and
// then falsely imply a save - Jarvis had no way to write.
//
// Two-phase, confirm-before-commit protocol (the user asked for this
// explicitly): the FIRST call (confirm=false, the default) stages the record
// and returns a read-back of exactly what will be saved and where, so Jarvis
// speaks it and asks "OK to commit this to memory?". Only after the user
// approves does Jarvis call again with confirm=true, which writes it.
//
// The staged write lives in a per-registry pending slot. The daemon builds one
// registry per Assistant and reuses it across wake turns, so the stage set on
// the propose turn is still present on the confirm turn. As a fallback (fresh
// process between the two turns, or the model re-passing the fields), a commit
// call may also carry content/key and will write those if nothing is staged.
//
// Records are written under brain.ClaudeMemoryProfile ("claude") - the profile
// claude_memory_recall queries - so a saved fact round-trips: next time the
// user asks "what do you know about my car", recall finds it. A stable `key`
// makes a re-save supersede the prior record instead of duplicating it.

type memorySaveInput struct {
	Content string `json:"content"`
	Key     string `json:"key"`
	Confirm bool   `json:"confirm"`
}

// stagedMemory is the pending write held between the propose and commit calls.
type stagedMemory struct {
	mu     sync.Mutex
	record *brain.MemoryRecord
}

func buildMemoryRecord(content, key string) *brain.MemoryRecord {
	return &brain.MemoryRecord{
		Type:    brain.MemoryFact,
		Key:     key,
		Body:    content,
		Tags:    []string{"jarvis-saved", "scope:global"},
		Profile: brain.ClaudeMemoryProfile,
		Source:  "jarvis-voice",
	}
}

func memorySaveTool(b *brain.Brain) Tool {
	staged := &stagedMemory{}
	return Tool{
		Name: "memory_save",
		Description: "Persist a durable fact about the user into your long-term brain memory - " +
			"the same store you read with claude_memory_recall. USE THIS when the user says " +
			"'save this', 'remember this', 'add this to my profile', 'commit this to memory', " +
			"'note this about me', or otherwise asks you to durably keep a fact (a vehicle they " +
			"own, a preference, a person, a project detail). This WRITES; claude_memory_recall READS.\n\n" +
			"ALWAYS use the two-step confirm protocol:\n" +
			"1. First call with confirm=false (the default) and the `content` to save. This does " +
			"NOT write - it stages the record and returns a read-back. Speak the read-back to the " +
			"user: repeat exactly what you'll save and where, then ask 'Is it OK for me to commit " +
			"this to memory, sir?'. Then STOP and wait for their reply - do NOT call memory_save " +
			"with confirm=true in the same turn.\n" +
			"2. Only after the user approves (their next message), call memory_save again with " +
			"confirm=true to commit it. If they decline or change it, either drop it or re-propose " +
			"with confirm=false.\n\n" +
			"Write `content` as a self-contained fact about the user, e.g. " +
			"'Ryan owns a 2021 VW Atlas SE V6 4Motion (~5,000 lb tow capacity with the factory hitch)'. " +
			"Set a short stable `key` (e.g. 'vehicle-atlas') so re-saving updates the same fact instead " +
			"of duplicating it.",
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"content": map[string]interface{}{
					"type":        "string",
					"description": "The fact to save, as a self-contained sentence about the user. Required to propose.",
				},
				"key": map[string]interface{}{
					"type":        "string",
					"description": "Optional short stable slug (e.g. 'vehicle-atlas') so a re-save supersedes the prior fact instead of duplicating it.",
				},
				"confirm": map[string]interface{}{
					"type":        "boolean",
					"description": "false (default) = propose and read back, do NOT write. true = commit the previously-proposed save, only after the user approved.",
				},
			},
		},
		Run: func(_ context.Context, raw json.RawMessage) (string, error) {
			if b == nil {
				return "I can't save to memory without a brain attached, sir.", nil
			}
			var in memorySaveInput
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &in); err != nil {
					return "", err
				}
			}
			in.Content = strings.TrimSpace(in.Content)
			in.Key = strings.TrimSpace(in.Key)

			if !in.Confirm {
				// Propose: stage, do not write.
				if in.Content == "" {
					return "Tell me what you'd like me to remember, sir, and I'll read it back before saving.", nil
				}
				staged.mu.Lock()
				staged.record = buildMemoryRecord(in.Content, in.Key)
				staged.mu.Unlock()
				keyNote := ""
				if in.Key != "" {
					keyNote = fmt.Sprintf(" under key %q", in.Key)
				}
				return fmt.Sprintf(
					"PROPOSED - not saved yet. I will store this to your long-term brain memory "+
						"(fact store, profile \"claude\")%s:\n\n  %s\n\n"+
						"Read this back to the user verbatim, then ask: \"Is it OK for me to commit this to memory, sir?\" "+
						"Do NOT call memory_save with confirm=true until they approve.",
					keyNote, in.Content,
				), nil
			}

			// Commit.
			staged.mu.Lock()
			rec := staged.record
			staged.record = nil
			staged.mu.Unlock()

			if rec == nil {
				// Fallback: no stage survived (fresh process, or model re-passed
				// the fields on the confirm call) - commit the re-passed content.
				if in.Content == "" {
					return "There's nothing staged to commit, sir - tell me what to save first.", nil
				}
				rec = buildMemoryRecord(in.Content, in.Key)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			saved, err := b.WriteMemory(ctx, *rec)
			if err != nil {
				return "", fmt.Errorf("memory_save commit: %w", err)
			}
			path := filepath.Join("brain", "memory", string(saved.Type), saved.ID+".json")
			return fmt.Sprintf(
				"Committed to memory. Saved to %s (profile %q). Confirm to the user that it's saved: "+
					"\"Saved to your brain's long-term memory, sir - it'll come back next time you ask.\"",
				path, saved.Profile,
			), nil
		},
	}
}
