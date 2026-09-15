// Package models is the AI provider/plan/nickname roster ("which models can
// I use, on which plan, and what nickname means what") plus live usage
// probing. The roster itself lives at ~/.aida/models.yaml (config.Config's
// ModelsPath); this package only reads it, and only probes local state
// (keychain, session logs, a proxy's own API) -- it never writes the
// roster. See `aida models`, GET /api/models, and the Models panel on
// /dashboard, all of which are thin views over Load + Resolve + Probe.
package models

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Roster is the parsed shape of ~/.aida/models.yaml.
type Roster struct {
	Updated string `yaml:"updated" json:"updated"`
	// Guidance is a dated, free-text list of standing model-choice rules
	// ("use Sonnet for coding agents, Codex for judgement calls, ...").
	// The newest line wins on a conflict -- that's a convention enforced
	// by whoever edits the file, not by this package.
	Guidance  []string   `yaml:"guidance,omitempty" json:"guidance,omitempty"`
	Providers []Provider `yaml:"providers" json:"providers"`
}

// Provider is one AI provider entry: a plan, an account, how to probe its
// live usage, and the models available on it.
type Provider struct {
	Name     string   `yaml:"name" json:"name"`
	Label    string   `yaml:"label" json:"label"`
	Plan     string   `yaml:"plan" json:"plan"`
	Account  string   `yaml:"account,omitempty" json:"account,omitempty"`
	Billing  string   `yaml:"billing,omitempty" json:"billing,omitempty"`
	Surfaces []string `yaml:"surfaces,omitempty" json:"surfaces,omitempty"`
	// Probe selects which live-usage prober to run: "claude-oauth",
	// "codex-sessions", "litellm", "gemini-local", "none", or anything
	// else unrecognized -- all of the latter degrade to a no-op probe
	// (empty Usage, no error) rather than failing.
	Probe  string   `yaml:"probe" json:"probe"`
	Limits []string `yaml:"limits,omitempty" json:"limits,omitempty"`
	Notes  []string `yaml:"notes,omitempty" json:"notes,omitempty"`
	Models []Model  `yaml:"models,omitempty" json:"models,omitempty"`

	// LiteLLM configures the litellm probe: where the proxy lives and how
	// to get its master key. Nil for every other probe kind.
	LiteLLM *LiteLLMConfig `yaml:"litellm,omitempty" json:"litellm,omitempty"`
	// ClaudeConfig configures the claude-oauth probe for an account that
	// is NOT this machine's own `/login` session -- a credentials file
	// (optionally read over ssh) instead of the local macOS keychain. Nil
	// means "use this machine's keychain," the default and original path.
	ClaudeConfig *ClaudeConfig `yaml:"claude,omitempty" json:"claude,omitempty"`
}

// Model is one model available on a Provider.
type Model struct {
	ID string `yaml:"id" json:"id"`
	// Nicknames are the spoken/typed variants Resolve matches against, in
	// addition to ID itself. The first entry is the display nickname used
	// by `aida models`' compact "models:" line.
	Nicknames []string `yaml:"nicknames,omitempty" json:"nicknames,omitempty"`
	Tier      string   `yaml:"tier,omitempty" json:"tier,omitempty"`
	Note      string   `yaml:"note,omitempty" json:"note,omitempty"`
}

// LiteLLMConfig is the litellm probe's target + credential source.
type LiteLLMConfig struct {
	URL    string `yaml:"url,omitempty" json:"url,omitempty"`
	LANURL string `yaml:"lan_url,omitempty" json:"lan_url,omitempty"`
	KeyEnv string `yaml:"key_env,omitempty" json:"key_env,omitempty"`
	// SSHHost + SSHEnvFile are the fallback path when KeyEnv is unset
	// locally: ssh in and grep the master key out of the proxy's env
	// file, in memory only.
	SSHHost    string `yaml:"ssh_host,omitempty" json:"ssh_host,omitempty"`
	SSHEnvFile string `yaml:"ssh_env_file,omitempty" json:"ssh_env_file,omitempty"`
}

// ClaudeConfig overrides the claude-oauth probe's credential source for a
// Claude account that isn't logged into this machine's own Claude Code
// (e.g. a company account only ever run on a remote box).
type ClaudeConfig struct {
	// CredentialsFile is the path to a Claude Code credentials.json (same
	// shape as the macOS keychain item: .claudeAiOauth.accessToken /
	// .subscriptionType / .rateLimitTier). Read locally, or over ssh when
	// SSHHost is also set.
	CredentialsFile string `yaml:"credentials_file,omitempty" json:"credentials_file,omitempty"`
	// SSHHost, when set, means CredentialsFile lives on that remote host,
	// not on this machine -- fetched via `ssh <host> cat <file>`, held in
	// memory only.
	SSHHost string `yaml:"ssh_host,omitempty" json:"ssh_host,omitempty"`
}

// Load reads and parses the models roster at path (config.Config's
// ModelsPath, normally ~/.aida/models.yaml). Unlike some other aida
// registries, a missing file IS an error here: the roster is small,
// hand-maintained, and expected to exist once this feature is in use, so
// silently returning an empty roster would hide a real misconfiguration
// (a wrong --path, a machine that never got the file synced) behind an
// empty-looking `aida models` instead of a clear error.
func Load(path string) (*Roster, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading models roster %q: %w", path, err)
	}
	var r Roster
	if err := yaml.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parsing models roster %q: %w", path, err)
	}
	return &r, nil
}

// Match is one hit from Resolve: the provider + model a nickname resolved
// to, carrying enough of the Model's own metadata (Tier, Note) that a
// caller can print a useful line without a second lookup.
type Match struct {
	Provider string `json:"provider"`
	ModelID  string `json:"model_id"`
	Tier     string `json:"tier,omitempty"`
	Note     string `json:"note,omitempty"`
}

// normalizeNickname lowercases s and strips spaces, hyphens, dots, and
// underscores, so "gpt-sol", "GPT Sol", "gpt.sol", and "gpt_sol" all
// compare equal. Applied to both sides of every Resolve comparison
// (query, model ID, and every nickname), which is what makes an exact
// model ID like "claude-fable-5-1" resolve too, alongside its nicknames.
func normalizeNickname(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case ' ', '-', '.', '_':
			continue
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Resolve looks up nickname (or an exact model ID) against every model on
// every provider, case-insensitively and punctuation-tolerant (see
// normalizeNickname). It can return more than one Match -- the same
// nickname ("sonnet", "opus") is deliberately reused across several
// providers/accounts in the roster, and the caller (CLI/voice) decides
// what to do with more than one hit.
func (r *Roster) Resolve(nickname string) []Match {
	if r == nil {
		return nil
	}
	q := normalizeNickname(nickname)
	if q == "" {
		return nil
	}
	var matches []Match
	for _, p := range r.Providers {
		for _, m := range p.Models {
			if normalizeNickname(m.ID) == q {
				matches = append(matches, Match{Provider: p.Name, ModelID: m.ID, Tier: m.Tier, Note: m.Note})
				continue
			}
			for _, nick := range m.Nicknames {
				if normalizeNickname(nick) == q {
					matches = append(matches, Match{Provider: p.Name, ModelID: m.ID, Tier: m.Tier, Note: m.Note})
					break
				}
			}
		}
	}
	return matches
}
