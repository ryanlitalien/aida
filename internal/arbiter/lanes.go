package arbiter

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// DataClass is the sensitivity ceiling a task's tags resolve to (see
// ClassifyTags in dataclass.go) and the allowlist token a Lane carries in
// DataClasses. The four values are ordered most-restrictive-first:
// personal > butterstack > games > public.
type DataClass string

// The four data classes docs/arbiter-plan.md section 3 defines. A lane's
// DataClasses is an explicit allowlist of these -- there is no "allow
// everything" wildcard, by design: a new data class must be added to a
// lane on purpose, never inherited silently.
const (
	DataClassPersonal    DataClass = "personal"
	DataClassButterstack DataClass = "butterstack"
	DataClassGames       DataClass = "games"
	DataClassPublic      DataClass = "public"
)

// Role names used as keys into Lane.Models. Only "executor" is required
// by Validate; "thinker" and "trivial" are looked up by Pick with a
// fallback to "executor" when a lane doesn't define them.
const (
	RoleExecutor = "executor"
	RoleThinker  = "thinker"
	RoleTrivial  = "trivial"
)

// Known runner kinds -- see runner.go's BuildCommand for what each one
// actually does.
const (
	RunnerClaude    = "claude"
	RunnerExec      = "exec"
	RunnerAidaAgent = "aida-agent"
)

// Known auth kinds. Validate enforces the plan's "a subscription-login
// lane may only run through the vendor's own CLI" rule (section 8, step
// 2) against these.
const (
	AuthSubscription = "subscription"
	AuthAPIKey       = "api-key"
)

// defaultMinHeadroomPct is Lane.MinHeadroomPct's fallback when unset (0).
const defaultMinHeadroomPct = 5.0

// defaultHandoffAtPct is Lane.HandoffAtPct's fallback when unset (0).
const defaultHandoffAtPct = 90.0

// anthropicRoutingEnvKeys are the environment variables a subscription-
// login lane must never set (Validate) -- setting any of them turns a
// `claude` invocation from the interactive OAuth/subscription path into
// an API-key or third-party-backend path, which is exactly what
// docs/arbiter-plan.md section 8 step 2 rules out for a subscription lane.
var anthropicRoutingEnvKeys = []string{
	"ANTHROPIC_BASE_URL",
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_API_KEY",
}

// Lane is one execution lane: a machine + runtime + credential, gated by
// an explicit data-class allowlist, ordered cheapest-first by Order.
// Field shapes and the six-lane default roster come from
// docs/arbiter-plan.md section 3 and the 2026-09-17 design conversation
// (see the build brief this package was written from).
type Lane struct {
	// ID is the lane's stable name (e.g. "claude-max"). Referenced by
	// signals.go's lane-state map and ledger.go's Entry.Lane.
	ID string `yaml:"id" json:"id"`
	// Order ranks lanes cheapest-first; Sorted() walks lanes in this
	// order and Pick returns the first eligible one. Every lane must
	// have a distinct Order (Validate).
	Order int `yaml:"order" json:"order"`
	// Provider must match a live roster provider's Label field exactly
	// (internal/models.Provider.Label, e.g. "Anthropic / Claude") --
	// that's how LaneCapacity matches this lane's windows against
	// burndown.Report's rows. Empty means "no provider probe" (e.g.
	// proxy-model's unprobed backend): only usable when AllowUnprobed is
	// true.
	Provider string `yaml:"provider,omitempty" json:"provider,omitempty"`
	// Windows are the burndown window keys (WindowKey's return value --
	// "5h", "7d", ...) this lane draws from. Empty together with Spend
	// true means "use the provider's spend rows instead" (a
	// dollar-denominated budget lane like litellm); empty with Spend
	// false means "no rate-limit window at all" (an unprobed lane like
	// proxy-model, which must set AllowUnprobed).
	Windows []string `yaml:"windows,omitempty" json:"windows,omitempty"`
	// Spend selects the provider's Spend rows (burndown.Capacity.IsSpend)
	// instead of its rate-limit Bar rows.
	Spend bool `yaml:"spend,omitempty" json:"spend,omitempty"`
	// DataClasses is this lane's explicit allowlist -- see the DataClass
	// consts' doc comment. At least one is required (Validate); no lane
	// may list both personal and butterstack (Validate).
	DataClasses []DataClass `yaml:"data_classes" json:"data_classes"`
	// Roles restricts which Pick roles this lane serves (e.g. gemini-agy
	// is thinker-only per the 2026-09-08 benchmark ruling it out for
	// agent loops). Empty means "serves every role".
	Roles []string `yaml:"roles,omitempty" json:"roles,omitempty"`
	// Runner selects how runner.go's BuildCommand assembles the command:
	// RunnerClaude, RunnerExec, or RunnerAidaAgent.
	Runner string `yaml:"runner" json:"runner"`
	// Auth is AuthSubscription or AuthAPIKey -- drives the Validate rules
	// in this file's doc comments above.
	Auth string `yaml:"auth" json:"auth"`
	// ConfigDir is a `claude` CLI config directory override
	// (CLAUDE_CONFIG_DIR, e.g. "~/.claude-company" for a second seat on
	// a company account) -- RunnerClaude only. An api-key lane must never set this
	// (Validate): CLAUDE_CONFIG_DIR selects an OAuth/subscription
	// identity, which has no meaning for an API-key credential.
	ConfigDir string `yaml:"config_dir,omitempty" json:"config_dir,omitempty"`
	// Env is extra environment this lane's command needs (e.g.
	// proxy-model's ANTHROPIC_BASE_URL pointing at a LiteLLM-shaped
	// proxy). A
	// subscription lane must never set any of anthropicRoutingEnvKeys
	// here (Validate).
	Env map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	// Command is the RunnerExec command template, one arg per element,
	// with "{model}" and "{prompt}" placeholders substituted by
	// runner.go's BuildCommand. Unused by RunnerClaude/RunnerAidaAgent.
	Command []string `yaml:"command,omitempty" json:"command,omitempty"`
	// Models maps a Pick role (RoleExecutor/RoleThinker/RoleTrivial) to
	// the model id this lane runs it with. RoleExecutor is required
	// (Validate); Pick falls back to Models[RoleExecutor] when a
	// requested role has no entry.
	Models map[string]string `yaml:"models" json:"models"`
	// MinHeadroomPct is the minimum burndown.Capacity.Headroom every one
	// of this lane's windows must clear for Pick to consider it eligible.
	// Zero means defaultMinHeadroomPct -- see EffectiveMinHeadroomPct.
	MinHeadroomPct float64 `yaml:"min_headroom_pct,omitempty" json:"min_headroom_pct,omitempty"`
	// AllowUnprobed lets a lane with no matching capacity rows (Provider
	// empty, or a provider burndown.Report never returned a row for) be
	// picked anyway -- the only way proxy-model's unprobed backend can
	// ever be eligible.
	AllowUnprobed bool `yaml:"allow_unprobed,omitempty" json:"allow_unprobed,omitempty"`
	// HandoffAtPct is the used-percent threshold NearExhaustion arms a
	// HANDOFF.md at (plan decision 1). Zero means defaultHandoffAtPct --
	// see EffectiveHandoffAtPct.
	HandoffAtPct float64 `yaml:"handoff_at_pct,omitempty" json:"handoff_at_pct,omitempty"`
	// Note is free-text context carried into examples/lanes.yaml and any
	// rendered lane listing; never read by Pick.
	Note string `yaml:"note,omitempty" json:"note,omitempty"`
}

// EffectiveMinHeadroomPct returns l.MinHeadroomPct, or
// defaultMinHeadroomPct when it's unset (0) -- 0 is never a meaningful
// configured floor (it would mean "any headroom above literally zero is
// fine"), so it doubles safely as the "not set" sentinel.
func (l Lane) EffectiveMinHeadroomPct() float64 {
	if l.MinHeadroomPct == 0 {
		return defaultMinHeadroomPct
	}
	return l.MinHeadroomPct
}

// EffectiveHandoffAtPct returns l.HandoffAtPct, or defaultHandoffAtPct
// when it's unset (0) -- see EffectiveMinHeadroomPct's rationale.
func (l Lane) EffectiveHandoffAtPct() float64 {
	if l.HandoffAtPct == 0 {
		return defaultHandoffAtPct
	}
	return l.HandoffAtPct
}

// ServesRole reports whether l serves the given Pick role -- an empty
// Roles list means "every role".
func (l Lane) ServesRole(role string) bool {
	if len(l.Roles) == 0 {
		return true
	}
	for _, r := range l.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// AllowsClass reports whether class is in l's DataClasses allowlist.
func (l Lane) AllowsClass(class DataClass) bool {
	for _, dc := range l.DataClasses {
		if dc == class {
			return true
		}
	}
	return false
}

// ModelFor returns l.Models[role], falling back to l.Models[RoleExecutor]
// when role has no entry -- the same fallback Pick applies when handing
// back a Decision.
func (l Lane) ModelFor(role string) string {
	if m, ok := l.Models[role]; ok && m != "" {
		return m
	}
	return l.Models[RoleExecutor]
}

// Config is the parsed shape of the lane roster (examples/lanes.yaml,
// normally installed at ~/.aida/lanes.yaml, config.Config.LanesPath).
type Config struct {
	Lanes []Lane `yaml:"lanes" json:"lanes"`
}

// Load reads and parses the lane roster at path. A missing file is an
// error, the same convention burndown.Load and models.Load both follow:
// this is small, hand-maintained state, so silently falling back to an
// empty roster would hide a real misconfiguration behind "no lane is ever
// eligible" instead of a clear error at startup.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("arbiter: reading lane config %q: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("arbiter: parsing lane config %q: %w", path, err)
	}
	return &c, nil
}

// Sorted returns c's lanes ordered cheapest-first by Order. Pick walks
// lanes in exactly this order.
func (c *Config) Sorted() []Lane {
	if c == nil {
		return nil
	}
	out := make([]Lane, len(c.Lanes))
	copy(out, c.Lanes)
	sort.Slice(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	return out
}

// Validate enforces the load-time invariants docs/arbiter-plan.md section
// 8 step 2 calls for ("Lane config validated at load time") plus the
// build brief's explicit rule list:
//
//   - lane IDs are unique
//   - every lane has a distinct Order (so Sorted's cheapest-first walk is
//     unambiguous)
//   - every lane names at least one DataClass
//   - Runner is one of the known runner kinds
//   - Models has a RoleExecutor entry
//   - no lane names both DataClassPersonal and DataClassButterstack
//   - a subscription-auth lane's Runner is RunnerClaude or RunnerExec,
//     and it never sets any of anthropicRoutingEnvKeys in Env (plan
//     section 8 step 2: "a subscription-login lane may only run through
//     the vendor's own CLI")
//   - an api-key-auth lane never sets ConfigDir
//
// A nil Config is valid (zero lanes) -- Pick on it always returns
// ErrNoLane, which is a legitimate "nothing configured yet" state, not a
// validation failure.
func (c *Config) Validate() error {
	if c == nil {
		return nil
	}
	seenID := make(map[string]bool, len(c.Lanes))
	seenOrder := make(map[int]string, len(c.Lanes))
	for _, l := range c.Lanes {
		if l.ID == "" {
			return fmt.Errorf("arbiter: lane has empty id")
		}
		if seenID[l.ID] {
			return fmt.Errorf("arbiter: duplicate lane id %q", l.ID)
		}
		seenID[l.ID] = true

		if other, ok := seenOrder[l.Order]; ok {
			return fmt.Errorf("arbiter: lane %q and %q share order %d, orders must be distinct so Sorted's cheapest-first walk is unambiguous", l.ID, other, l.Order)
		}
		seenOrder[l.Order] = l.ID

		if len(l.DataClasses) == 0 {
			return fmt.Errorf("arbiter: lane %q: at least one data class is required", l.ID)
		}
		hasPersonal, hasButterstack := false, false
		for _, dc := range l.DataClasses {
			switch dc {
			case DataClassPersonal:
				hasPersonal = true
			case DataClassButterstack:
				hasButterstack = true
			}
		}
		if hasPersonal && hasButterstack {
			return fmt.Errorf("arbiter: lane %q: may not carry both %s and %s data classes -- personal and company data never share a lane", l.ID, DataClassPersonal, DataClassButterstack)
		}

		if l.Runner != RunnerClaude && l.Runner != RunnerExec && l.Runner != RunnerAidaAgent {
			return fmt.Errorf("arbiter: lane %q: unknown runner %q", l.ID, l.Runner)
		}
		if l.Runner == RunnerExec && len(l.Command) == 0 {
			return fmt.Errorf("arbiter: lane %q: runner %q requires a command template", l.ID, RunnerExec)
		}

		if l.Models[RoleExecutor] == "" {
			return fmt.Errorf("arbiter: lane %q: models.%s is required", l.ID, RoleExecutor)
		}
		if strings.Contains(strings.ToLower(l.Models[RoleExecutor]), "fable") {
			return fmt.Errorf("arbiter: lane %q: models.%s must never be a Fable model; Fable is the interactive thinker and its weekly bar is protected by a hard floor (task #481, the 2026-09-13 sweep example)", l.ID, RoleExecutor)
		}

		switch l.Auth {
		case AuthSubscription:
			if l.Runner != RunnerClaude && l.Runner != RunnerExec {
				return fmt.Errorf("arbiter: lane %q: a subscription-login lane may only run through the vendor's own CLI (runner %s or %s), got %q", l.ID, RunnerClaude, RunnerExec, l.Runner)
			}
			for _, k := range anthropicRoutingEnvKeys {
				if _, ok := l.Env[k]; ok {
					return fmt.Errorf("arbiter: lane %q: subscription-login lane may not set %s (plan section 8 step 2)", l.ID, k)
				}
			}
		case AuthAPIKey:
			if l.ConfigDir != "" {
				return fmt.Errorf("arbiter: lane %q: api-key lane may not set config_dir", l.ID)
			}
		default:
			return fmt.Errorf("arbiter: lane %q: unknown auth %q", l.ID, l.Auth)
		}
	}
	return nil
}

// DefaultConfig returns a six-lane PLACEHOLDER roster shaped like
// docs/arbiter-plan.md section 3, cheapest first, in the shape
// examples/lanes.yaml mirrors exactly (there is a round-trip test
// asserting the two never drift apart). It is a vendor-shaped example
// to copy and edit, not any particular household's real lanes -- it
// names no machine, no company, and no person. A real roster is
// hand-maintained at ~/.aida/lanes.yaml (config.Config.LanesPath) and
// Load requires that file to exist at runtime (see Load's doc comment);
// DefaultConfig stays exported only for tests and for `aida arbiter
// lanes --example`.
func DefaultConfig() *Config {
	return &Config{
		Lanes: []Lane{
			{
				ID:          "claude-personal",
				Order:       1,
				Provider:    "Anthropic / Claude",
				Windows:     []string{"5h", "7d"},
				DataClasses: []DataClass{DataClassPersonal, DataClassGames, DataClassPublic},
				Runner:      RunnerClaude,
				Auth:        AuthSubscription,
				Models: map[string]string{
					RoleExecutor: "claude-sonnet-5",
					RoleThinker:  "claude-opus-5",
					RoleTrivial:  "claude-haiku-4-5-20251001",
				},
				Note: "Example: a personal subscription plan, claude CLI, personal config dir. Cheapest lane -- always tried first.",
			},
			{
				ID:          "claude-company",
				Order:       2,
				Provider:    "Anthropic / Claude (company seat)",
				Windows:     []string{"5h", "7d"},
				DataClasses: []DataClass{DataClassButterstack, DataClassPublic},
				Runner:      RunnerClaude,
				Auth:        AuthSubscription,
				ConfigDir:   "~/.claude-company",
				Models: map[string]string{
					RoleExecutor: "claude-sonnet-5",
					RoleThinker:  "claude-opus-5",
				},
				Note: "Example: a second Claude seat on a company account, claude CLI with CLAUDE_CONFIG_DIR=~/.claude-company. Primary company-data lane.",
			},
			{
				ID:          "gemini",
				Order:       3,
				Provider:    "Google / Gemini",
				Windows:     []string{"5h", "7d"},
				DataClasses: []DataClass{DataClassPersonal, DataClassGames, DataClassPublic},
				Runner:      RunnerExec,
				Auth:        AuthSubscription,
				Command:     []string{"agy", "-i", "--model", "{model}", "{prompt}"},
				Models: map[string]string{
					RoleExecutor: "gemini-3.8-flash",
					RoleThinker:  "gemini-3.1-pro",
				},
				Note: "Example: agy -i (never -p, 5-minute cap). Serves every role -- the 2026-09-08 benchmark found Flash weak in agent loops, so this default re-tests that rather than assuming it still holds.",
			},
			{
				ID:          "codex",
				Order:       4,
				Provider:    "OpenAI / ChatGPT + Codex",
				Windows:     []string{"5h", "7d"},
				DataClasses: []DataClass{DataClassPersonal, DataClassGames, DataClassPublic},
				Runner:      RunnerExec,
				Auth:        AuthSubscription,
				Command:     []string{"codex", "exec", "-s", "workspace-write", "--model", "{model}", "{prompt}"},
				Models: map[string]string{
					RoleExecutor: "gpt-5.6-terra",
					RoleThinker:  "gpt-6-sol",
				},
				Note: "Example: codex exec -s workspace-write, stdin closed. Banked resets are manual levers, not automatic.",
			},
			{
				ID:            "proxy-model",
				Order:         5,
				DataClasses:   []DataClass{DataClassButterstack, DataClassGames, DataClassPublic},
				Runner:        RunnerClaude,
				Auth:          AuthAPIKey,
				AllowUnprobed: true,
				Env: map[string]string{
					"ANTHROPIC_BASE_URL": "http://localhost:4000",
				},
				Models: map[string]string{
					RoleExecutor: "qwen",
					RoleThinker:  "qwen",
				},
				Note: "Example: a model behind a local API-key proxy (a self-hosted model, a second vendor, whatever). No live probe -- allow_unprobed is the only way this lane is ever eligible.",
			},
			{
				ID:          "litellm",
				Order:       6,
				Provider:    "LiteLLM proxy",
				Spend:       true,
				DataClasses: []DataClass{DataClassButterstack, DataClassGames, DataClassPublic},
				Runner:      RunnerAidaAgent,
				Auth:        AuthAPIKey,
				Models: map[string]string{
					RoleExecutor: "claude-sonnet-5",
					RoleThinker:  "claude-opus-5",
				},
				Note: "Example: real dollars, last. aida --agent via LiteLLM budget keys. Never carries personal data.",
			},
		},
	}
}
