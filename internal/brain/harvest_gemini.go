package brain

// Gemini source for the multi-agent memory bridge harvester
// (harvest.go). Three sub-sources, verified on-disk 2026-08:
//
//   (a) Gemini CLI session logs:
//       ~/.gemini/tmp/<projectHash>/chats/session-*.json -- one JSON
//       object per session: {sessionId, projectHash, startTime,
//       lastUpdated, messages:[{id, timestamp, type, content}]}, type
//       is "user" or "gemini", content is a plain string. LLM-distilled
//       like Codex sessions. projectHash is a hash of the working
//       directory computed by the Gemini CLI; no reverse mapping from
//       hash -> path exists on disk, so scope is the opaque hash rather
//       than a human-legible project slug (documented limitation, see
//       docs/memory-bridge-multi-agent.md). The sibling
//       ~/.gemini/tmp/<hash>/logs.json is a redundant flat index of the
//       same messages and is not parsed.
//
//   (b) Antigravity artifacts:
//       ~/.gemini/antigravity-cli/brain/<conversation-id>/*.md are
//       already-distilled task/plan/walkthrough markdown -- mirrored
//       directly as event records, no LLM call. Scoped via
//       ~/.gemini/antigravity-cli/conversation_summaries.db (sqlite),
//       workspace_uris column, a JSON array of file:// URIs. Non-md
//       files (png/jpg/gif/pb/...) are always skipped by construction
//       (only *.md is walked).
//
//   (c) ~/.gemini/GEMINI.md, when present, is mirrored the same way
//       Claude Code's global CLAUDE.md is mirrored: a single global
//       instruction record, no LLM call.
//
// All three write under the "gemini" memory profile and share one
// watermark file (meta/harvest_watermark_gemini.json), namespaced by ID
// prefix ("gemini-cli:", "antigravity:", "gemini-md") so they never
// collide.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/ui"
)

// geminiTranscriptHalfBudget mirrors codexTranscriptHalfBudget for
// Gemini CLI session logs.
const geminiTranscriptHalfBudget = 15000

// geminiCLISessionFile is the shape of one
// ~/.gemini/tmp/<hash>/chats/session-*.json file.
type geminiCLISessionFile struct {
	SessionID   string `json:"sessionId"`
	ProjectHash string `json:"projectHash"`
	StartTime   string `json:"startTime"`
	LastUpdated string `json:"lastUpdated"`
	Messages    []struct {
		ID        string `json:"id"`
		Timestamp string `json:"timestamp"`
		Type      string `json:"type"` // "user" | "gemini"
		Content   string `json:"content"`
	} `json:"messages"`
}

// listGeminiCLISessionFiles enumerates every chats/session-*.json file
// under ~/.gemini/tmp/*/chats/. Missing ~/.gemini/tmp is not an error.
func listGeminiCLISessionFiles(geminiHome string) ([]string, error) {
	root := filepath.Join(geminiHome, "tmp")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		chatsDir := filepath.Join(root, e.Name(), "chats")
		chatEntries, err := os.ReadDir(chatsDir)
		if err != nil {
			continue // e.g. "bin" isn't a project-hash dir at all
		}
		for _, ce := range chatEntries {
			if ce.IsDir() || !strings.HasSuffix(ce.Name(), ".json") {
				continue
			}
			out = append(out, filepath.Join(chatsDir, ce.Name()))
		}
	}
	return out, nil
}

// readGeminiCLISession parses one session log into a HarvestSession. ok
// is false for an empty/no-text session (normal, not an error); a
// genuinely unparseable file returns an error so the caller can log and
// skip it per the harvest spec ("if the format is absent or
// unparseable, log and skip gracefully").
func readGeminiCLISession(path string) (sess HarvestSession, ok bool, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return HarvestSession{}, false, err
	}
	var f geminiCLISessionFile
	if err := json.Unmarshal(data, &f); err != nil {
		return HarvestSession{}, false, fmt.Errorf("unparseable gemini session log %s: %w", path, err)
	}
	if f.SessionID == "" || len(f.Messages) == 0 {
		return HarvestSession{}, false, nil
	}

	updated := f.LastUpdated
	if updated == "" {
		updated = f.StartTime
	}
	t, err := parseFlexibleRFC3339(updated)
	if err != nil {
		return HarvestSession{}, false, nil
	}

	var text strings.Builder
	for _, m := range f.Messages {
		content := strings.TrimSpace(m.Content)
		if content == "" {
			continue
		}
		label := "Gemini"
		if m.Type == "user" {
			label = "User"
		}
		fmt.Fprintf(&text, "%s: %s\n\n", label, content)
	}
	transcript := strings.TrimSpace(text.String())
	if transcript == "" {
		return HarvestSession{}, false, nil
	}

	scope := "global"
	if f.ProjectHash != "" {
		scope = "project:gemini-" + f.ProjectHash
	}

	return HarvestSession{
		ID:         "gemini-cli:" + f.SessionID,
		UpdatedAt:  t,
		Scope:      scope,
		Transcript: capTranscript(transcript, geminiTranscriptHalfBudget, geminiTranscriptHalfBudget),
		SourceRef:  "gemini-cli:" + path,
	}, true, nil
}

// harvestGeminiCLISessions loads every Gemini CLI session log,
// filters+caps via selectHarvestSessions, and runs the shared LLM
// distillation path over the survivors.
func (b *Brain) harvestGeminiCLISessions(ctx context.Context, geminiHome string, distill DistillFunc, opts HarvestOptions) (*HarvestResult, error) {
	files, err := listGeminiCLISessionFiles(geminiHome)
	if err != nil {
		return nil, fmt.Errorf("list gemini cli sessions: %w", err)
	}

	var all []HarvestSession
	for _, path := range files {
		sess, ok, err := readGeminiCLISession(path)
		if err != nil {
			ui.PrintVerbose("Harvest", "gemini cli session: "+err.Error())
			continue
		}
		if !ok {
			continue
		}
		all = append(all, sess)
	}

	wm := readHarvestWatermark(b.Path, "gemini")
	selected, skippedQuiet := selectHarvestSessions(all, wm, opts)

	result, err := b.harvestDistilledSessions(ctx, "gemini", selected, distill, opts.DryRun)
	if err != nil {
		return nil, err
	}
	result.CandidatesTotal = len(all)
	result.SkippedQuiet = skippedQuiet
	return result, nil
}

// geminiMDFile is one *.md artifact found under an Antigravity
// conversation's brain directory.
type geminiMDFile struct {
	ConvID  string
	RelPath string
	AbsPath string
	Mtime   time.Time
}

// listGeminiAntigravityMarkdown walks
// ~/.gemini/antigravity-cli/brain/<conversation-id>/** for *.md files.
// Missing brain/ dir is not an error -- Antigravity may not be
// installed/used.
func listGeminiAntigravityMarkdown(geminiHome string) ([]geminiMDFile, error) {
	root := filepath.Join(geminiHome, "antigravity-cli", "brain")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var out []geminiMDFile
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		convID := e.Name()
		convDir := filepath.Join(root, convID)
		_ = filepath.WalkDir(convDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if strings.ToLower(filepath.Ext(d.Name())) != ".md" {
				return nil // non-md artifacts (png/jpg/gif/pb/...) always skipped
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			rel, err := filepath.Rel(convDir, path)
			if err != nil {
				rel = d.Name()
			}
			out = append(out, geminiMDFile{ConvID: convID, RelPath: rel, AbsPath: path, Mtime: info.ModTime()})
			return nil
		})
	}
	return out, nil
}

// loadGeminiAntigravityScopes reads conversation_id -> project scope
// from conversation_summaries.db's workspace_uris column (a JSON array
// of file:// URIs; the first entry wins). A missing/unopenable/
// unqueryable database degrades to "no scope known" rather than an
// error -- every conversation just falls back to global scope.
func loadGeminiAntigravityScopes(dbPath string) map[string]string {
	scopes := map[string]string{}
	if _, err := os.Stat(dbPath); err != nil {
		return scopes
	}
	con, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		ui.PrintVerbose("Harvest", "open antigravity db failed: "+err.Error())
		return scopes
	}
	defer con.Close()

	rows, err := con.Query(`SELECT conversation_id, workspace_uris FROM conversation_summaries`)
	if err != nil {
		ui.PrintVerbose("Harvest", "query antigravity db failed: "+err.Error())
		return scopes
	}
	defer rows.Close()

	for rows.Next() {
		var id, uris string
		if rows.Scan(&id, &uris) != nil {
			continue
		}
		var list []string
		if json.Unmarshal([]byte(uris), &list) != nil || len(list) == 0 {
			continue
		}
		p := strings.TrimPrefix(list[0], "file://")
		if p == "" {
			continue
		}
		scopes[id] = "project:" + cwdToProjectSlug(p)
	}
	return scopes
}

// harvestGeminiAntigravityItems builds one HarvestDirectMemory per
// Antigravity *.md artifact, ready for harvestDirectMemories.
func harvestGeminiAntigravityItems(geminiHome string) ([]HarvestDirectMemory, error) {
	files, err := listGeminiAntigravityMarkdown(geminiHome)
	if err != nil {
		return nil, fmt.Errorf("list antigravity markdown: %w", err)
	}
	if len(files) == 0 {
		return nil, nil
	}

	dbPath := filepath.Join(geminiHome, "antigravity-cli", "conversation_summaries.db")
	scopes := loadGeminiAntigravityScopes(dbPath)

	var items []HarvestDirectMemory
	for _, f := range files {
		data, err := os.ReadFile(f.AbsPath)
		if err != nil {
			continue
		}
		body := strings.TrimSpace(string(data))
		if body == "" {
			continue
		}
		name := harvestKebab(strings.TrimSuffix(filepath.Base(f.RelPath), filepath.Ext(f.RelPath)))
		if name == "" {
			continue
		}
		scope := scopes[f.ConvID]
		if scope == "" {
			scope = "global"
		}
		items = append(items, HarvestDirectMemory{
			ID:        "antigravity:" + f.ConvID + ":" + f.RelPath,
			UpdatedAt: f.Mtime,
			Type:      MemoryEvent,
			Key:       "gemini:antigravity:" + f.ConvID + ":" + name,
			Body:      body,
			Scope:     scope,
			Tags:      []string{"gemini-antigravity", "conversation:" + f.ConvID},
			Source:    "gemini-antigravity:" + f.AbsPath,
		})
	}
	return items, nil
}

// harvestGeminiMDItem builds the single HarvestDirectMemory mirroring
// ~/.gemini/GEMINI.md, when present -- the Gemini analogue of Claude
// Code's global CLAUDE.md capture. Returns (nil, nil) when the file
// doesn't exist or is empty.
func harvestGeminiMDItem(geminiHome string) (*HarvestDirectMemory, error) {
	path := filepath.Join(geminiHome, "GEMINI.md")
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	body := strings.TrimSpace(string(data))
	if body == "" {
		return nil, nil
	}
	return &HarvestDirectMemory{
		ID:        "gemini-md",
		UpdatedAt: info.ModTime(),
		Type:      MemoryInstruction,
		Key:       "gemini:global:GEMINI",
		Body:      body,
		Scope:     "global",
		Tags:      []string{"gemini-md"},
		Source:    "gemini:" + path,
	}, nil
}

// HarvestGemini distills Gemini CLI sessions, mirrors Antigravity
// walkthrough/plan artifacts, and mirrors GEMINI.md, all under the
// "gemini" profile. distill is typically client.CompleteJSON.
func (b *Brain) HarvestGemini(ctx context.Context, distill DistillFunc, opts HarvestOptions) (*HarvestResult, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home dir: %w", err)
	}
	return b.harvestGemini(ctx, filepath.Join(home, ".gemini"), distill, opts)
}

// harvestGemini is the injectable core: geminiHome is overridable so
// tests point at a fixture tree instead of the real ~/.gemini.
func (b *Brain) harvestGemini(ctx context.Context, geminiHome string, distill DistillFunc, opts HarvestOptions) (*HarvestResult, error) {
	result, err := b.harvestGeminiCLISessions(ctx, geminiHome, distill, opts)
	if err != nil {
		return nil, err
	}

	var direct []HarvestDirectMemory
	if agItems, err := harvestGeminiAntigravityItems(geminiHome); err != nil {
		ui.PrintVerbose("Harvest", "antigravity scan failed: "+err.Error())
	} else {
		direct = append(direct, agItems...)
	}
	if mdItem, err := harvestGeminiMDItem(geminiHome); err != nil {
		ui.PrintVerbose("Harvest", "GEMINI.md read failed: "+err.Error())
	} else if mdItem != nil {
		direct = append(direct, *mdItem)
	}

	written, err := b.harvestDirectMemories(ctx, "gemini", direct, opts)
	if err != nil {
		return nil, err
	}
	result.DirectMirrored = written

	return result, nil
}
