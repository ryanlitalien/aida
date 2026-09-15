package brain

// Codex source for the multi-agent memory bridge harvester (harvest.go).
//
// Verified on-disk shapes (2026-08):
//
//   - ~/.codex/session_index.jsonl -- one JSON object per line,
//     {"id", "thread_name", "updated_at"}. Cheap: this is what watermark
//     filtering runs against, before any rollout file is read.
//   - ~/.codex/sessions/<YYYY>/<MM>/<DD>/rollout-<ts>-<id>.jsonl -- the
//     full transcript for one session id. The date-partition directory
//     is keyed off the rollout's *creation* time, which need not match
//     session_index's updated_at (a session can be forked/resumed), so
//     the file is located by walking the tree for a name ending in
//     "-<id>.jsonl" rather than guessing a path from the id.
//     First line is type=="session_meta" with payload.cwd -- the
//     project scope. Subsequent lines are typed events; only
//     type=="response_item" with payload.type=="message" and
//     payload.role in {user, assistant} carry conversation text (their
//     payload.content[].text where type is input_text/output_text).
//     Everything else (reasoning, function_call, function_call_output,
//     developer/system messages, turn_context, world_state, ...) is
//     tool/scaffolding noise and is skipped.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// codexTranscriptHalfBudget is the per-half (head/tail) character cap
// fed to capTranscript -- ~30k total per session, per the harvest spec.
const codexTranscriptHalfBudget = 15000

// codexSessionMeta is the cheap (id, updated_at, thread_name) triple
// read from session_index.jsonl.
type codexSessionMeta struct {
	ID         string
	ThreadName string
	UpdatedAt  time.Time
}

// listCodexSessionMetas reads ~/.codex/session_index.jsonl. Missing
// file is not an error (Codex simply isn't installed / has no
// sessions yet) -- returns an empty slice.
func listCodexSessionMetas(codexHome string) ([]codexSessionMeta, error) {
	path := filepath.Join(codexHome, "session_index.jsonl")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []codexSessionMeta
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var raw struct {
			ID         string `json:"id"`
			ThreadName string `json:"thread_name"`
			UpdatedAt  string `json:"updated_at"`
		}
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			continue
		}
		if raw.ID == "" {
			continue
		}
		t, err := parseFlexibleRFC3339(raw.UpdatedAt)
		if err != nil {
			continue
		}
		out = append(out, codexSessionMeta{ID: raw.ID, ThreadName: raw.ThreadName, UpdatedAt: t})
	}
	return out, sc.Err()
}

// findCodexRolloutPath locates the rollout file for a session id under
// codexHome/sessions/**. Returns "" (no error) when not found.
func findCodexRolloutPath(codexHome, id string) (string, error) {
	root := filepath.Join(codexHome, "sessions")
	suffix := "-" + id + ".jsonl"
	var found string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // best-effort: skip unreadable entries
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), suffix) {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	return found, nil
}

// codexResponseItemContent is one element of a response_item message's
// content array.
type codexResponseItemContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// buildCodexHarvestSession reads and parses one rollout file into a
// HarvestSession. ok is false when the rollout couldn't be located or
// carried no user/assistant text worth distilling (both are normal,
// not errors).
func buildCodexHarvestSession(codexHome string, meta codexSessionMeta) (sess HarvestSession, ok bool, err error) {
	path, err := findCodexRolloutPath(codexHome, meta.ID)
	if err != nil {
		return HarvestSession{}, false, err
	}
	if path == "" {
		return HarvestSession{}, false, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return HarvestSession{}, false, err
	}
	defer f.Close()

	var cwd string
	var text strings.Builder
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var env struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal([]byte(line), &env); err != nil {
			continue
		}
		switch env.Type {
		case "session_meta":
			if cwd == "" {
				var m struct {
					Cwd string `json:"cwd"`
				}
				_ = json.Unmarshal(env.Payload, &m)
				cwd = m.Cwd
			}
		case "response_item":
			var item struct {
				Type    string                     `json:"type"`
				Role    string                     `json:"role"`
				Content []codexResponseItemContent `json:"content"`
			}
			if err := json.Unmarshal(env.Payload, &item); err != nil {
				continue
			}
			if item.Type != "message" {
				continue
			}
			label := ""
			switch item.Role {
			case "user":
				label = "User"
			case "assistant":
				label = "Assistant"
			default:
				continue // skip developer/system boilerplate
			}
			for _, c := range item.Content {
				if c.Text == "" {
					continue
				}
				if c.Type != "input_text" && c.Type != "output_text" {
					continue
				}
				fmt.Fprintf(&text, "%s: %s\n\n", label, c.Text)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return HarvestSession{}, false, err
	}

	transcript := strings.TrimSpace(text.String())
	if transcript == "" {
		return HarvestSession{}, false, nil
	}

	scope := "global"
	if cwd != "" {
		scope = "project:" + cwdToProjectSlug(cwd)
	}

	return HarvestSession{
		ID:         meta.ID,
		UpdatedAt:  meta.UpdatedAt,
		Scope:      scope,
		Transcript: capTranscript(transcript, codexTranscriptHalfBudget, codexTranscriptHalfBudget),
		SourceRef:  "codex:" + path,
	}, true, nil
}

// HarvestCodex distills new/changed Codex sessions into typed memory
// records under the "codex" profile, incrementally via a persisted
// watermark. distill is typically client.CompleteJSON.
func (b *Brain) HarvestCodex(ctx context.Context, distill DistillFunc, opts HarvestOptions) (*HarvestResult, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home dir: %w", err)
	}
	return b.harvestCodex(ctx, filepath.Join(home, ".codex"), distill, opts)
}

// harvestCodex is the injectable core: codexHome is overridable so
// tests point at a fixture tree instead of the real ~/.codex. Two-phase:
// list cheap metadata, filter via selectHarvestSessions, then read only
// the surviving rollout files (avoids re-reading a large session history
// on every run).
func (b *Brain) harvestCodex(ctx context.Context, codexHome string, distill DistillFunc, opts HarvestOptions) (*HarvestResult, error) {
	metas, err := listCodexSessionMetas(codexHome)
	if err != nil {
		return nil, fmt.Errorf("list codex sessions: %w", err)
	}

	wm := readHarvestWatermark(b.Path, "codex")
	stand := make([]HarvestSession, len(metas))
	metaByID := make(map[string]codexSessionMeta, len(metas))
	for i, m := range metas {
		stand[i] = HarvestSession{ID: m.ID, UpdatedAt: m.UpdatedAt}
		metaByID[m.ID] = m
	}
	selectedStand, skippedQuiet := selectHarvestSessions(stand, wm, opts)

	var sessions []HarvestSession
	for _, s := range selectedStand {
		sess, ok, err := buildCodexHarvestSession(codexHome, metaByID[s.ID])
		if err != nil {
			continue
		}
		if !ok {
			continue
		}
		sessions = append(sessions, sess)
	}

	result, err := b.harvestDistilledSessions(ctx, "codex", sessions, distill, opts.DryRun)
	if err != nil {
		return nil, err
	}
	result.CandidatesTotal = len(metas)
	result.SkippedQuiet = skippedQuiet
	result.Selected = len(sessions)
	return result, nil
}
