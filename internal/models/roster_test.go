package models

import (
	"os"
	"path/filepath"
	"testing"
)

const testRosterYAML = `
updated: 2026-09-08
guidance:
  - "2026-09-08 coding agents: Sonnet for coding, Codex for judgement calls."
  - "Fable weekly cap trips first."
providers:
  - name: anthropic
    label: Anthropic / Claude
    plan: Claude Max 20x
    account: user@example.com
    probe: claude-oauth
    models:
      - id: claude-fable-5-1
        nicknames: [fable, fable 5.1, mythos]
        tier: frontier
        note: judgement, design, audits.
      - id: claude-sonnet-5
        nicknames: [sonnet, sonnet 5]
        tier: workhorse

  - name: openai
    label: OpenAI / ChatGPT + Codex
    plan: ChatGPT Plus
    probe: codex-sessions
    models:
      - id: gpt-5.6-sol
        nicknames: [sol, gpt-sol, gpt sol, gpt-5.6-sol medium]
        tier: workhorse
      - id: gpt-6-astra
        nicknames: [astra, gpt-6, gpt astra]
        tier: frontier

  - name: google
    label: Google / Gemini
    plan: Google AI Pro
    probe: gemini-local
    models:
      - id: gemini-3.1-pro
        nicknames: [3.1 pro, gemini pro, pro]
        tier: frontier

  - name: litellm
    label: LiteLLM proxy (minty)
    plan: budget keys
    probe: litellm
    litellm:
      url: http://100.64.0.1:4000
      lan_url: http://192.168.40.99:4000
      key_env: LITELLM_MASTER_KEY
      ssh_host: minty
      ssh_env_file: /mnt/storage_apps/litellm/.env
    models:
      - id: claude-sonnet-5
        nicknames: [sonnet]
    retired:
      - claude-sonnet-4-6
      - claude-opus-4-6
      - claude-haiku-4-5
`

func loadTestRoster(t *testing.T) *Roster {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.yaml")
	if err := os.WriteFile(path, []byte(testRosterYAML), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	r, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return r
}

func TestLoad(t *testing.T) {
	r := loadTestRoster(t)
	if r.Updated != "2026-09-08" {
		t.Errorf("Updated = %q, want 2026-09-08", r.Updated)
	}
	if len(r.Guidance) != 2 {
		t.Fatalf("Guidance = %v, want 2 entries", r.Guidance)
	}
	if len(r.Providers) != 4 {
		t.Fatalf("Providers = %d, want 4", len(r.Providers))
	}

	var litellm *Provider
	for i := range r.Providers {
		if r.Providers[i].Name == "litellm" {
			litellm = &r.Providers[i]
		}
	}
	if litellm == nil {
		t.Fatal("litellm provider not found")
	}
	if litellm.LiteLLM == nil {
		t.Fatal("litellm.LiteLLM is nil")
	}
	if litellm.LiteLLM.URL != "http://100.64.0.1:4000" {
		t.Errorf("litellm URL = %q", litellm.LiteLLM.URL)
	}
	if litellm.LiteLLM.SSHHost != "minty" {
		t.Errorf("litellm SSHHost = %q, want minty", litellm.LiteLLM.SSHHost)
	}
}

func TestLoad_RetiredModels(t *testing.T) {
	r := loadTestRoster(t)
	var litellm *Provider
	for i := range r.Providers {
		if r.Providers[i].Name == "litellm" {
			litellm = &r.Providers[i]
		}
	}
	if litellm == nil {
		t.Fatal("litellm provider not found")
	}
	wantRetired := []string{"claude-sonnet-4-6", "claude-opus-4-6", "claude-haiku-4-5"}
	if len(litellm.Retired) != len(wantRetired) {
		t.Fatalf("Retired = %v, want %v", litellm.Retired, wantRetired)
	}
	for i, id := range wantRetired {
		if litellm.Retired[i] != id {
			t.Errorf("Retired[%d] = %q, want %q", i, litellm.Retired[i], id)
		}
	}

	// A retired id is known for the unlisted-model diff...
	known := litellm.KnownModelIDs()
	for _, id := range wantRetired {
		if !known[id] {
			t.Errorf("KnownModelIDs()[%q] = false, want true (retired ids count as known)", id)
		}
	}
	if !known["claude-sonnet-5"] {
		t.Error(`KnownModelIDs()["claude-sonnet-5"] = false, want true (active model)`)
	}

	// ...but never resolves by nickname or id, and never shows up in Models.
	for _, id := range wantRetired {
		if matches := r.Resolve(id); len(matches) != 0 {
			t.Errorf("Resolve(%q) = %v, want no matches (retired model)", id, matches)
		}
	}
	for _, m := range litellm.Models {
		for _, id := range wantRetired {
			if m.ID == id {
				t.Errorf("Models contains retired id %q, want it absent", id)
			}
		}
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("expected an error for a missing roster file, got nil")
	}
}

func TestLoad_ClaudeConfig(t *testing.T) {
	const yamlWithClaudeBlock = `
updated: 2026-09-08
providers:
  - name: anthropic-acme-widgets
    label: Anthropic / Claude Pro (Acme Widgets)
    plan: Claude Pro
    probe: claude-oauth
    claude:
      credentials_file: /home/ryan/.claude-acmewidgets/.credentials.json
      ssh_host: minty
    models:
      - id: claude-sonnet-5
        nicknames: [sonnet]
`
	path := filepath.Join(t.TempDir(), "models.yaml")
	if err := os.WriteFile(path, []byte(yamlWithClaudeBlock), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	r, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(r.Providers) != 1 {
		t.Fatalf("Providers = %d, want 1", len(r.Providers))
	}
	cfg := r.Providers[0].ClaudeConfig
	if cfg == nil {
		t.Fatal("ClaudeConfig is nil")
	}
	if cfg.CredentialsFile != "/home/ryan/.claude-acmewidgets/.credentials.json" {
		t.Errorf("CredentialsFile = %q", cfg.CredentialsFile)
	}
	if cfg.SSHHost != "minty" {
		t.Errorf("SSHHost = %q, want minty", cfg.SSHHost)
	}
}

func TestResolve(t *testing.T) {
	r := loadTestRoster(t)

	tests := []struct {
		name        string
		query       string
		wantEmpty   bool
		wantAnyOf   []string // acceptable ModelID values (any one match is fine)
		wantAll     []string // every listed ModelID must appear among matches
		wantMatches int      // exact count check when > 0
	}{
		{name: "exact nickname", query: "gpt-sol", wantAnyOf: []string{"gpt-5.6-sol"}},
		{name: "spaced+capitalized nickname", query: "GPT Sol", wantAnyOf: []string{"gpt-5.6-sol"}},
		{name: "nickname with space and dot", query: "fable 5.1", wantAnyOf: []string{"claude-fable-5-1"}},
		{name: "nickname with leading digits and dot", query: "3.1 pro", wantAnyOf: []string{"gemini-3.1-pro"}},
		{name: "exact model ID", query: "claude-fable-5-1", wantAnyOf: []string{"claude-fable-5-1"}},
		{name: "model ID normalized differently", query: "Claude Fable 5 1", wantAnyOf: []string{"claude-fable-5-1"}},
		{name: "no match", query: "totally-unknown-model", wantEmpty: true},
		{name: "empty query", query: "", wantEmpty: true},
		{name: "sonnet resolves across providers", query: "sonnet", wantAll: []string{"claude-sonnet-5"}, wantMatches: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matches := r.Resolve(tt.query)
			if tt.wantEmpty {
				if len(matches) != 0 {
					t.Errorf("Resolve(%q) = %v, want no matches", tt.query, matches)
				}
				return
			}
			if len(matches) == 0 {
				t.Fatalf("Resolve(%q) = no matches, want at least one", tt.query)
			}
			if len(tt.wantAnyOf) > 0 {
				found := false
				for _, m := range matches {
					for _, want := range tt.wantAnyOf {
						if m.ModelID == want {
							found = true
						}
					}
				}
				if !found {
					t.Errorf("Resolve(%q) = %v, want one of %v", tt.query, matches, tt.wantAnyOf)
				}
			}
			if tt.wantMatches > 0 && len(matches) != tt.wantMatches {
				t.Errorf("Resolve(%q) returned %d matches, want %d: %v", tt.query, len(matches), tt.wantMatches, matches)
			}
			for _, want := range tt.wantAll {
				found := false
				for _, m := range matches {
					if m.ModelID == want {
						found = true
					}
				}
				if !found {
					t.Errorf("Resolve(%q) = %v, want it to include %q", tt.query, matches, want)
				}
			}
		})
	}
}

func TestNormalizeNickname(t *testing.T) {
	tests := map[string]string{
		"gpt-sol":    "gptsol",
		"GPT Sol":    "gptsol",
		"gpt.sol":    "gptsol",
		"gpt_sol":    "gptsol",
		"fable 5.1":  "fable51",
		"3.1 pro":    "31pro",
		"":           "",
		"  spaced  ": "spaced",
	}
	for in, want := range tests {
		if got := normalizeNickname(in); got != want {
			t.Errorf("normalizeNickname(%q) = %q, want %q", in, got, want)
		}
	}
}
