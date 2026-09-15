package models

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/execx"
)

type geminiSettings struct {
	Security struct {
		Auth struct {
			SelectedType string `json:"selectedType"`
		} `json:"auth"`
	} `json:"security"`
}

type geminiAccounts struct {
	Active string `json:"active"`
}

// geminiLocalDetail reads local Gemini CLI auth/version state --
// settings.json's selected OAuth auth type, google_accounts.json's
// active account, and `gemini --version` -- with no live quota call
// involved. Shared by probeGeminiLocal (its only output) and
// probeAgyQuota (folded into that probe's own Detail), so switching the
// google provider's `probe:` between the two in the roster never loses
// this local state. Every field is best-effort: a missing/unreadable
// file or binary just means that Detail key is absent.
func geminiLocalDetail(ctx context.Context) map[string]string {
	detail := map[string]string{}

	if home, err := os.UserHomeDir(); err == nil {
		if data, rerr := os.ReadFile(filepath.Join(home, ".gemini", "settings.json")); rerr == nil {
			var s geminiSettings
			if json.Unmarshal(data, &s) == nil && s.Security.Auth.SelectedType != "" {
				detail["auth_type"] = s.Security.Auth.SelectedType
			}
		}
		if data, rerr := os.ReadFile(filepath.Join(home, ".gemini", "google_accounts.json")); rerr == nil {
			var a geminiAccounts
			if json.Unmarshal(data, &a) == nil && a.Active != "" {
				detail["active_account"] = a.Active
			}
		}
	}

	if res, verr := execx.Run(ctx, "gemini", []string{"--version"}, execx.RunOpts{Timeout: 3 * time.Second}); verr == nil && res.ExitCode == 0 {
		if v := strings.TrimSpace(string(res.Stdout)); v != "" {
			detail["gemini_version"] = v
		}
	}

	return detail
}

// probeGeminiLocal is the gemini-local prober. There is no scriptable
// quota endpoint through the Gemini CLI itself (Code Assist's
// loadCodeAssist/retrieveUserQuota needs the CLI's own OAuth refresh
// flow -- not available out-of-process as of this probe's writing; see
// probeAgyQuota for the agy-based probe that DOES have one), so this
// only reports geminiLocalDetail and leaves Bars empty. Err is
// deliberately left "": a provider with no live-usage probe wired up is
// not a probe failure, it's a documented gap (see Detail["quota"]).
func probeGeminiLocal(ctx context.Context, p Provider) Usage {
	u := Usage{Provider: p.Name, Plan: p.Plan, Account: p.Account, ProbedAt: time.Now()}
	detail := geminiLocalDetail(ctx)
	detail["quota"] = "not scriptable yet (Code Assist retrieveUserQuota)"
	u.Detail = detail
	return u
}
