package models

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/execx"
)

// codexMaxSessionFiles caps how many of the newest rollout-*.jsonl files
// findLatestCodexRateLimits will open looking for a rate_limits event,
// so a machine with years of Codex history never turns this probe into a
// slow directory-wide scan.
const codexMaxSessionFiles = 20

// aidaClientVersion reads this binary's own module version (via
// runtime/debug, the same mechanism internal/cli's buildVersion uses --
// internal/models can't import internal/cli without a cycle, so this is
// a small local echo of it) for the app-server RPC's clientInfo.version
// field. Purely informational; "0.1" is a fine fallback when build info
// isn't available (e.g. a `go test` binary).
func aidaClientVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		v := strings.TrimSuffix(info.Main.Version, "+dirty")
		if v != "" && v != "(devel)" {
			return v
		}
	}
	return "0.1"
}

// probeCodexSessions is the codex-sessions prober. Live usage bars come
// first from a call to the Codex app-server over stdio JSON-RPC
// (probeCodexAppServerRateLimits) -- verified working directly against
// `codex app-server` -- and, only when that fails (codex missing, a
// timeout, a malformed reply), fall back to scanning the newest session
// rollout file that logged a rate_limits event. Detail["source"] records
// which path produced the bars. The account's plan is read from the
// id_token JWT in ~/.codex/auth.json either way -- that's also this
// probe's "no key" gate (not logged in locally), checked before either
// bars source is attempted.
func probeCodexSessions(ctx context.Context, p Provider) Usage {
	u := Usage{Provider: p.Name, Plan: p.Plan, Account: p.Account, ProbedAt: time.Now()}

	home, err := os.UserHomeDir()
	if err != nil {
		u.Err = "error: resolving home directory: " + err.Error()
		return u
	}
	codexDir := filepath.Join(home, ".codex")

	plan, err := readCodexPlan(filepath.Join(codexDir, "auth.json"))
	if err != nil {
		u.Err = "no key (Codex CLI not logged in on this machine)"
		return u
	}
	if plan != "" {
		u.Plan = plan
	}

	detail := readCodexConfigDetail(filepath.Join(codexDir, "config.toml"))
	if v := readCodexVersion(ctx); v != "" {
		detail["codex_version"] = v
	}
	known := map[string]bool{}
	for _, m := range p.Models {
		known[strings.ToLower(m.ID)] = true
	}
	if unlisted := unlistedCodexModels(filepath.Join(codexDir, "models_cache.json"), known); unlisted != "" {
		detail["unlisted_models"] = unlisted
	}

	// Live RPC first.
	if rl, resetCredits, rpcErr := probeCodexAppServerRateLimits(ctx, aidaClientVersion()); rpcErr == nil {
		if rl.PlanType != "" {
			u.Plan = rl.PlanType
			detail["chatgpt_plan_type_live"] = rl.PlanType
		}
		if rl.Credits != nil {
			detail["credits_balance"] = strings.Trim(string(rl.Credits.Balance), `"`)
		}
		detail["reset_credits"] = strconv.Itoa(resetCredits)
		detail["source"] = "app-server"
		u.Detail = detail
		u.Bars = mapCodexBars(rl)
		return u
	}

	// Fall back to the session-log scan.
	detail["source"] = "session-log (app-server unavailable)"

	rl, err := findLatestCodexRateLimits(filepath.Join(codexDir, "sessions"))
	if err != nil {
		u.Detail = detail
		u.Err = "error: scanning codex sessions: " + err.Error()
		return u
	}
	if rl == nil {
		u.Detail = detail
		u.Err = "no data (no recent Codex session with rate_limit info)"
		return u
	}

	if rl.PlanType != "" {
		detail["chatgpt_plan_type_live"] = rl.PlanType
	}
	if rl.Credits != nil {
		detail["credits_balance"] = strings.Trim(string(rl.Credits.Balance), `"`)
	}
	u.Detail = detail
	u.Bars = mapCodexBars(rl)
	return u
}

// ---- plan (id_token JWT) ----

type codexAuthFile struct {
	// Codex 0.15x nests the OAuth tokens under "tokens"; older builds
	// wrote id_token at the top level. Accept both.
	IDToken string `json:"id_token"`
	Tokens  struct {
		IDToken string `json:"id_token"`
	} `json:"tokens"`
}

// readCodexPlan reads ~/.codex/auth.json's id_token JWT and pulls
// chatgpt_plan_type out of its "https://api.openai.com/auth" claim. No
// signature verification -- this is a local, already-trusted file, and
// the claim is read for display only, never for auth decisions.
func readCodexPlan(authPath string) (string, error) {
	data, err := os.ReadFile(authPath)
	if err != nil {
		return "", err
	}
	var auth codexAuthFile
	if err := json.Unmarshal(data, &auth); err != nil {
		return "", fmt.Errorf("parsing auth.json: %w", err)
	}
	idToken := auth.Tokens.IDToken
	if idToken == "" {
		idToken = auth.IDToken
	}
	if idToken == "" {
		return "", fmt.Errorf("auth.json missing id_token")
	}
	payload, err := decodeJWTPayload(idToken)
	if err != nil {
		return "", err
	}
	authClaim, _ := payload["https://api.openai.com/auth"].(map[string]any)
	plan, _ := authClaim["chatgpt_plan_type"].(string)
	if plan == "" {
		return "", fmt.Errorf("id_token missing chatgpt_plan_type claim")
	}
	return plan, nil
}

// decodeJWTPayload base64url-decodes a JWT's middle (payload) segment and
// parses it as JSON. Never touches the signature segment.
func decodeJWTPayload(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("malformed JWT (expected 3 dot-separated segments)")
	}
	seg := parts[1]
	data, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		// Some encoders emit padded base64url; retry before giving up.
		data, err = base64.URLEncoding.DecodeString(seg)
		if err != nil {
			return nil, fmt.Errorf("decoding JWT payload: %w", err)
		}
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("parsing JWT payload: %w", err)
	}
	return payload, nil
}

// ---- config.toml (default model + reasoning effort) ----

// codexTopLevelModelRe / codexTopLevelEffortRe match only unindented
// top-level assignments -- readCodexConfigDetail truncates the file at
// the first "[section]" header before applying them, so a
// profile-scoped override under [profiles.<name>] never wins over the
// process-wide default.
var (
	codexTopLevelModelRe  = regexp.MustCompile(`(?m)^model\s*=\s*"([^"]*)"`)
	codexTopLevelEffortRe = regexp.MustCompile(`(?m)^model_reasoning_effort\s*=\s*"([^"]*)"`)
	codexSectionHeaderRe  = regexp.MustCompile(`(?m)^\[`)
)

// readCodexConfigDetail parses just the two top-level keys this probe
// cares about out of config.toml with a couple of regexps -- no TOML
// dependency needed for that much. A missing or unreadable file yields
// an empty (non-nil) map, never an error.
func readCodexConfigDetail(path string) map[string]string {
	detail := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return detail
	}
	text := string(data)
	if loc := codexSectionHeaderRe.FindStringIndex(text); loc != nil {
		text = text[:loc[0]]
	}
	if m := codexTopLevelModelRe.FindStringSubmatch(text); len(m) == 2 {
		detail["default_model"] = m[1]
	}
	if m := codexTopLevelEffortRe.FindStringSubmatch(text); len(m) == 2 {
		detail["reasoning_effort"] = m[1]
	}
	return detail
}

// readCodexVersion runs `codex --version`, bounded to a few seconds; a
// missing binary or any other failure just means the detail key is
// omitted, not a probe error.
func readCodexVersion(ctx context.Context) string {
	res, err := execx.Run(ctx, "codex", []string{"--version"}, execx.RunOpts{Timeout: 3 * time.Second})
	if err != nil || res.ExitCode != 0 {
		return ""
	}
	return strings.TrimSpace(string(res.Stdout))
}

// ---- models_cache.json (unlisted-model diff) ----

type codexModelsCache struct {
	Models []struct {
		Slug        string `json:"slug"`
		DisplayName string `json:"display_name"`
	} `json:"models"`
}

// unlistedCodexModels returns a comma-joined, sorted list of model slugs
// present in Codex's own models_cache.json but absent from the roster's
// `known` set (lowercased model IDs), so the roster can be diffed
// against what Codex actually offers locally. Empty when the cache file
// is missing/unreadable or every cached slug is already listed.
func unlistedCodexModels(cachePath string, known map[string]bool) string {
	data, err := os.ReadFile(cachePath)
	if err != nil {
		return ""
	}
	var cache codexModelsCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return ""
	}
	var unlisted []string
	for _, m := range cache.Models {
		if m.Slug == "" || known[strings.ToLower(m.Slug)] {
			continue
		}
		unlisted = append(unlisted, m.Slug)
	}
	sort.Strings(unlisted)
	return strings.Join(unlisted, ", ")
}

// ---- session rollout scan (rate_limits) ----

type codexRateWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes float64 `json:"window_minutes"`
	ResetsAt      float64 `json:"resets_at"` // unix seconds
}

type codexRateLimits struct {
	Primary   *codexRateWindow `json:"primary"`
	Secondary *codexRateWindow `json:"secondary"`
	Credits   *struct {
		// Codex writes balance as a string ("0"); keep it raw so a
		// numeric form in a future build parses too.
		Balance json.RawMessage `json:"balance"`
	} `json:"credits"`
	PlanType string `json:"plan_type"`
}

// findLatestCodexRateLimits scans the codexMaxSessionFiles newest
// rollout-*.jsonl files under sessionsRoot (newest mtime first),
// stopping at the first file that yields a rate_limits hit. Returns
// (nil, nil) -- not an error -- when no scanned file has one; that's the
// "no data" case the caller maps to Usage.Err, distinct from a real I/O
// error walking the tree.
func findLatestCodexRateLimits(sessionsRoot string) (*codexRateLimits, error) {
	paths, err := newestCodexSessionFiles(sessionsRoot, codexMaxSessionFiles)
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		rl, err := rateLimitsFromFile(path)
		if err != nil {
			continue // unreadable/corrupt file -- try the next newest
		}
		if rl != nil {
			return rl, nil
		}
	}
	return nil, nil
}

// newestCodexSessionFiles walks sessionsRoot (which does not exist at
// all on a machine that has never run Codex -- treated as "zero files,"
// not an error) collecting rollout-*.jsonl files, and returns up to n
// paths sorted newest mtime first.
func newestCodexSessionFiles(sessionsRoot string, n int) ([]string, error) {
	type fileInfo struct {
		path    string
		modTime time.Time
	}
	var files []fileInfo
	err := filepath.WalkDir(sessionsRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries; don't fail the whole walk
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		files = append(files, fileInfo{path: path, modTime: info.ModTime()})
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modTime.After(files[j].modTime) })
	if len(files) > n {
		files = files[:n]
	}
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.path
	}
	return paths, nil
}

// rateLimitsFromFile finds the LAST line in path containing a
// `"rate_limits"` key and hands it to parseCodexRateLimitsLine.
// Returns (nil, nil) when no line in the file matches at all.
func rateLimitsFromFile(path string) (*codexRateLimits, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var lastMatch string
	scanner := bufio.NewScanner(f)
	// Rollout lines can carry large tool-call payloads; grow well past
	// bufio.Scanner's 64KiB default so a long line doesn't get silently
	// dropped as a scan error.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		// Codex writes both compact (`"rate_limits":{`) and
		// pretty-printed (`"rate_limits": {`) lines; match either.
		if strings.Contains(line, `"rate_limits"`) {
			lastMatch = line
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if lastMatch == "" {
		return nil, nil
	}
	return parseCodexRateLimitsLine(lastMatch)
}

// parseCodexRateLimitsLine parses one whole rollout JSONL event line and
// extracts its rate_limits object, wherever it sits in the structure
// (findKeyAnyDepth). The pure, no-I/O unit this package's tests exercise
// directly against a canned line. Returns (nil, nil) -- not an error --
// when the line parses as JSON but carries no rate_limits key at all.
func parseCodexRateLimitsLine(line string) (*codexRateLimits, error) {
	var event map[string]any
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		return nil, fmt.Errorf("parsing rollout line: %w", err)
	}
	raw, ok := findKeyAnyDepth(event, "rate_limits")
	if !ok {
		return nil, nil
	}
	rlMap, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("rate_limits value is not an object")
	}
	data, err := json.Marshal(rlMap)
	if err != nil {
		return nil, err
	}
	var rl codexRateLimits
	if err := json.Unmarshal(data, &rl); err != nil {
		return nil, fmt.Errorf("parsing rate_limits object: %w", err)
	}
	return &rl, nil
}

// findKeyAnyDepth walks a generically-decoded JSON value (map[string]any
// / []any / scalars) depth-first looking for the first occurrence of
// key, at any nesting level.
func findKeyAnyDepth(v any, key string) (any, bool) {
	switch t := v.(type) {
	case map[string]any:
		if val, ok := t[key]; ok {
			return val, true
		}
		for _, val := range t {
			if found, ok := findKeyAnyDepth(val, key); ok {
				return found, true
			}
		}
	case []any:
		for _, item := range t {
			if found, ok := findKeyAnyDepth(item, key); ok {
				return found, true
			}
		}
	}
	return nil, false
}

// ---- rate_limits -> Bars ----

// codexBarLabel names a window: the two well-known Codex windows get
// friendly labels ("5-hour" for a 300-minute primary window, "7-day"
// for a 10080-minute secondary one); anything else renders as a plain
// duration so a window Codex adds later still shows *something*
// meaningful instead of a blank label.
func codexBarLabel(minutes float64, isPrimary bool) string {
	switch {
	case isPrimary && minutes == 300:
		return "5-hour"
	case !isPrimary && minutes == 10080:
		return "7-day"
	case minutes > 0 && minutes < 1440:
		return fmt.Sprintf("%dh", int(minutes/60))
	case minutes >= 1440:
		return fmt.Sprintf("%dd", int(minutes/1440))
	default:
		return "window"
	}
}

func unixSecondsToTime(secs float64) time.Time {
	if secs <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(secs), 0)
}

// mapCodexBars is the pure mapping step (rate_limits -> Bars), unit
// tested directly.
func mapCodexBars(rl *codexRateLimits) []Bar {
	if rl == nil {
		return nil
	}
	var bars []Bar
	if rl.Primary != nil {
		bars = append(bars, Bar{
			Label:      codexBarLabel(rl.Primary.WindowMinutes, true),
			Percent:    rl.Primary.UsedPercent,
			ResetsAt:   unixSecondsToTime(rl.Primary.ResetsAt),
			Severity:   severityForPercent(rl.Primary.UsedPercent),
			WindowMins: rl.Primary.WindowMinutes,
		})
	}
	if rl.Secondary != nil {
		bars = append(bars, Bar{
			Label:      codexBarLabel(rl.Secondary.WindowMinutes, false),
			Percent:    rl.Secondary.UsedPercent,
			ResetsAt:   unixSecondsToTime(rl.Secondary.ResetsAt),
			Severity:   severityForPercent(rl.Secondary.UsedPercent),
			WindowMins: rl.Secondary.WindowMinutes,
		})
	}
	return bars
}
