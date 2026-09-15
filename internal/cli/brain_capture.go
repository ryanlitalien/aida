package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/ryanlitalien/aida/internal/brain"
	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/ui"
)

// claudeMemoryProfile pins every captured Claude Code memory record to a
// fixed, machine-independent profile -- distinct from Aida's own
// work/home profile axis. brain.WriteMemory only defaults Profile when
// it's empty, so setting it here sticks and keeps supersession stable
// across machines instead of forking per Aida profile.
const claudeMemoryProfile = "claude"

// newBrainCaptureHookCmd builds `aida brain capture-hook` -- the
// PostToolUse(Write|Edit) hook entry point. It reads the hook's JSON
// payload from stdin, extracts the written file path, and mirrors it
// into the brain via the shared runCapture core. It must never fail or
// block the tool call that triggered it, so the RunE stays a thin
// wrapper that always returns nil; every failure is logged to stderr
// only.
func newBrainCaptureHookCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "capture-hook",
		Short: "Capture a Claude Code memory write from a PostToolUse hook (reads stdin)",
		Long: "Reads a PostToolUse hook JSON payload from stdin, extracts the\n" +
			"written file's path, and mirrors it into the brain as a typed\n" +
			"memory record. Wired into ~/.claude/settings.json as a detached\n" +
			"PostToolUse(Write|Edit) hook -- always exits 0 so a broken brain\n" +
			"degrades to \"no capture\", never a blocked write.",
		RunE: func(cmd *cobra.Command, args []string) error {
			runBrainCaptureHook(cmd.Context())
			return nil
		},
	}
}

// hookInput is the subset of the Claude Code PostToolUse payload this
// command needs: the path of the file the tool just wrote.
type hookInput struct {
	ToolInput struct {
		FilePath string `json:"file_path"`
	} `json:"tool_input"`
}

// runBrainCaptureHook is capture-hook's body. It deliberately never
// surfaces an error to its caller -- every failure path logs to
// stderr and returns, so the hook script (and the Write/Edit tool call
// that spawned it) is never blocked or failed by a brain problem.
func runBrainCaptureHook(ctx context.Context) {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "brain capture-hook: read stdin: %v\n", err)
		return
	}

	var in hookInput
	if err := json.Unmarshal(data, &in); err != nil {
		fmt.Fprintf(os.Stderr, "brain capture-hook: parse stdin: %v\n", err)
		return
	}
	if in.ToolInput.FilePath == "" {
		return
	}

	cfg, err := config.LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "brain capture-hook: load config: %v\n", err)
		return
	}
	_, profileName := cfg.ActiveProfileConfig()
	b, err := brain.Open(cfg.BrainPath(), profileName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
	if err != nil {
		fmt.Fprintf(os.Stderr, "brain capture-hook: open brain: %v\n", err)
		return
	}
	defer b.Close()

	if err := runCapture(ctx, b, in.ToolInput.FilePath, "", ""); err != nil {
		fmt.Fprintf(os.Stderr, "brain capture-hook: %v\n", err)
	}
}

// newBrainRememberCmd builds `aida brain remember` -- the manual/backfill
// entry point for the Claude Code memory bridge. Unlike capture-hook,
// this command is allowed to return real errors: it's a human (or
// script) deliberately asking for a specific file to be captured, not a
// fire-and-forget hook.
func newBrainRememberCmd() *cobra.Command {
	var (
		file      string
		scopeFlag string
		typeFlag  string
	)
	cmd := &cobra.Command{
		Use:   "remember",
		Short: "Capture (or backfill) a Claude Code memory file into the brain",
		Long: "Manual/backfill entry point for the Claude Code memory bridge: reads\n" +
			"the given memory file, classifies its scope from the path (override\n" +
			"with --scope), maps its frontmatter type to a brain memory type\n" +
			"(override with --type), and writes it as a typed, embedded memory\n" +
			"record.\n\n" +
			"Re-running on the same file supersedes the prior record for that key\n" +
			"instead of duplicating it, so it's safe to call repeatedly -- this is\n" +
			"what `aida brain capture-hook` does under the hood on every\n" +
			"Write/Edit of a Claude memory file.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(file) == "" {
				return fmt.Errorf("--file is required")
			}
			cfg, err := config.LoadConfig()
			if err != nil {
				return err
			}
			_, profileName := cfg.ActiveProfileConfig()

			b, err := brain.Open(cfg.BrainPath(), profileName, cfg.VoyageKeyEnv(), cfg.Brain.GitHubRepo())
			if err != nil {
				return err
			}
			defer b.Close()

			return runCapture(cmd.Context(), b, file, scopeFlag, typeFlag)
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "path to the Claude Code memory file to capture (required)")
	cmd.Flags().StringVar(&scopeFlag, "scope", "", "override the derived scope: global or project:<key>")
	cmd.Flags().StringVar(&typeFlag, "type", "", "override the derived memory type: fact, instruction, or event")
	return cmd
}

// memoryPathInfo is the result of classifying a Claude Code memory file
// path -- everything runCapture needs to know before it even reads the
// file's content.
type memoryPathInfo struct {
	scope    string // "global" or "project"
	projKey  string // set only when scope == "project"
	name     string // basename with any trailing ".md" stripped
	isCLAUDE bool   // true only for HOME/.claude/CLAUDE.md
	ok       bool   // false means "not a Claude memory path -- no-op"
}

// classifyMemoryPath determines scope, project key, and memory name from
// a file path alone -- cwd is not needed, the project identity already
// lives in the path for project-scoped memory files. Deliberately a pure
// function of (path, home) so it's unit-testable in isolation.
//
// Recognized shapes:
//   - HOME/.claude/CLAUDE.md                              -> global, isCLAUDE
//   - HOME/.claude/memory/<name>.md (includes MEMORY.md)  -> global
//   - HOME/.claude/projects/<slug>/memory/<name>.md        -> project, projKey=<slug>
//   - anything else                                       -> ok=false (no-op)
func classifyMemoryPath(path, home string) memoryPathInfo {
	path = filepath.Clean(path)

	claudeMD := filepath.Join(home, ".claude", "CLAUDE.md")
	if path == claudeMD {
		return memoryPathInfo{scope: "global", name: "CLAUDE", isCLAUDE: true, ok: true}
	}

	if !strings.HasSuffix(path, ".md") {
		return memoryPathInfo{}
	}

	globalMemDir := filepath.Join(home, ".claude", "memory") + string(filepath.Separator)
	if strings.HasPrefix(path, globalMemDir) {
		name := strings.TrimSuffix(filepath.Base(path), ".md")
		return memoryPathInfo{scope: "global", name: name, ok: true}
	}

	projectsDir := filepath.Join(home, ".claude", "projects") + string(filepath.Separator)
	if strings.HasPrefix(path, projectsDir) {
		rest := strings.TrimPrefix(path, projectsDir)
		parts := strings.Split(rest, string(filepath.Separator))
		// Expect exactly <slug>/memory/<name>.md -- the single path
		// segment right after "projects/" is the slug, used verbatim
		// since it's unique and stable per Claude Code project.
		if len(parts) == 3 && parts[1] == "memory" {
			return memoryPathInfo{
				scope:   "project",
				projKey: parts[0],
				name:    strings.TrimSuffix(parts[2], ".md"),
				ok:      true,
			}
		}
	}

	return memoryPathInfo{}
}

// parseScopeOverride parses a --scope value of "global" or
// "project:<key>" into (scope, projKey). Anything else is an error --
// unlike the path classifier, an explicit override that doesn't parse is
// a caller mistake worth surfacing, not a silent no-op.
func parseScopeOverride(s string) (scope, projKey string, err error) {
	if s == "global" {
		return "global", "", nil
	}
	if rest, ok := strings.CutPrefix(s, "project:"); ok && rest != "" {
		return "project", rest, nil
	}
	return "", "", fmt.Errorf("invalid --scope %q: must be \"global\" or \"project:<key>\"", s)
}

// memFrontmatter is the subset of Claude Code memory-file frontmatter
// this command reads. Global memory files use a top-level `type:`;
// project-scoped files nest it under `metadata.type:` -- both dialects
// are supported.
type memFrontmatter struct {
	Type        string `yaml:"type"`
	Description string `yaml:"description"`
	Metadata    struct {
		Type string `yaml:"type"`
	} `yaml:"metadata"`
}

const (
	soulBlockBegin = "<!-- BEGIN aida-soul -->"
	soulBlockEnd   = "<!-- END aida-soul -->"
)

// stripSoulBlock removes a managed soul-projection block from s, if
// present: everything from the line containing soulBlockBegin through
// the line containing soulBlockEnd, inclusive. Defensive no-op when the
// markers aren't both present -- Phase 2 is what writes them into
// CLAUDE.md, this just needs to not choke before that lands.
func stripSoulBlock(s string) string {
	lines := strings.Split(s, "\n")
	start := -1
	end := -1
	for i, line := range lines {
		if start == -1 && strings.Contains(line, soulBlockBegin) {
			start = i
			continue
		}
		if start != -1 && strings.Contains(line, soulBlockEnd) {
			end = i
			break
		}
	}
	if start == -1 || end == -1 {
		return s
	}
	out := make([]string, 0, len(lines)-(end-start+1))
	out = append(out, lines[:start]...)
	out = append(out, lines[end+1:]...)
	return strings.Join(out, "\n")
}

// mapMemoryType picks the brain memory type for a captured record.
// typeOverride wins when set and must be one of fact/instruction/event;
// otherwise sourceType (the file's frontmatter type, either dialect) is
// mapped per the Claude Code memory bridge's rule: directly-authored
// memories enter as instruction/fact, never as an event needing
// consolidation.
func mapMemoryType(sourceType, typeOverride string) (brain.MemoryType, error) {
	if typeOverride != "" {
		mt := brain.MemoryType(typeOverride)
		if !brain.IsValidMemoryType(mt) {
			return "", fmt.Errorf("invalid type override %q: must be fact, instruction, or event", typeOverride)
		}
		return mt, nil
	}
	switch sourceType {
	case "user", "feedback":
		return brain.MemoryInstruction, nil
	case "project", "reference":
		return brain.MemoryFact, nil
	case "session":
		return brain.MemoryEvent, nil
	default:
		return brain.MemoryFact, nil
	}
}

// runCapture is the shared core behind both `capture-hook` and
// `remember`: classify the path, read + parse the file, map it to a
// brain memory type, and write (or supersede) the record. scopeOverride
// and typeOverride are empty for capture-hook's automatic path; remember
// passes through whatever the user asked for.
func runCapture(ctx context.Context, b *brain.Brain, path, scopeOverride, typeOverride string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home dir: %w", err)
	}

	info := classifyMemoryPath(path, home)
	if scopeOverride != "" {
		scope, projKey, err := parseScopeOverride(scopeOverride)
		if err != nil {
			return err
		}
		info.scope = scope
		info.projKey = projKey
		info.ok = true
		if info.name == "" {
			info.name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		}
	}
	if !info.ok {
		return nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	// Derive Created from the file's real last-write time, not the moment of
	// capture. Claude memory files carry no authoring date in frontmatter, so
	// without this every backfilled record collapses onto the capture instant
	// and recency recall can't tell a months-old note from a fresh one. A stat
	// failure leaves created empty, falling back to WriteMemory's default (now).
	created := ""
	if fi, statErr := os.Stat(path); statErr == nil {
		created = fi.ModTime().UTC().Format(time.RFC3339)
	}

	fm, body, hasFM := splitFrontmatter(data)
	sourceType := ""
	description := ""
	if hasFM {
		var parsed memFrontmatter
		if err := yaml.Unmarshal([]byte(fm), &parsed); err != nil {
			ui.PrintVerbose("brain remember", fmt.Sprintf("unparseable frontmatter in %s, capturing body only: %v", path, err))
		} else {
			sourceType = parsed.Type
			if parsed.Metadata.Type != "" {
				sourceType = parsed.Metadata.Type
			}
			description = parsed.Description
		}
	} else {
		body = string(data)
	}

	if info.isCLAUDE {
		body = stripSoulBlock(body)
	}

	finalBody := strings.TrimSpace(body)
	if description != "" {
		finalBody = description + "\n\n" + finalBody
	}
	if finalBody == "" {
		return nil
	}

	memType, err := mapMemoryType(sourceType, typeOverride)
	if err != nil {
		return err
	}

	scopeTag := "scope:" + info.scope
	var key string
	tags := []string{"claude-code", scopeTag}
	if info.scope == "project" {
		key = "claude:project:" + info.projKey + ":" + info.name
		tags = append(tags, "project:"+info.projKey)
	} else {
		key = "claude:global:" + info.name
	}
	source := "claude-code:" + path

	existing, err := b.DB.ActiveMemoryByKey(memType, key, claudeMemoryProfile)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("lookup existing memory: %w", err)
	}
	if err == nil && existing.Body == finalBody {
		// Body unchanged. Still reconcile the timestamp: an early backfill
		// stamped capture-time, so correct it to the file's mtime in place
		// (no supersede, no churn) rather than skipping outright.
		if created != "" && existing.Created != created {
			if dryRunGuard("retime "+key, "created: "+existing.Created+" -> "+created) {
				return nil
			}
			prior, err := b.RetimeMemory(existing.ID, created)
			if err != nil {
				return fmt.Errorf("retime memory: %w", err)
			}
			fmt.Printf("retimed %s %s (%s -> %s)\n", memType, key, prior, created)
			return nil
		}
		fmt.Printf("%s %s unchanged, skipped\n", memType, key)
		return nil
	}

	if dryRunGuard("brain remember", path) {
		fmt.Printf("  type:    %s\n", memType)
		fmt.Printf("  key:     %s\n", key)
		fmt.Printf("  tags:    %s\n", strings.Join(tags, ", "))
		fmt.Printf("  profile: %s\n", claudeMemoryProfile)
		fmt.Printf("  created: %s\n", created)
		fmt.Printf("  body:    %s\n", truncate(finalBody, 200))
		return nil
	}

	rec, err := b.WriteMemory(ctx, brain.MemoryRecord{
		Type:       memType,
		Key:        key,
		Body:       finalBody,
		Tags:       tags,
		Source:     source,
		Profile:    claudeMemoryProfile,
		Confidence: 1.0,
		Created:    created,
	})
	if err != nil {
		return fmt.Errorf("write memory: %w", err)
	}

	// Cross-type supersede: the frontmatter type can change across
	// edits (e.g. a memory reclassified from "reference" to "user"),
	// which would otherwise leave two active records -- one fact, one
	// instruction -- under the same key. Fold the other types' active
	// record into this one so a key never has two active records of
	// different types.
	for _, other := range []brain.MemoryType{brain.MemoryFact, brain.MemoryEvent, brain.MemoryInstruction} {
		if other == memType {
			continue
		}
		found, err := b.DB.ActiveMemoryByKey(other, key, claudeMemoryProfile)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				ui.PrintVerbose("brain remember", "cross-type lookup failed: "+err.Error())
			}
			continue
		}
		if err := b.DB.SupersedeByID(found.ID, rec.ID); err != nil {
			ui.PrintVerbose("brain remember", "cross-type supersede failed: "+err.Error())
		}
	}

	fmt.Printf("captured %s %s\n", memType, key)
	return nil
}
