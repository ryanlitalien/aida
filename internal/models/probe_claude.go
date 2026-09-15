package models

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/ryanlitalien/aida/internal/execx"
)

// claudeUsageEndpoint is Anthropic's OAuth usage API. Requires the same
// bearer token Claude Code itself uses (from the macOS keychain, or a
// ClaudeConfig override) plus the oauth beta header -- there is no other
// way to read a subscription account's rate-limit bars.
const claudeUsageEndpoint = "https://api.anthropic.com/api/oauth/usage"

// claudeKeychainCreds is the shape of both the macOS keychain item
// "Claude Code-credentials" and a Claude Code credentials.json file on
// disk (ClaudeConfig.CredentialsFile) -- same JSON either way.
type claudeKeychainCreds struct {
	ClaudeAiOauth struct {
		AccessToken      string `json:"accessToken"`
		SubscriptionType string `json:"subscriptionType"`
		RateLimitTier    string `json:"rateLimitTier"`
		ExpiresAt        int64  `json:"expiresAt"` // unix millis
	} `json:"claudeAiOauth"`
}

// errClaudeUnauthorized distinguishes "the usage endpoint rejected the
// token" (a "no key" condition -- the token is stale/revoked) from any
// other transport or server failure.
var errClaudeUnauthorized = errors.New("claude usage endpoint: unauthorized")

// probeClaudeOAuth is the claude-oauth prober: read the account's OAuth
// credentials (this machine's keychain by default, or p.ClaudeConfig's
// override), then GET the usage endpoint and map its limits[] into Bars.
//
// Never logs, stores, or returns the access token itself -- it's used
// only as the Authorization header on the one outbound request.
func probeClaudeOAuth(ctx context.Context, p Provider) Usage {
	u := Usage{Provider: p.Name, Plan: p.Plan, Account: p.Account, ProbedAt: time.Now()}

	creds, err := loadClaudeCredentials(ctx, p.ClaudeConfig)
	if err != nil || creds.ClaudeAiOauth.AccessToken == "" {
		if p.ClaudeConfig != nil && p.ClaudeConfig.CredentialsFile != "" {
			u.Err = fmt.Sprintf("no key (credentials file unreadable or missing accessToken: %s)", p.ClaudeConfig.CredentialsFile)
		} else {
			u.Err = "no key (Claude Code not logged in on this machine)"
		}
		return u
	}

	detail := map[string]string{}
	if creds.ClaudeAiOauth.SubscriptionType != "" {
		detail["subscription_type"] = creds.ClaudeAiOauth.SubscriptionType
	}
	if creds.ClaudeAiOauth.RateLimitTier != "" {
		detail["rate_limit_tier"] = creds.ClaudeAiOauth.RateLimitTier
	}
	// ~/.claude.json's own org fields are best-effort, and only meaningful
	// for THIS machine's own login -- a remote ClaudeConfig account has
	// its own, unreachable ~/.claude.json on the far side.
	if p.ClaudeConfig == nil {
		for k, v := range readClaudeDotJSONDetail() {
			detail[k] = v
		}
	}
	if len(detail) > 0 {
		u.Detail = detail
	}

	// An expired token is a "no key" outcome, not a network error: the
	// usage endpoint answers 401 or 429 to it, and the fix is to run
	// Claude Code on that machine once so it refreshes.
	if exp := creds.ClaudeAiOauth.ExpiresAt; exp > 0 && time.UnixMilli(exp).Before(time.Now()) {
		where := "this machine"
		if p.ClaudeConfig != nil && p.ClaudeConfig.SSHHost != "" {
			where = p.ClaudeConfig.SSHHost
		}
		u.Err = fmt.Sprintf("no key (token expired %s ago on %s; run claude there once to refresh)",
			time.Since(time.UnixMilli(exp)).Round(time.Minute), where)
		return u
	}

	resp, err := fetchClaudeUsage(ctx, creds.ClaudeAiOauth.AccessToken)
	if err != nil {
		if errors.Is(err, errClaudeUnauthorized) {
			u.Err = "no key (Claude Code token expired or invalid)"
		} else {
			u.Err = "error: " + err.Error()
		}
		return u
	}

	bars := mapClaudeUsage(resp)
	if len(bars) == 0 {
		u.Err = "no data"
		return u
	}
	u.Bars = bars
	return u
}

// loadClaudeCredentials resolves credentials per cfg: nil (or an unset
// CredentialsFile) reads this machine's own keychain; a set
// CredentialsFile reads that file, locally or (when SSHHost is also set)
// over ssh.
func loadClaudeCredentials(ctx context.Context, cfg *ClaudeConfig) (claudeKeychainCreds, error) {
	if cfg == nil || cfg.CredentialsFile == "" {
		return readClaudeKeychainCreds(ctx)
	}
	if cfg.SSHHost != "" {
		return readClaudeCredentialsViaSSH(ctx, cfg.SSHHost, cfg.CredentialsFile)
	}
	return readClaudeCredentialsFromFile(cfg.CredentialsFile)
}

// readClaudeKeychainCreds reads the "Claude Code-credentials" item from
// the macOS keychain -- the credential source for this machine's own
// `claude /login` session.
func readClaudeKeychainCreds(ctx context.Context) (claudeKeychainCreds, error) {
	res, err := execx.Run(ctx, "security", []string{"find-generic-password", "-s", "Claude Code-credentials", "-w"}, execx.RunOpts{})
	if err != nil {
		return claudeKeychainCreds{}, err
	}
	if res.ExitCode != 0 {
		return claudeKeychainCreds{}, fmt.Errorf("security find-generic-password exited %d", res.ExitCode)
	}
	return parseClaudeCreds(res.Stdout)
}

// readClaudeCredentialsFromFile reads a Claude Code credentials.json off
// local disk (no keychain involved) -- used when ClaudeConfig names a
// file on THIS machine.
func readClaudeCredentialsFromFile(path string) (claudeKeychainCreds, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return claudeKeychainCreds{}, err
	}
	return parseClaudeCreds(data)
}

// readClaudeCredentialsViaSSH fetches a remote credentials.json via
// `ssh <host> cat <path>`, held only in memory -- never written to disk
// or logged on either side of the trip.
func readClaudeCredentialsViaSSH(ctx context.Context, host, path string) (claudeKeychainCreds, error) {
	res, err := execx.Run(ctx, "ssh", []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=5", host, "cat", path}, execx.RunOpts{})
	if err != nil {
		return claudeKeychainCreds{}, err
	}
	if res.ExitCode != 0 {
		return claudeKeychainCreds{}, fmt.Errorf("ssh %s cat %s exited %d", host, path, res.ExitCode)
	}
	return parseClaudeCreds(res.Stdout)
}

func parseClaudeCreds(raw []byte) (claudeKeychainCreds, error) {
	var creds claudeKeychainCreds
	if err := json.Unmarshal(bytes.TrimSpace(raw), &creds); err != nil {
		return claudeKeychainCreds{}, fmt.Errorf("parsing claude credentials: %w", err)
	}
	return creds, nil
}

// claudeDotJSON is the slice of ~/.claude.json this probe reads: the
// resolved plan/org tier for the account this machine is logged into.
type claudeDotJSON struct {
	OauthAccount struct {
		OrganizationType          string `json:"organizationType"`
		OrganizationRateLimitTier string `json:"organizationRateLimitTier"`
	} `json:"oauthAccount"`
}

// readClaudeDotJSONDetail is best-effort: a missing or unparsable
// ~/.claude.json just means those two Detail keys are absent, never an
// error for the probe as a whole.
func readClaudeDotJSONDetail() map[string]string {
	detail := map[string]string{}
	home, err := os.UserHomeDir()
	if err != nil {
		return detail
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		return detail
	}
	var cfg claudeDotJSON
	if err := json.Unmarshal(data, &cfg); err != nil {
		return detail
	}
	if cfg.OauthAccount.OrganizationType != "" {
		detail["organization_type"] = cfg.OauthAccount.OrganizationType
	}
	if cfg.OauthAccount.OrganizationRateLimitTier != "" {
		detail["organization_rate_limit_tier"] = cfg.OauthAccount.OrganizationRateLimitTier
	}
	return detail
}

// claudeUsageLimit is one entry of the usage endpoint's limits[] array.
type claudeUsageLimit struct {
	Kind     string  `json:"kind"`
	Group    string  `json:"group"`
	Percent  float64 `json:"percent"`
	Severity string  `json:"severity"`
	ResetsAt string  `json:"resets_at"`
	IsActive bool    `json:"is_active"`
	Scope    struct {
		Model struct {
			DisplayName string `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
}

// claudeUsageWindow is the five_hour/seven_day rollup shape; parsed but
// not currently rendered -- limits[] already carries the same
// information per-window with resets_at and severity attached, which is
// what mapClaudeUsage uses.
type claudeUsageWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at"`
}

type claudeUsageResponse struct {
	Limits   []claudeUsageLimit `json:"limits"`
	FiveHour *claudeUsageWindow `json:"five_hour"`
	SevenDay *claudeUsageWindow `json:"seven_day"`
}

// fetchClaudeUsage is the one network call this prober makes, factored
// out from mapClaudeUsage so the mapping logic is unit-testable against
// a canned claudeUsageResponse with no network involved.
func fetchClaudeUsage(ctx context.Context, token string) (*claudeUsageResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, claudeUsageEndpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errClaudeUnauthorized
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("usage endpoint rate-limited this token (429); retry in a minute")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("usage endpoint returned %d", resp.StatusCode)
	}

	var out claudeUsageResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("parsing usage response: %w", err)
	}
	return &out, nil
}

// claudeBarLabel maps a limits[] entry's kind (+ its scoped model's
// display name, when scoped) to the label shown on the CLI/dashboard:
// "session" -> "5-hour", "weekly_all" -> "7-day", "weekly_scoped" ->
// "7-day <model>". Any other kind is shown verbatim rather than dropped,
// so a new limit kind the API adds later still renders as *something*.
func claudeBarLabel(kind, scopeModel string) string {
	switch kind {
	case "session":
		return "5-hour"
	case "weekly_all":
		return "7-day"
	case "weekly_scoped":
		if scopeModel != "" {
			return "7-day " + scopeModel
		}
		return "7-day"
	default:
		return kind
	}
}

// claudeBarWindowMins maps a limits[] entry's kind to its window
// duration in minutes -- the same three kinds claudeBarLabel names:
// "session" is a 5-hour window, "weekly_all" and "weekly_scoped" are
// both 7-day windows (a model-scoped bar narrows WHICH usage counts
// toward the cap, not how long the window is). Any other kind returns 0
// ("unknown"), which PaceFor and every other pace computation treats as
// "no verdict" rather than guessing.
func claudeBarWindowMins(kind string) float64 {
	switch kind {
	case "session":
		return 300
	case "weekly_all", "weekly_scoped":
		return 10080
	default:
		return 0
	}
}

// claudeSeverity prefers the API's own severity string when it's one of
// the three this package recognizes; otherwise it's derived from percent
// the same way every other probe's Bar is.
func claudeSeverity(severity string, percent float64) string {
	switch severity {
	case SeverityNormal, SeverityWarning, SeverityCritical:
		return severity
	default:
		return severityForPercent(percent)
	}
}

// mapClaudeUsage is the pure mapping step -- no network, no I/O -- unit
// tested directly against canned claudeUsageResponse values.
func mapClaudeUsage(resp *claudeUsageResponse) []Bar {
	if resp == nil {
		return nil
	}
	bars := make([]Bar, 0, len(resp.Limits))
	for _, l := range resp.Limits {
		bars = append(bars, Bar{
			Label:      claudeBarLabel(l.Kind, l.Scope.Model.DisplayName),
			Percent:    l.Percent,
			ResetsAt:   parseTimeLoose(l.ResetsAt),
			Severity:   claudeSeverity(l.Severity, l.Percent),
			Active:     l.IsActive,
			WindowMins: claudeBarWindowMins(l.Kind),
		})
	}
	return bars
}
