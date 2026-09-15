package models

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// codexAppServerRPCTimeout bounds how long runCodexAppServerRPC waits for
// the id:2 reply once the process is up -- distinct from (and inside) the
// caller's own ctx, which is already bounded to probeTimeout by Probe. A
// wedged app-server (no reply, no exit) still gets killed promptly by
// ctx's own deadline; this is just documentation of that fact, not a
// second timer.

// codexRPCWindow is one rate-limit window in the app-server's camelCase
// reply shape -- the live-RPC analogue of codexRateWindow's snake_case
// session-log shape.
type codexRPCWindow struct {
	UsedPercent        float64 `json:"usedPercent"`
	WindowDurationMins float64 `json:"windowDurationMins"`
	ResetsAt           float64 `json:"resetsAt"` // unix seconds
}

// codexRPCCredits is account/rateLimits/read's credits object -- Balance
// arrives as a JSON string ("0"), unlike the session-log path's
// json.RawMessage (which has to tolerate either shape).
type codexRPCCredits struct {
	HasCredits bool   `json:"hasCredits"`
	Unlimited  bool   `json:"unlimited"`
	Balance    string `json:"balance"`
}

// codexRPCRateLimits is the "rateLimits" object inside account/rateLimits/read's
// result.
type codexRPCRateLimits struct {
	LimitID   string           `json:"limitId"`
	Primary   *codexRPCWindow  `json:"primary"`
	Secondary *codexRPCWindow  `json:"secondary"`
	Credits   *codexRPCCredits `json:"credits"`
	PlanType  string           `json:"planType"`
}

// codexRPCResetCredits is account/rateLimits/read result's
// "rateLimitResetCredits" object -- a sibling of "rateLimits", not
// nested inside it.
type codexRPCResetCredits struct {
	AvailableCount int `json:"availableCount"`
}

// codexRPCResult is the "result" object of the id:2 reply.
type codexRPCResult struct {
	RateLimits            *codexRPCRateLimits   `json:"rateLimits"`
	RateLimitResetCredits *codexRPCResetCredits `json:"rateLimitResetCredits"`
}

// codexRPCReply is the id:2 JSON-RPC reply to account/rateLimits/read.
type codexRPCReply struct {
	ID     int             `json:"id"`
	Result *codexRPCResult `json:"result"`
}

// codexAppServerRequestLines builds the three newline-delimited JSON-RPC
// lines this probe writes to `codex app-server`'s stdin on startup:
// initialize, the initialized notification, then the one call this probe
// actually wants (account/rateLimits/read). clientVersion is informational
// only (the app-server logs it); an empty value falls back to "0.1".
func codexAppServerRequestLines(clientVersion string) []string {
	if clientVersion == "" {
		clientVersion = "0.1"
	}
	msgs := []map[string]any{
		{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "initialize",
			"params": map[string]any{
				"clientInfo": map[string]any{"name": "aida", "version": clientVersion},
			},
		},
		{"jsonrpc": "2.0", "method": "initialized"},
		{"jsonrpc": "2.0", "id": 2, "method": "account/rateLimits/read", "params": map[string]any{}},
	}
	lines := make([]string, 0, len(msgs))
	for _, m := range msgs {
		b, err := json.Marshal(m)
		if err != nil {
			continue // unreachable for these static literals
		}
		lines = append(lines, string(b))
	}
	return lines
}

// parseCodexRPCLine inspects one line of app-server stdout and reports
// whether it's the id:2 reply this probe is waiting for. Every other
// line -- notifications with no "id" (e.g. remoteControl/status/changed),
// the id:1 initialize reply, malformed JSON -- parses fine but reports
// matched=false so the caller keeps reading. An id:2 line that carries a
// JSON-RPC "error" instead of a "result" reports matched=true with a
// non-nil error, since that IS the reply the caller was waiting for, just
// a failed one -- distinct from "keep scanning."
//
// The pure, no-I/O unit this package's tests exercise directly against
// canned lines, mirroring parseCodexRateLimitsLine's contract for the
// session-log path.
func parseCodexRPCLine(line string) (reply *codexRPCReply, matched bool, err error) {
	var envelope struct {
		ID     *int            `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if jerr := json.Unmarshal([]byte(line), &envelope); jerr != nil {
		return nil, false, nil // not JSON this probe cares about; keep scanning
	}
	if envelope.ID == nil || *envelope.ID != 2 {
		return nil, false, nil // a notification, or some other request's reply
	}
	if envelope.Error != nil {
		return nil, true, fmt.Errorf("app-server returned an error for account/rateLimits/read: %s", envelope.Error.Message)
	}
	var result codexRPCResult
	if jerr := json.Unmarshal(envelope.Result, &result); jerr != nil {
		return nil, true, fmt.Errorf("parsing account/rateLimits/read result: %w", jerr)
	}
	return &codexRPCReply{ID: 2, Result: &result}, true, nil
}

// runCodexAppServerRPC spawns `codex app-server`, writes the three
// handshake/request lines to its stdin, and reads stdout line by line
// until the id:2 reply arrives. `codex app-server` is a long-lived stdio
// server (it never exits on its own once the reply is sent), not a
// one-shot command, so this can't use execx's send-all-stdin-then-wait
// model -- stdin has to be written and then left open while stdout is
// read incrementally. The process is always killed before returning,
// whether a reply was found or not.
//
// Bounded entirely by ctx: exec.CommandContext ties the process's life
// to ctx, so ctx's own deadline (the caller passes the same probeTimeout
// / probeTimeoutFor ctx every other probe gets) kills a wedged
// app-server and unblocks the stdout scan via EOF -- there's no separate
// internal timer here.
func runCodexAppServerRPC(ctx context.Context, clientVersion string) (*codexRPCReply, error) {
	cmd := exec.CommandContext(ctx, "codex", "app-server")

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("opening app-server stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("opening app-server stdout: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting codex app-server: %w", err)
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()

	for _, line := range codexAppServerRequestLines(clientVersion) {
		if _, werr := stdin.Write([]byte(line + "\n")); werr != nil {
			return nil, fmt.Errorf("writing app-server request: %w", werr)
		}
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		reply, matched, perr := parseCodexRPCLine(line)
		if !matched {
			continue
		}
		return reply, perr
	}
	if serr := scanner.Err(); serr != nil {
		return nil, fmt.Errorf("reading app-server stdout: %w", serr)
	}
	return nil, fmt.Errorf("app-server closed stdout before replying to account/rateLimits/read")
}

// toCodexRateLimits converts the app-server's camelCase rate-limits shape
// into the session-log path's codexRateLimits, so both sources feed the
// same tested mapCodexBars.
func toCodexRateLimits(rpc *codexRPCRateLimits) *codexRateLimits {
	if rpc == nil {
		return nil
	}
	rl := &codexRateLimits{PlanType: rpc.PlanType}
	if rpc.Primary != nil {
		rl.Primary = &codexRateWindow{
			UsedPercent:   rpc.Primary.UsedPercent,
			WindowMinutes: rpc.Primary.WindowDurationMins,
			ResetsAt:      rpc.Primary.ResetsAt,
		}
	}
	if rpc.Secondary != nil {
		rl.Secondary = &codexRateWindow{
			UsedPercent:   rpc.Secondary.UsedPercent,
			WindowMinutes: rpc.Secondary.WindowDurationMins,
			ResetsAt:      rpc.Secondary.ResetsAt,
		}
	}
	if rpc.Credits != nil {
		balanceJSON, err := json.Marshal(rpc.Credits.Balance)
		if err == nil {
			rl.Credits = &struct {
				Balance json.RawMessage `json:"balance"`
			}{Balance: balanceJSON}
		}
	}
	return rl
}

// probeCodexAppServerRateLimits is the live-RPC path's top-level entry
// point: run the RPC, and on success convert its reply into the same
// codexRateLimits + reset-credits-count shape probeCodexSessions maps
// into Bars/Detail. Returns an error whenever the RPC didn't produce a
// usable rateLimits object -- codex missing, a timeout, a malformed
// reply, or a reply with no rateLimits at all -- so the caller can fall
// back to the session-log scan.
func probeCodexAppServerRateLimits(ctx context.Context, clientVersion string) (*codexRateLimits, int, error) {
	reply, err := runCodexAppServerRPC(ctx, clientVersion)
	if err != nil {
		return nil, 0, err
	}
	if reply == nil || reply.Result == nil || reply.Result.RateLimits == nil {
		return nil, 0, fmt.Errorf("app-server reply carried no rateLimits")
	}
	resetCredits := 0
	if reply.Result.RateLimitResetCredits != nil {
		resetCredits = reply.Result.RateLimitResetCredits.AvailableCount
	}
	return toCodexRateLimits(reply.Result.RateLimits), resetCredits, nil
}
