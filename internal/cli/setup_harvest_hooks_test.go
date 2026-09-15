package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// codexHooksFixture mirrors the real ~/.codex/hooks.json shape on a
// machine running supacode: SessionStart/Stop/UserPromptSubmit each
// carry supacode-managed hook-groups (a notify.sh command plus an awk
// one-liner with heavily escaped quotes) that a merge must never lose
// or corrupt.
const codexHooksFixture = `{
  "hooks": {
    "SessionStart": [
      { "hooks": [ { "command": "/Users/fakehome/.superset/hooks/notify.sh", "type": "command" } ] }
    ],
    "Stop": [
      { "hooks": [ { "command": "/Users/fakehome/.superset/hooks/notify.sh", "type": "command" } ] },
      { "hooks": [ { "command": "[ -n \"${SUPACODE_SURFACE_ID:-}\" ] && printf 'x=\\\"y\\\"' # supacode-managed-hook", "timeout": 10, "type": "command" } ] }
    ],
    "UserPromptSubmit": [
      { "hooks": [ { "command": "/Users/fakehome/.superset/hooks/notify.sh", "type": "command" } ] }
    ]
  }
}`

// geminiSettingsFixture mirrors the real ~/.gemini/settings.json shape:
// non-hook top-level keys (security, general) plus BeforeAgent/
// AfterAgent/AfterTool hook events, all pointing at a supacode script.
const geminiSettingsFixture = `{
  "security": {
    "auth": {
      "selectedType": "oauth-personal"
    }
  },
  "general": {
    "previewFeatures": true
  },
  "hooks": {
    "BeforeAgent": [
      { "hooks": [ { "type": "command", "command": "/Users/fakehome/.superset/hooks/gemini-hook.sh" } ] }
    ],
    "AfterAgent": [
      { "hooks": [ { "type": "command", "command": "/Users/fakehome/.superset/hooks/gemini-hook.sh" } ] }
    ],
    "AfterTool": [
      { "hooks": [ { "type": "command", "command": "/Users/fakehome/.superset/hooks/gemini-hook.sh" } ] }
    ]
  }
}`

func TestMergeAdditiveHook_CodexAppendsToStopPreservesEverythingElse(t *testing.T) {
	out, installed, err := mergeAdditiveHook([]byte(codexHooksFixture), "Stop", "harvest-codex.sh", commandHookGroup("/x/harvest-codex.sh"))
	if err != nil {
		t.Fatalf("mergeAdditiveHook: %v", err)
	}
	if !installed {
		t.Fatal("expected installed=true on first merge")
	}

	var got struct {
		Hooks map[string][]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal merged output: %v", err)
	}

	if len(got.Hooks["Stop"]) != 3 {
		t.Fatalf("Stop hooks = %d, want 3 (2 existing + 1 new)", len(got.Hooks["Stop"]))
	}
	if len(got.Hooks["SessionStart"]) != 1 {
		t.Errorf("SessionStart should be untouched, got %d entries", len(got.Hooks["SessionStart"]))
	}
	if len(got.Hooks["UserPromptSubmit"]) != 1 {
		t.Errorf("UserPromptSubmit should be untouched, got %d entries", len(got.Hooks["UserPromptSubmit"]))
	}

	// The supacode notify.sh entry and the gnarly awk one-liner must
	// both survive byte-for-byte in substance (still contain their
	// original command strings after the round trip).
	full := string(out)
	if !strings.Contains(full, "/Users/fakehome/.superset/hooks/notify.sh") {
		t.Error("supacode notify.sh command was lost in the merge")
	}
	if !strings.Contains(full, "supacode-managed-hook") {
		t.Error("supacode-managed-hook awk one-liner was lost in the merge")
	}
	if !strings.Contains(full, "/x/harvest-codex.sh") {
		t.Error("new harvest hook command missing from merged output")
	}
}

func TestMergeAdditiveHook_Idempotent(t *testing.T) {
	first, installed1, err := mergeAdditiveHook([]byte(codexHooksFixture), "Stop", "harvest-codex.sh", commandHookGroup("/x/harvest-codex.sh"))
	if err != nil || !installed1 {
		t.Fatalf("first merge: installed=%v err=%v", installed1, err)
	}

	second, installed2, err := mergeAdditiveHook(first, "Stop", "harvest-codex.sh", commandHookGroup("/x/harvest-codex.sh"))
	if err != nil {
		t.Fatalf("second merge: %v", err)
	}
	if installed2 {
		t.Error("second merge should be a no-op (already wired), got installed=true")
	}
	if string(second) != string(first) {
		t.Error("second merge should return the input unchanged")
	}
}

func TestMergeAdditiveHook_GeminiAppendsToAfterAgentPreservesNonHookKeys(t *testing.T) {
	out, installed, err := mergeAdditiveHook([]byte(geminiSettingsFixture), "AfterAgent", "harvest-gemini.sh", commandHookGroup("/x/harvest-gemini.sh"))
	if err != nil {
		t.Fatalf("mergeAdditiveHook: %v", err)
	}
	if !installed {
		t.Fatal("expected installed=true on first merge")
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal merged output: %v", err)
	}
	if _, ok := got["security"]; !ok {
		t.Error("top-level \"security\" key was dropped by the merge")
	}
	if _, ok := got["general"]; !ok {
		t.Error("top-level \"general\" key was dropped by the merge")
	}
	if !strings.Contains(string(got["security"]), "oauth-personal") {
		t.Error("security block content was altered by the merge")
	}

	var hooks struct {
		BeforeAgent []json.RawMessage `json:"BeforeAgent"`
		AfterAgent  []json.RawMessage `json:"AfterAgent"`
		AfterTool   []json.RawMessage `json:"AfterTool"`
	}
	if err := json.Unmarshal(got["hooks"], &hooks); err != nil {
		t.Fatalf("unmarshal hooks: %v", err)
	}
	if len(hooks.BeforeAgent) != 1 {
		t.Errorf("BeforeAgent should be untouched, got %d entries", len(hooks.BeforeAgent))
	}
	if len(hooks.AfterTool) != 1 {
		t.Errorf("AfterTool should be untouched, got %d entries", len(hooks.AfterTool))
	}
	if len(hooks.AfterAgent) != 2 {
		t.Fatalf("AfterAgent = %d, want 2 (1 existing + 1 new)", len(hooks.AfterAgent))
	}
}

func TestMergeAdditiveHook_EmptyInputStartsFresh(t *testing.T) {
	out, installed, err := mergeAdditiveHook(nil, "Stop", "harvest-codex.sh", commandHookGroup("/x/harvest-codex.sh"))
	if err != nil {
		t.Fatalf("mergeAdditiveHook: %v", err)
	}
	if !installed {
		t.Error("expected installed=true for a fresh/empty file")
	}
	var got struct {
		Hooks map[string][]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Hooks["Stop"]) != 1 {
		t.Errorf("Stop hooks = %d, want 1", len(got.Hooks["Stop"]))
	}
}

func TestInstallHookScript(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "hooks", "harvest-codex.sh")

	installed, err := installHookScript(dest, "#!/bin/sh\necho hi\n")
	if err != nil {
		t.Fatalf("installHookScript: %v", err)
	}
	if !installed {
		t.Error("expected installed=true on first write")
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0755 {
		t.Errorf("mode = %o, want 0755", info.Mode().Perm())
	}

	installed2, err := installHookScript(dest, "#!/bin/sh\necho hi\n")
	if err != nil {
		t.Fatalf("installHookScript (2nd): %v", err)
	}
	if installed2 {
		t.Error("second install of identical content should be a no-op")
	}
}

// TestWireCodexHarvestHook_FullFlow exercises the whole install: the
// script lands on disk and ~/.codex/hooks.json gets the new Stop
// hook-group merged in, on top of a realistic pre-existing fixture.
func TestWireCodexHarvestHook_FullFlow(t *testing.T) {
	home := t.TempDir()
	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	hooksPath := filepath.Join(codexDir, "hooks.json")
	if err := os.WriteFile(hooksPath, []byte(codexHooksFixture), 0644); err != nil {
		t.Fatalf("seed hooks.json: %v", err)
	}

	if err := wireCodexHarvestHook(home); err != nil {
		t.Fatalf("wireCodexHarvestHook: %v", err)
	}

	scriptPath := filepath.Join(home, ".aida", "hooks", "harvest-codex.sh")
	if _, err := os.Stat(scriptPath); err != nil {
		t.Fatalf("expected script at %s: %v", scriptPath, err)
	}

	data, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatalf("read hooks.json: %v", err)
	}
	if !strings.Contains(string(data), "harvest-codex.sh") {
		t.Error("hooks.json does not reference the harvest script")
	}
	if !strings.Contains(string(data), "supacode-managed-hook") {
		t.Error("hooks.json lost the pre-existing supacode hook")
	}

	// Idempotent: running again does not duplicate the entry.
	if err := wireCodexHarvestHook(home); err != nil {
		t.Fatalf("wireCodexHarvestHook (2nd): %v", err)
	}
	data2, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatalf("read hooks.json (2nd): %v", err)
	}
	if strings.Count(string(data2), "harvest-codex.sh") != strings.Count(string(data), "harvest-codex.sh") {
		t.Error("second wireCodexHarvestHook call duplicated the hook entry")
	}
}

// TestWireCodexHarvestHook_MissingCodexIsNoOp verifies a machine
// without Codex installed doesn't error and doesn't create ~/.codex.
func TestWireCodexHarvestHook_MissingCodexIsNoOp(t *testing.T) {
	home := t.TempDir()
	if err := wireCodexHarvestHook(home); err != nil {
		t.Fatalf("expected no error when ~/.codex is absent, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex")); err == nil {
		t.Error("wireCodexHarvestHook should not create ~/.codex")
	}
}

// TestWireGeminiHarvestHook_FullFlow mirrors the Codex test for Gemini,
// including the non-hook security/general keys.
func TestWireGeminiHarvestHook_FullFlow(t *testing.T) {
	home := t.TempDir()
	geminiDir := filepath.Join(home, ".gemini")
	if err := os.MkdirAll(geminiDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	settingsPath := filepath.Join(geminiDir, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(geminiSettingsFixture), 0644); err != nil {
		t.Fatalf("seed settings.json: %v", err)
	}

	if err := wireGeminiHarvestHook(home); err != nil {
		t.Fatalf("wireGeminiHarvestHook: %v", err)
	}

	scriptPath := filepath.Join(home, ".aida", "hooks", "harvest-gemini.sh")
	if _, err := os.Stat(scriptPath); err != nil {
		t.Fatalf("expected script at %s: %v", scriptPath, err)
	}

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	if !strings.Contains(string(data), "harvest-gemini.sh") {
		t.Error("settings.json does not reference the harvest script")
	}
	if !strings.Contains(string(data), "oauth-personal") {
		t.Error("settings.json lost the pre-existing security block")
	}
	if !strings.Contains(string(data), "/Users/fakehome/.superset/hooks/gemini-hook.sh") {
		t.Error("settings.json lost the pre-existing supacode gemini-hook.sh entries")
	}

	if err := wireGeminiHarvestHook(home); err != nil {
		t.Fatalf("wireGeminiHarvestHook (2nd): %v", err)
	}
	data2, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read settings.json (2nd): %v", err)
	}
	if strings.Count(string(data2), "harvest-gemini.sh") != strings.Count(string(data), "harvest-gemini.sh") {
		t.Error("second wireGeminiHarvestHook call duplicated the hook entry")
	}
}

func TestWireGeminiHarvestHook_MissingGeminiIsNoOp(t *testing.T) {
	home := t.TempDir()
	if err := wireGeminiHarvestHook(home); err != nil {
		t.Fatalf("expected no error when ~/.gemini is absent, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".gemini")); err == nil {
		t.Error("wireGeminiHarvestHook should not create ~/.gemini")
	}
}

func TestHarvestHookScriptEmbeds(t *testing.T) {
	if !strings.HasPrefix(harvestCodexHookScript, "#!/usr/bin/env bash") {
		t.Error("harvestCodexHookScript missing bash shebang")
	}
	if !strings.Contains(harvestCodexHookScript, "aida brain harvest --tool codex") {
		t.Error("harvestCodexHookScript does not invoke `aida brain harvest --tool codex`")
	}
	if !strings.HasPrefix(harvestGeminiHookScript, "#!/usr/bin/env bash") {
		t.Error("harvestGeminiHookScript missing bash shebang")
	}
	if !strings.Contains(harvestGeminiHookScript, "aida brain harvest --tool gemini") {
		t.Error("harvestGeminiHookScript does not invoke `aida brain harvest --tool gemini`")
	}
}
