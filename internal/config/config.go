package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	ConfigDir  = ".aida"
	ConfigFile = "config.yaml"
)

type Config struct {
	Model         ModelConfig        `yaml:"model"`
	ActiveProfile string             `yaml:"active_profile"`
	Profiles      map[string]Profile `yaml:"profiles"`
	API           APIConfig          `yaml:"api"`
	Editor        string             `yaml:"editor,omitempty"`
	Brain         BrainConfig        `yaml:"brain,omitempty"`
	Agent         AgentYAMLConfig    `yaml:"agent,omitempty"`
	Wiki          WikiConfig         `yaml:"wiki,omitempty"`
	Jobs          JobsConfig         `yaml:"jobs,omitempty"`
	Devices       []DeviceConfig     `yaml:"devices,omitempty"`
	Fleet         FleetConfig        `yaml:"fleet,omitempty"`
	Bifrost       BifrostConfig      `yaml:"bifrost,omitempty"`
	Menu          MenuConfig         `yaml:"menu,omitempty"`
	Habits        HabitsConfig       `yaml:"habits,omitempty"`
	Training      TrainingConfig     `yaml:"training,omitempty"`
	Models        ModelsConfig       `yaml:"models,omitempty"`
	Burndown      BurndownConfig     `yaml:"burndown,omitempty"`
	// IDPatterns configures the identifier shapes ExtractIDsFromText (see
	// ari_extractor.go) recognizes when scanning free text -- e.g. a docs
	// page listing per-environment resource IDs -- for semantic key
	// inference. Empty uses DefaultIDPatterns (a generic 16-char
	// alphanumeric ID with no category/modifier classification).
	IDPatterns []IDPattern `yaml:"id_patterns,omitempty"`
	// Timezone is an IANA zone name (e.g. "America/New_York") used for
	// time/date questions (the current-time source, the zero-LLM time
	// shortcut, and datetime injection into LLM prompts). Empty means
	// "use the OS-local zone" -- see Location().
	Timezone string `yaml:"timezone,omitempty"`
}

// Location resolves the configured timezone, falling back to the OS-local
// zone when unset or invalid.
func (c *Config) Location() *time.Location {
	if c.Timezone == "" {
		return time.Local
	}
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return time.Local
	}
	return loc
}

// AgentYAMLConfig holds agent loop settings configurable via config.yaml.
type AgentYAMLConfig struct {
	MaxTurns     int     `yaml:"max_turns,omitempty"`
	MaxBudgetUSD float64 `yaml:"max_budget_usd,omitempty"`
	Model        string  `yaml:"model,omitempty"`
}

// BrainConfig holds settings for the shared brain/memory system.
type BrainConfig struct {
	Path       string          `yaml:"path,omitempty"`      // defaults to ~/.aida/brain
	Remote     string          `yaml:"remote,omitempty"`    // git remote URL
	AutoSync   bool            `yaml:"auto_sync,omitempty"` // pull/push on every query
	Embeddings EmbeddingConfig `yaml:"embeddings,omitempty"`
}

// GitHubRepo extracts owner/repo from the brain's git remote URL.
func (b BrainConfig) GitHubRepo() string {
	return ParseGitHubRepo(b.Remote)
}

// ParseGitHubRepo extracts owner/repo from a git remote URL.
// Supports both SSH (git@github.com:owner/repo.git) and HTTPS (https://github.com/owner/repo.git).
func ParseGitHubRepo(remote string) string {
	if remote == "" {
		return ""
	}
	// SSH: git@github.com:owner/repo.git
	if strings.Contains(remote, "github.com:") {
		parts := strings.SplitN(remote, ":", 2)
		if len(parts) == 2 {
			return strings.TrimSuffix(parts[1], ".git")
		}
	}
	// HTTPS: https://github.com/owner/repo.git
	if strings.Contains(remote, "github.com/") {
		idx := strings.Index(remote, "github.com/")
		return strings.TrimSuffix(remote[idx+len("github.com/"):], ".git")
	}
	return ""
}

// JobsConfig holds settings for the background-agent jobs system
// (internal/jobs), including where pr_work jobs find a parent git repo.
type JobsConfig struct {
	// PRWorkRepoRoot is the git repository pr_work jobs use as the parent
	// for `git worktree add`. Needed because `aida serve` running under the
	// launchd daemon has WorkingDirectory set to the user's home directory
	// (scripts/launchd/com.ryanlitalien.aida.plist), which is not a git
	// repo, so resolving the repo from the process's cwd fails there. Set
	// this to the aida checkout (e.g. ~/dev/aida) to fix pr_work jobs under
	// the deployed daemon. Empty means "resolve from cwd instead", which is
	// what an interactive `aida serve` run from inside a checkout gets for
	// free.
	PRWorkRepoRoot string `yaml:"pr_work_repo_root,omitempty"`
}

// WikiConfig holds settings for the personal wiki -- a separate git repo
// of markdown pages (projects/entities/concepts) distilled from a user's
// own archives, distinct from the brain's lessons/memory/tasks. See
// internal/brain/wiki_index.go for the indexing pipeline and
// internal/cli/wiki_lint.go for the lint tool.
type WikiConfig struct {
	Path string `yaml:"path,omitempty"` // defaults to ~/dev/aida-wiki
}

// MenuConfig holds settings for the read-only weekly dinner menu page
// (`aida serve`'s /menu route + LAN listener). Mirrors WikiConfig's shape --
// a single configurable path with a sane default, since the menu itself
// lives outside ~/.aida in a separate personal-health repo.
type MenuConfig struct {
	Path string `yaml:"path,omitempty"` // defaults to ~/dev/health/nutrition/menu.yaml
}

// HabitsConfig holds settings for the habit-tracking calendar page
// (`aida serve`'s /habits route). Mirrors MenuConfig's shape -- two
// configurable paths with sane defaults, since habit data lives outside
// ~/.aida in the same personal-health repo the menu page reads.
type HabitsConfig struct {
	DBPath        string `yaml:"db_path,omitempty"`         // defaults to ~/dev/health/body/habits.db
	FoodDiaryPath string `yaml:"food_diary_path,omitempty"` // defaults to ~/dev/health/nutrition/data/food_diary.json
}

// TrainingConfig holds settings for the personal-health repo's workout log,
// read (never written) by the habits page to auto-detect a completed workout
// on days with no explicit habits.db checkin. Mirrors HabitsConfig's shape.
type TrainingConfig struct {
	DBPath string `yaml:"db_path,omitempty"` // defaults to ~/dev/health/training/workouts.db
}

// ModelsConfig holds settings for the AI provider/plan/nickname roster
// (`aida models`, GET /api/models, the Models panel on /dashboard).
// Mirrors MenuConfig's shape -- a single configurable path with a sane
// default, since the roster is hand-maintained state, not derived data.
type ModelsConfig struct {
	Path string `yaml:"path,omitempty"` // defaults to ~/.aida/models.yaml
}

// BurndownConfig holds settings for the burn-down floors config
// (`aida burndown capacity`, internal/burndown). Mirrors ModelsConfig's
// shape exactly -- a single configurable path with a sane default,
// hand-maintained state alongside the models roster it's read next to.
type BurndownConfig struct {
	Path string `yaml:"path,omitempty"` // defaults to ~/.aida/burndown.yaml
}

// EmbeddingConfig holds embedding provider settings. Model and output
// dimension are deliberately NOT config here -- both are pinned constants
// in internal/brain/embeddings.go (voyageModel, voyageOutputDimension)
// because every row in brain.db must share one model's vector space; a
// config override with no corresponding full re-embed would silently mix
// incomparable vectors in the same table. Bump the const and run
// `aida brain reembed` instead.
type EmbeddingConfig struct {
	Provider  string `yaml:"provider,omitempty"`    // "voyage" (default)
	APIKeyEnv string `yaml:"api_key_env,omitempty"` // env var name, default "VOYAGE_API_KEY"
}

// BrainPath returns the resolved brain directory path.
func (c *Config) BrainPath() string {
	if c.Brain.Path != "" {
		return expandPath(c.Brain.Path)
	}
	return filepath.Join(Dir(), "brain")
}

// WikiPath returns the resolved wiki repo directory path. Mirrors
// BrainPath's shape, but the wiki lives outside ~/.aida -- it's a
// standalone git repo cloned alongside other dev checkouts.
func (c *Config) WikiPath() string {
	if c.Wiki.Path != "" {
		return expandPath(c.Wiki.Path)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", "dev", "aida-wiki")
	}
	return filepath.Join(home, "dev", "aida-wiki")
}

// MenuPath returns the resolved dinner-menu YAML path. Mirrors WikiPath's
// shape: an explicit config value wins (tilde-expanded), else the personal
// default of ~/dev/health/nutrition/menu.yaml.
func (c *Config) MenuPath() string {
	if c.Menu.Path != "" {
		return expandPath(c.Menu.Path)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", "dev", "health", "nutrition", "menu.yaml")
	}
	return filepath.Join(home, "dev", "health", "nutrition", "menu.yaml")
}

// HabitsDBPath returns the resolved habit-tracker SQLite path. Mirrors
// MenuPath's shape: an explicit config value wins (tilde-expanded), else
// the personal default of ~/dev/health/body/habits.db -- the same DB
// `~/dev/health/scripts/habit.py` reads and writes.
func (c *Config) HabitsDBPath() string {
	if c.Habits.DBPath != "" {
		return expandPath(c.Habits.DBPath)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", "dev", "health", "body", "habits.db")
	}
	return filepath.Join(home, "dev", "health", "body", "habits.db")
}

// FoodDiaryPath returns the resolved food-diary JSON path (a flat list of
// logged meals) used to compute the habits page's read-only
// "eating-correctly" percentage. Mirrors MenuPath's shape.
func (c *Config) FoodDiaryPath() string {
	if c.Habits.FoodDiaryPath != "" {
		return expandPath(c.Habits.FoodDiaryPath)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", "dev", "health", "nutrition", "data", "food_diary.json")
	}
	return filepath.Join(home, "dev", "health", "nutrition", "data", "food_diary.json")
}

// WorkoutsDBPath returns the resolved workout-log SQLite path (the `workouts`
// table, keyed by a `workout_day` YYYY-MM-DD column) that the habits page
// reads read-only to auto-detect a completed session on days with no
// explicit manual checkin. Mirrors HabitsDBPath's shape: an explicit config
// value wins (tilde-expanded), else the personal default of
// ~/dev/health/training/workouts.db.
func (c *Config) WorkoutsDBPath() string {
	if c.Training.DBPath != "" {
		return expandPath(c.Training.DBPath)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", "dev", "health", "training", "workouts.db")
	}
	return filepath.Join(home, "dev", "health", "training", "workouts.db")
}

// ModelsPath returns the resolved AI-model-roster YAML path. Mirrors
// MenuPath's shape: an explicit config value wins (tilde-expanded), else
// the default of ~/.aida/models.yaml -- unlike Menu/Habits/Training,
// whose data lives outside ~/.aida in the personal-health repo, this
// roster lives alongside aida's own config, so the fallback is
// filepath.Join(Dir(), ...) rather than a ~/dev path.
func (c *Config) ModelsPath() string {
	if c.Models.Path != "" {
		return expandPath(c.Models.Path)
	}
	return filepath.Join(Dir(), "models.yaml")
}

// BurndownPath returns the resolved burn-down floors config path. Mirrors
// ModelsPath's shape exactly: an explicit config value wins (tilde-
// expanded), else the default of ~/.aida/burndown.yaml.
func (c *Config) BurndownPath() string {
	if c.Burndown.Path != "" {
		return expandPath(c.Burndown.Path)
	}
	return filepath.Join(Dir(), "burndown.yaml")
}

// PRWorkRepoRoot returns the configured repo root for pr_work jobs, with
// tilde expansion applied. Empty means "not configured" -- callers should
// fall back to resolving the repo from the process's cwd.
func (c *Config) PRWorkRepoRoot() string {
	if c.Jobs.PRWorkRepoRoot == "" {
		return ""
	}
	return expandPath(c.Jobs.PRWorkRepoRoot)
}

// VoyageKeyEnv returns the env var name for the Voyage API key.
func (c *Config) VoyageKeyEnv() string {
	if c.Brain.Embeddings.APIKeyEnv != "" {
		return c.Brain.Embeddings.APIKeyEnv
	}
	return "VOYAGE_API_KEY"
}

type ModelConfig struct {
	Primary  string `yaml:"primary"`
	Fallback string `yaml:"fallback,omitempty"`
	// Offline names the local Ollama model to use for --offline (or
	// offline_mode: true) runs, overriding the built-in Ollama default
	// (llm.DefaultOllamaModel). Unset by default: --offline never needs a
	// cloud API key, but a fallback that names a hosted model (e.g. a
	// Gemini or Claude model) is ignored in favor of the Ollama default
	// rather than sent to a provider with no credentials - set this only
	// to point at a different pulled Ollama model.
	Offline     string            `yaml:"offline,omitempty"`
	OfflineMode bool              `yaml:"offline_mode"`
	Stages      map[string]string `yaml:"stages,omitempty"` // per-stage overrides: "parse" → model, "synthesize" → model
}

// ModelForStage returns the model to use for a given pipeline stage.
// Falls back to Primary if no stage-specific override exists.
func (m ModelConfig) ModelForStage(stage string) string {
	if m.Stages != nil {
		if model, ok := m.Stages[stage]; ok && model != "" {
			return model
		}
	}
	return m.Primary
}

// GuardrailsConfig holds per-profile safety constraints for agent mode.
type GuardrailsConfig struct {
	ConfirmAlways []string `yaml:"confirm_always,omitempty"`
}

type Profile struct {
	Detect         DetectConfig          `yaml:"detect,omitempty"`
	ScanPaths      []string              `yaml:"scan_paths,omitempty"`
	Tools          map[string]string     `yaml:"tools,omitempty"`
	Cloud          *CloudConfig          `yaml:"cloud,omitempty"`
	Guardrails     GuardrailsConfig      `yaml:"guardrails,omitempty"`
	Daily          *DailyConfig          `yaml:"daily,omitempty"`
	Serve          *ServeConfig          `yaml:"serve,omitempty"`
	ClaudeFallback *ClaudeFallbackConfig `yaml:"claude_fallback,omitempty"`
}

// ClaudeFallbackConfig controls the `claude -p` connector fallback: when `aida`
// itself can't answer a connector-shaped question (email/calendar/slack/etc.),
// it proxies to `claude -p`, which reaches the user's OAuth MCP connectors that
// `aida`'s own discovery can't. Defaults are ON so voice/terminal connector
// queries work out of the box; set enabled:false per profile to disable.
//
// Enabled is a *bool so "unset" (nil) can mean ON - a plain bool zero-value
// would default the feature OFF and silently defeat the goal (same reason
// ServeConfig.Jarvis is a pointer).
type ClaudeFallbackConfig struct {
	Enabled        *bool  `yaml:"enabled,omitempty"`         // nil => ON
	Model          string `yaml:"model,omitempty"`           // "" => claude's default (capable, for multi-step tool-use)
	TimeoutSeconds int    `yaml:"timeout_seconds,omitempty"` // 0 => 90
	Cwd            string `yaml:"cwd,omitempty"`             // "" => $HOME
}

// ServeConfig holds per-profile defaults for `aida serve`. Each field is a
// pointer so nil means "unset" - equivalent to the historical CLI default.
// Explicit CLI flags (--no-jarvis, --no-listen) always win over profile config.
type ServeConfig struct {
	// Jarvis toggles the Jarvis HTTP routes + voice loop. nil = on (legacy default).
	Jarvis *bool `yaml:"jarvis,omitempty"`
	// Listen toggles the always-on mic listener. Requires Jarvis to be enabled.
	// nil = on (legacy default).
	Listen *bool `yaml:"listen,omitempty"`
	// MicPrefer is a priority-ordered list of input-device name substrings the
	// listener should pick (case-insensitive), resolved by NAME at startup
	// because avfoundation indices shift as devices connect/disconnect. The
	// first connected match wins; empty or no-match falls back to the system
	// default mic (:0). Example: ["Unknown USB Audio Device", "MacBook Pro Microphone"].
	MicPrefer []string `yaml:"mic_prefer,omitempty"`
	// MicPreferByHost overrides MicPrefer per machine - the same profile (e.g.
	// "home") runs on several computers (a clamshell MacBook Pro on a webcam
	// mic, a MacBook Air on its built-in), and the config syncs across them, so
	// the mic choice must be keyed to the machine. Keys are case-insensitive
	// substrings of the machine name (scutil LocalHostName, e.g. "edith");
	// the first matching key's list wins, else MicPrefer is used. Keys stop
	// matching when a machine is renamed - re-key the config to the new name.
	MicPreferByHost map[string][]string `yaml:"mic_prefer_by_host,omitempty"`
	// LMDWhisperModel overrides the whisper.cpp model path the LMD (Android
	// client) turn handler's Transcriber uses - see internal/jarvis/lmd and
	// stt.TinyEnModelPath. Empty defaults to tiny.en: unlike the desk-mic
	// listener, which typically runs on Apple Silicon with Metal
	// acceleration and can afford the small.en default's better accuracy
	// for ~100-300ms of extra time per turn, the LMD daemon may run on much
	// weaker hardware (e.g. a 2-core Pentium with no GPU), where small.en
	// measured 94s versus tiny.en's 10.6s on an 11s clip. Set to an
	// explicit ggml-*.bin path (e.g.
	// ~/.aida/jarvis/models/ggml-small.en.bin) to trade latency for
	// accuracy on capable hardware.
	LMDWhisperModel string `yaml:"lmd_whisper_model,omitempty"`
}

// ResolveMicPrefer returns the mic-preference list for the given machine name:
// the first MicPreferByHost key that is a substring of host (sorted for
// determinism), else the profile-default MicPrefer. Pure; unit-tested.
func (s *ServeConfig) ResolveMicPrefer(host string) []string {
	if s == nil {
		return nil
	}
	if len(s.MicPreferByHost) > 0 && strings.TrimSpace(host) != "" {
		h := strings.ToLower(host)
		keys := make([]string, 0, len(s.MicPreferByHost))
		for k := range s.MicPreferByHost {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if strings.Contains(h, strings.ToLower(k)) {
				return s.MicPreferByHost[k]
			}
		}
	}
	return s.MicPrefer
}

// LMDWhisperModelPath returns the resolved path to the LMD (Android client)
// whisper model, with tilde expansion applied. Empty means "not configured" -
// callers should fall back to the default tiny.en model.
func (s *ServeConfig) LMDWhisperModelPath() string {
	if s == nil || s.LMDWhisperModel == "" {
		return ""
	}
	return expandPath(s.LMDWhisperModel)
}

// DailyConfig holds per-profile settings for `aida daily`.
// Nil or Enabled=false means the profile has not opted in.
type DailyConfig struct {
	Enabled bool `yaml:"enabled"`
	// Pipeline picks the briefing implementation. Empty or "claude" runs the
	// legacy claude -p + MCP path (still works on profiles where a former
	// employer's managed-settings.json doesn't lock down gdrive perms).
	// "gws" runs the pure-Go pipeline that calls a former employer's gws
	// CLI directly - required on
	// profiles where allowManagedPermissionRulesOnly: true blocks MCP allowlists.
	Pipeline                string   `yaml:"pipeline,omitempty"`
	ProjectDir              string   `yaml:"project_dir,omitempty"`
	EmailTo                 string   `yaml:"email_to,omitempty"`
	GmailLabels             []string `yaml:"gmail_labels,omitempty"`
	GmailPartnerLabelPrefix string   `yaml:"gmail_partner_label_prefix,omitempty"`
	GmailPartners           []string `yaml:"gmail_partners,omitempty"`
	GmailExclude            string   `yaml:"gmail_exclude,omitempty"`
	// GmailBriefingLabel, when non-empty, is applied to the sent briefing message
	// post-send (best-effort). Looked up by name via users.labels list; create the
	// label manually in Gmail first. Empty = no labeling.
	GmailBriefingLabel string `yaml:"gmail_briefing_label,omitempty"`
	TaskTag            string `yaml:"task_tag,omitempty"`
	GoogleTasksListID  string `yaml:"google_tasks_list_id,omitempty"`
	WatchdogSeconds    int    `yaml:"watchdog_seconds,omitempty"`
	// Model overrides the model claude -p uses. When empty, claude picks its default.
	// The daily routine is mechanical (fetch/format/send) and doesn't need Opus -
	// pinning Haiku here cuts wall-clock 5–10x at a fraction of the cost.
	Model string `yaml:"model,omitempty"`
	// NotionBackup, when set, configures the `aida daily --notion-backup` mode.
	// The Notion meeting backup runs on its own schedule (every 4h via launchd)
	// so a wedged Notion call can't hang the time-critical 6am briefing.
	NotionBackup *NotionBackupConfig `yaml:"notion_backup,omitempty"`
}

// NotionBackupConfig holds settings for `aida daily --notion-backup`.
// Decoupled from the briefing so Notion latency/instability doesn't
// block the email pipeline.
type NotionBackupConfig struct {
	WatchdogSeconds int    `yaml:"watchdog_seconds,omitempty"`
	Model           string `yaml:"model,omitempty"`
}

// CloudConfig holds per-profile managed agent settings for `aida investigate`.
type CloudConfig struct {
	APIKeyEnv      string      `yaml:"api_key_env,omitempty"`
	APIKeyFile     string      `yaml:"api_key_file,omitempty"`
	AgentID        string      `yaml:"agent_id,omitempty"`
	EnvironmentID  string      `yaml:"environment_id,omitempty"`
	Model          string      `yaml:"model,omitempty"`
	ContextPaths   []string    `yaml:"context_paths,omitempty"`
	GitHubTokenEnv string      `yaml:"github_token_env,omitempty"` // env var name for GitHub PAT
	VaultID        string      `yaml:"vault_id,omitempty"`         // per-profile vault ID
	MCPServers     []MCPServer `yaml:"mcp_servers,omitempty"`      // MCP servers for this profile
}

// MCPServer defines an MCP server endpoint for vault-based credential auth.
type MCPServer struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
}

// GetAPIKey resolves the cloud API key. Checks cloud-specific env var first,
// then file, then falls back to the global APIConfig.
func (c *CloudConfig) GetAPIKey(fallback *APIConfig) string {
	if c == nil {
		if fallback != nil {
			return resolveAPIConfig(fallback)
		}
		return ""
	}
	if c.APIKeyEnv != "" {
		if key := os.Getenv(c.APIKeyEnv); key != "" {
			return key
		}
	}
	if c.APIKeyFile != "" {
		path := expandPath(c.APIKeyFile)
		data, err := os.ReadFile(path)
		if err == nil {
			return strings.TrimSpace(string(data))
		}
	}
	if fallback != nil {
		return resolveAPIConfig(fallback)
	}
	return ""
}

// GetGitHubToken resolves the GitHub PAT from the configured env var.
func (c *CloudConfig) GetGitHubToken() string {
	if c == nil || c.GitHubTokenEnv == "" {
		return ""
	}
	return os.Getenv(c.GitHubTokenEnv)
}

// resolveAPIConfig reads the API key from an APIConfig (env var or file).
func resolveAPIConfig(api *APIConfig) string {
	if api.AnthropicKeyEnv != "" {
		if key := os.Getenv(api.AnthropicKeyEnv); key != "" {
			return key
		}
	}
	if api.AnthropicKeyFile != "" {
		path := expandPath(api.AnthropicKeyFile)
		data, err := os.ReadFile(path)
		if err == nil {
			return strings.TrimSpace(string(data))
		}
	}
	return ""
}

type DetectConfig struct {
	HasTool     string `yaml:"has_tool,omitempty"`
	MissingTool string `yaml:"missing_tool,omitempty"`
}

type APIConfig struct {
	AnthropicKeyEnv  string `yaml:"anthropic_key_env,omitempty"`
	AnthropicKeyFile string `yaml:"anthropic_key_file,omitempty"`
}

// ConfigDir returns the path to ~/.aida/
func Dir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ConfigDir)
	}
	return filepath.Join(home, ConfigDir)
}

// EnsureDir creates ~/.aida/ if it doesn't exist.
func EnsureDir() error {
	return os.MkdirAll(Dir(), 0755)
}

// LoadDotEnv reads .env files and sets any unset environment variables.
// Priority: ~/.aida/.env, then ~/.aida/op.env, then walks up from cwd.
// Lines must be in KEY=VALUE format (an optional leading "export " is
// stripped first). Lines starting with # are ignored.
//
// ~/.aida/op.env holds 1Password service-account tokens (e.g.
// OP_SERVICE_ACCOUNT_TOKEN) and interim `export OP_SESSION_<ID>=...` lines.
// It is deliberately separate from ~/.aida/.env: the private secrets-sync tooling (aida-config)
// rewrites .env from env.tpl via `op inject --force`, which would wipe any
// hand-added token line placed there instead. Loading op.env here means
// every agent aida spawns -- `aida serve` jobs, `aida loop`, delegated
// `claude -p` -- inherits the service-account token and `op` works headless,
// without Touch ID.
func LoadDotEnv() {
	// 1. Always check ~/.aida/.env (works from any directory)
	loadEnvFile(filepath.Join(Dir(), ".env"))

	// 2. ~/.aida/op.env: 1Password tokens, never rendered by the secrets sync
	loadEnvFile(filepath.Join(Dir(), "op.env"))

	// 3. Walk up from cwd for project-local .env files
	dir, err := os.Getwd()
	if err != nil {
		return
	}
	for {
		loadEnvFile(filepath.Join(dir, ".env"))
		parent := filepath.Dir(dir)
		if parent == dir {
			return
		}
		dir = parent
	}
}

// loadEnvFile reads a single .env file, setting any vars not already in the
// environment. Each line may carry an optional leading "export " prefix
// (stripped before parsing), matching how op.env's interim
// `export OP_SESSION_<ID>=...` lines are written.
func loadEnvFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
}

// anthropicRoutingEnvPrefixes are environment-variable name prefixes that
// select or authenticate to a backend other than the user's direct claude.ai
// OAuth/subscription session: a third-party inference platform (Amazon
// Bedrock, Google Vertex AI, Microsoft Foundry, Claude Platform on AWS), or a
// Claude Code feature flag that turns one on. Claude Code scopes every
// credential, endpoint override, and routing knob for a given backend under
// one prefix (see code.claude.com/docs/en/env-vars), so matching on the
// prefix keeps this durable as those backends grow new configuration knobs --
// unlike an enumerated list, a newly added `ANTHROPIC_VERTEX_REGION` or a
// fifth `CLAUDE_CODE_USE_*` backend is covered without a code change.
var anthropicRoutingEnvPrefixes = []string{
	"CLAUDE_CODE_USE_",   // CLAUDE_CODE_USE_BEDROCK / _VERTEX / _FOUNDRY / _MANTLE: backend-enable switches
	"ANTHROPIC_AWS_",     // Claude Platform on AWS: API key, base URL, workspace ID
	"ANTHROPIC_BEDROCK_", // Amazon Bedrock: base URL, Mantle base URL, service tier
	"ANTHROPIC_VERTEX_",  // Google Vertex AI: base URL, project ID
	"ANTHROPIC_FOUNDRY_", // Microsoft Foundry: API key, auth token, base URL, resource name
}

// anthropicRoutingEnvNames are individual environment variables that
// authenticate or redirect a delegated `claude` invocation but don't share
// one of the prefixes above, so they're matched by exact name instead.
var anthropicRoutingEnvNames = map[string]bool{
	// Direct Anthropic API credentials: force API-key mode, where claude.ai
	// OAuth connectors (Slack, Gmail, Notion, ...) are unavailable.
	"ANTHROPIC_API_KEY":    true,
	"ANTHROPIC_AUTH_TOKEN": true,
	// Long-lived subscription bearer token minted by `claude setup-token`.
	// It keeps the child on the user's Claude plan but, per Anthropic's own
	// docs, "can only make model requests" -- it can't fetch claude.ai
	// connectors either, so it defeats this scrub's intent just as
	// thoroughly as an API key while looking like a legitimate login.
	"CLAUDE_CODE_OAUTH_TOKEN": true,
	// Workload identity federation: exchanges a federated token for a
	// different Anthropic workspace than the user's own.
	"ANTHROPIC_WORKSPACE_ID": true,
	// Redirects every request to an arbitrary host (LLM gateway/proxy),
	// regardless of which auth mode is in effect.
	"ANTHROPIC_BASE_URL": true,
	// Arbitrary extra request headers -- can carry alternate auth material
	// for a gateway.
	"ANTHROPIC_CUSTOM_HEADERS": true,
	// Bedrock-only bearer credential. Deliberately NOT the generic
	// AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / AWS_SESSION_TOKEN /
	// AWS_PROFILE / AWS_REGION vars: those are inert for Claude Code unless
	// CLAUDE_CODE_USE_BEDROCK is also set (which this scrub already strips),
	// and the child may need them for the user's own unrelated AWS work.
	"AWS_BEARER_TOKEN_BEDROCK": true,
}

// ScrubAnthropicCreds returns env with every Anthropic/Claude-Code credential
// and backend-routing variable removed. LoadDotEnv pulls ANTHROPIC_API_KEY
// from ~/.aida/.env into the process env, and a delegated `claude` subprocess
// that inherits it (or any of the other variables below) never reaches the
// user's interactive claude.ai OAuth/subscription session -- it authenticates
// as an API key, a long-lived setup-token, or a third-party cloud account
// instead. That matters because claude.ai account connectors (Slack, Gmail,
// Notion, ...) are only reachable over the OAuth/subscription path, a former
// employer's managed-settings forceLoginOrgUUID rejects `claude -p` outright in API-key
// mode, and a Bedrock/Vertex/Foundry credential would bill the request to
// whatever cloud account the credential belongs to (e.g. a separate company's
// AWS account), not the user's Anthropic subscription. Strip all of it so a
// delegated claude always falls back to the OAuth/subscription path.
//
// This is a floor, not a ceiling: it can't reach credentials configured
// outside the environment, namely an `apiKeyHelper` script or a signed-in
// Claude apps gateway session recorded in `.credentials.json` -- both
// outrank environment variables in Claude Code's own precedence order and
// would need to be handled separately if they become relevant here.
func ScrubAnthropicCreds(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if anthropicRoutingEnvNames[k] {
			continue
		}
		if hasAnthropicRoutingEnvPrefix(k) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// hasAnthropicRoutingEnvPrefix reports whether k starts with one of
// anthropicRoutingEnvPrefixes.
func hasAnthropicRoutingEnvPrefix(k string) bool {
	for _, prefix := range anthropicRoutingEnvPrefixes {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// LoadConfig reads config.yaml from ~/.aida/
func LoadConfig() (*Config, error) {
	path := filepath.Join(Dir(), ConfigFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultConfig(), nil
		}
		return nil, fmt.Errorf("reading config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	return &cfg, nil
}

// SaveConfig writes config.yaml to ~/.aida/
func SaveConfig(cfg *Config) error {
	if err := EnsureDir(); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	path := filepath.Join(Dir(), ConfigFile)
	return os.WriteFile(path, data, 0644)
}

// DefaultConfig returns a sensible default configuration.
func DefaultConfig() *Config {
	return &Config{
		Model: ModelConfig{
			Primary:     "claude-haiku-4-5-20251001",
			Fallback:    "gemini-3.7-flash",
			OfflineMode: false,
		},
		ActiveProfile: "auto",
		Profiles: map[string]Profile{
			// A single undetected default profile. Multiple profiles
			// (e.g. a "work" profile scoped to a work-only CLI on PATH)
			// are still fully supported via explicit `profiles:` config
			// and Profile.Detect (HasTool/MissingTool) - this is just
			// the out-of-the-box shape for a fresh ~/.aida/.
			"home": {
				ScanPaths: []string{"~/dev"},
				Tools: map[string]string{
					"gh": "gh",
				},
			},
		},
		API: APIConfig{
			AnthropicKeyEnv: "ANTHROPIC_API_KEY",
		},
	}
}

// ActiveProfile returns the resolved profile based on auto-detection or explicit setting.
// Precedence: AIDA_PROFILE env var → active_profile yaml → auto-detect → first profile.
func (c *Config) ActiveProfileConfig() (*Profile, string) {
	if env := os.Getenv("AIDA_PROFILE"); env != "" {
		if p, ok := c.Profiles[env]; ok {
			return &p, env
		}
		// Unknown profile name: fall through silently to the rest of the logic.
	}
	if c.ActiveProfile != "auto" {
		if p, ok := c.Profiles[c.ActiveProfile]; ok {
			return &p, c.ActiveProfile
		}
	}
	// Auto-detect: try each profile's detect rules
	for name, p := range c.Profiles {
		if p.Detect.HasTool != "" {
			if _, err := exec.LookPath(p.Detect.HasTool); err == nil {
				return &p, name
			}
		}
		if p.Detect.MissingTool != "" {
			if _, err := exec.LookPath(p.Detect.MissingTool); err != nil {
				return &p, name
			}
		}
	}
	// Fallback: first profile
	for name, p := range c.Profiles {
		return &p, name
	}
	return nil, ""
}

// ActiveClaudeFallbackConfig returns the active profile's claude -p connector
// fallback settings with defaults filled in, so callers never nil-check:
// fallback ON unless explicitly disabled, 90s timeout, claude's default model,
// $HOME cwd. Mirrors ActiveProfileConfig's resolution.
func (c *Config) ActiveClaudeFallbackConfig() ClaudeFallbackConfig {
	out := ClaudeFallbackConfig{}
	if p, _ := c.ActiveProfileConfig(); p != nil && p.ClaudeFallback != nil {
		out = *p.ClaudeFallback
	}
	if out.Enabled == nil {
		on := true
		out.Enabled = &on
	}
	if out.TimeoutSeconds == 0 {
		out.TimeoutSeconds = 90
	}
	out.Cwd = expandPath(out.Cwd) // resolve ~/... so claude -p runs in the real dir
	return out
}

// GetAPIKey resolves the Anthropic API key from env var or file.
func (c *Config) GetAPIKey() string {
	if c.API.AnthropicKeyEnv != "" {
		if key := os.Getenv(c.API.AnthropicKeyEnv); key != "" {
			return key
		}
	}
	if c.API.AnthropicKeyFile != "" {
		path := expandPath(c.API.AnthropicKeyFile)
		data, err := os.ReadFile(path)
		if err == nil {
			return strings.TrimSpace(string(data))
		}
	}
	return ""
}

// GetEditor returns the configured editor or falls back to $EDITOR or "vi".
func (c *Config) GetEditor() string {
	if c.Editor != "" {
		if strings.HasPrefix(c.Editor, "$") {
			if val := os.Getenv(c.Editor[1:]); val != "" {
				return val
			}
		}
		return c.Editor
	}
	if editor := os.Getenv("EDITOR"); editor != "" {
		return editor
	}
	return "vi"
}

func expandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

// ExpandPath is the exported version for use by other packages.
func ExpandPath(path string) string {
	return expandPath(path)
}
