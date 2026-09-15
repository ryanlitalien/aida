package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/execx"
)

// probeLiteLLM is the litellm prober: resolve the proxy's master key
// (env var first, then an ssh fallback that greps it out of the proxy's
// own env file), then GET /key/list and map each virtual key into a
// Spend row.
func probeLiteLLM(ctx context.Context, p Provider) Usage {
	u := Usage{Provider: p.Name, Plan: p.Plan, Account: p.Account, ProbedAt: time.Now()}

	if p.LiteLLM == nil {
		u.Err = "no key (litellm block missing from roster)"
		return u
	}

	key, err := resolveLiteLLMKey(ctx, p.LiteLLM)
	if err != nil || key == "" {
		u.Err = "no key (LITELLM_MASTER_KEY unset, ssh fallback failed)"
		return u
	}

	var resp *litellmKeyListResponse
	var lastErr error
	for _, base := range []string{p.LiteLLM.URL, p.LiteLLM.LANURL} {
		if base == "" {
			continue
		}
		resp, lastErr = fetchLiteLLMKeys(ctx, base, key)
		if lastErr == nil {
			break
		}
	}
	if resp == nil {
		if lastErr == nil {
			lastErr = errors.New("no litellm url configured")
		}
		u.Err = "error: " + lastErr.Error()
		return u
	}

	spend := mapLiteLLMSpend(resp)
	if len(spend) == 0 {
		u.Err = "no data"
		return u
	}
	u.Spend = spend
	return u
}

// resolveLiteLLMKey obtains the proxy's master key without ever writing
// it to disk or logging it: cfg.KeyEnv locally first, then -- when that's
// unset and an ssh fallback is configured -- an ssh one-liner that greps
// it out of the proxy's own env file on cfg.SSHHost, held only in the
// returned string.
func resolveLiteLLMKey(ctx context.Context, cfg *LiteLLMConfig) (string, error) {
	if cfg == nil {
		return "", errors.New("no litellm config")
	}
	if cfg.KeyEnv != "" {
		if v := os.Getenv(cfg.KeyEnv); v != "" {
			return v, nil
		}
	}
	if cfg.SSHHost == "" || cfg.SSHEnvFile == "" {
		return "", errors.New("no key source configured")
	}

	grepCmd := fmt.Sprintf(`grep -o "^LITELLM_MASTER_KEY=.*" %s | cut -d= -f2-`, cfg.SSHEnvFile)
	res, err := execx.Run(ctx, "ssh", []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=5", cfg.SSHHost, grepCmd}, execx.RunOpts{})
	if err != nil {
		return "", fmt.Errorf("ssh fallback: %w", err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("ssh fallback exited %d", res.ExitCode)
	}
	key := strings.TrimSpace(string(res.Stdout))
	if key == "" {
		return "", errors.New("ssh fallback returned an empty key")
	}
	return key, nil
}

// litellmKeyEntry is one entry of GET /key/list's keys[] array.
type litellmKeyEntry struct {
	KeyAlias       string   `json:"key_alias"`
	Spend          float64  `json:"spend"`
	MaxBudget      *float64 `json:"max_budget"`
	BudgetDuration string   `json:"budget_duration"`
	BudgetResetAt  string   `json:"budget_reset_at"`
	Models         []string `json:"models"`
}

type litellmKeyListResponse struct {
	Keys []litellmKeyEntry `json:"keys"`
}

// fetchLiteLLMKeys is the one network call this prober makes, factored
// out from mapLiteLLMSpend so the mapping logic is unit-testable against
// canned JSON with no network involved.
func fetchLiteLLMKeys(ctx context.Context, baseURL, masterKey string) (*litellmKeyListResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/key/list?return_full_object=true", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+masterKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("key/list returned %d", resp.StatusCode)
	}
	var out litellmKeyListResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("parsing key/list response: %w", err)
	}
	return &out, nil
}

// mapLiteLLMSpend is the pure mapping step (keys[] -> Spend rows), unit
// tested directly.
func mapLiteLLMSpend(resp *litellmKeyListResponse) []Spend {
	if resp == nil {
		return nil
	}
	out := make([]Spend, 0, len(resp.Keys))
	for _, k := range resp.Keys {
		var budget float64
		if k.MaxBudget != nil {
			budget = *k.MaxBudget
		}
		windowMins, _ := litellmBudgetDurationMins(k.BudgetDuration)
		out = append(out, Spend{
			Key:        k.KeyAlias,
			Spend:      k.Spend,
			Budget:     budget,
			ResetsAt:   parseTimeLoose(k.BudgetResetAt),
			Models:     k.Models,
			WindowMins: windowMins,
		})
	}
	return out
}

// litellmDurationRe matches LiteLLM's own budget_duration syntax: a
// count followed by one of its recognized units -- "s"/"m"/"h"/"d" for
// seconds/minutes/hours/days, "mo" for a 30-day month, "y" for a 365-day
// year. These are LiteLLM's own calendar approximations, not a real
// calendar month/year -- matching what the API actually says a key's
// period is, rather than deriving one from resets_at (e.g. "reset time
// minus one calendar month"), which would be wrong for the "30d" keys
// this proxy actually issues (see probe_litellm_test.go's fixture).
var litellmDurationRe = regexp.MustCompile(`^(\d+)(mo|[smhdy])$`)

// litellmBudgetDurationMins converts a LiteLLM budget_duration string
// into minutes for Spend.WindowMins. Returns (0, false) for anything it
// doesn't recognize -- an unparsed duration leaves WindowMins at 0
// ("unknown"), so paceForSpend never guesses a period it can't confirm.
func litellmBudgetDurationMins(s string) (float64, bool) {
	m := litellmDurationRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	switch m[2] {
	case "s":
		return n / 60, true
	case "m":
		return n, true
	case "h":
		return n * 60, true
	case "d":
		return n * 24 * 60, true
	case "mo":
		return n * 30 * 24 * 60, true
	case "y":
		return n * 365 * 24 * 60, true
	default:
		return 0, false
	}
}
