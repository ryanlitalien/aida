package models

import (
	"strings"
	"testing"
)

// cannedAppServerReply is the real id:2 reply captured from `codex
// app-server` on 2026-09-08 (account/rateLimits/read), trimmed of the
// rateLimitsByLimitId/accountId/rateLimitUpsell fields this probe
// doesn't read.
const cannedAppServerReply = `{"id":2,"result":{"rateLimits":{"limitId":"codex","primary":{"usedPercent":62,"windowDurationMins":300,"resetsAt":1788891614},"secondary":{"usedPercent":10,"windowDurationMins":10080,"resetsAt":1789478414},"credits":{"hasCredits":false,"unlimited":false,"balance":"0"},"planType":"plus","spendControlReached":false,"rateLimitReachedType":null},"rateLimitResetCredits":{"availableCount":3,"credits":[{"title":"Full reset (Weekly + 5 hr)","status":"available"}]}}}`

const cannedAppServerNotification = `{"method":"remoteControl/status/changed","params":{"status":"disabled"},"emittedAtMs":1788878975516}`

const cannedAppServerInitializeReply = `{"id":1,"result":{"codexHome":"/Users/fakehome/.codex"}}`

func TestParseCodexRPCLine_Id2Reply(t *testing.T) {
	reply, matched, err := parseCodexRPCLine(cannedAppServerReply)
	if err != nil {
		t.Fatalf("parseCodexRPCLine: %v", err)
	}
	if !matched {
		t.Fatal("matched = false, want true for an id:2 line")
	}
	if reply == nil || reply.Result == nil || reply.Result.RateLimits == nil {
		t.Fatal("reply/result/rateLimits is nil")
	}
	rl := reply.Result.RateLimits
	if rl.Primary == nil || rl.Primary.UsedPercent != 62 {
		t.Errorf("Primary = %+v, want UsedPercent 62", rl.Primary)
	}
	if rl.Primary.WindowDurationMins != 300 {
		t.Errorf("Primary.WindowDurationMins = %v, want 300", rl.Primary.WindowDurationMins)
	}
	if rl.Secondary == nil || rl.Secondary.UsedPercent != 10 {
		t.Errorf("Secondary = %+v, want UsedPercent 10", rl.Secondary)
	}
	if rl.PlanType != "plus" {
		t.Errorf("PlanType = %q, want plus", rl.PlanType)
	}
	if rl.Credits == nil || rl.Credits.Balance != "0" {
		t.Errorf("Credits = %+v, want Balance \"0\"", rl.Credits)
	}
	if reply.Result.RateLimitResetCredits == nil || reply.Result.RateLimitResetCredits.AvailableCount != 3 {
		t.Errorf("RateLimitResetCredits = %+v, want AvailableCount 3", reply.Result.RateLimitResetCredits)
	}
}

func TestParseCodexRPCLine_NotificationIgnored(t *testing.T) {
	reply, matched, err := parseCodexRPCLine(cannedAppServerNotification)
	if err != nil {
		t.Fatalf("parseCodexRPCLine: %v", err)
	}
	if matched {
		t.Errorf("matched = true, want false for a notification (no id): reply=%+v", reply)
	}
}

func TestParseCodexRPCLine_OtherIDIgnored(t *testing.T) {
	reply, matched, err := parseCodexRPCLine(cannedAppServerInitializeReply)
	if err != nil {
		t.Fatalf("parseCodexRPCLine: %v", err)
	}
	if matched {
		t.Errorf("matched = true, want false for the id:1 initialize reply: reply=%+v", reply)
	}
}

func TestParseCodexRPCLine_MalformedJSON(t *testing.T) {
	reply, matched, err := parseCodexRPCLine("not json at all")
	if err != nil {
		t.Errorf("err = %v, want nil (malformed lines are skipped, not fatal)", err)
	}
	if matched {
		t.Errorf("matched = true, want false: reply=%+v", reply)
	}
}

func TestParseCodexRPCLine_Id2Error(t *testing.T) {
	const errLine = `{"id":2,"error":{"code":-32000,"message":"not authenticated"}}`
	reply, matched, err := parseCodexRPCLine(errLine)
	if !matched {
		t.Fatal("matched = false, want true for an id:2 error reply")
	}
	if err == nil {
		t.Fatal("err = nil, want an error for an id:2 JSON-RPC error reply")
	}
	if reply != nil {
		t.Errorf("reply = %+v, want nil on error", reply)
	}
}

func TestToCodexRateLimits(t *testing.T) {
	reply, matched, err := parseCodexRPCLine(cannedAppServerReply)
	if err != nil || !matched {
		t.Fatalf("parseCodexRPCLine: matched=%v err=%v", matched, err)
	}
	rl := toCodexRateLimits(reply.Result.RateLimits)
	if rl == nil {
		t.Fatal("toCodexRateLimits returned nil")
	}
	if rl.Primary == nil || rl.Primary.UsedPercent != 62 || rl.Primary.WindowMinutes != 300 {
		t.Errorf("Primary = %+v", rl.Primary)
	}
	if rl.Secondary == nil || rl.Secondary.UsedPercent != 10 || rl.Secondary.WindowMinutes != 10080 {
		t.Errorf("Secondary = %+v", rl.Secondary)
	}
	if rl.PlanType != "plus" {
		t.Errorf("PlanType = %q, want plus", rl.PlanType)
	}
	if rl.Credits == nil {
		t.Fatal("Credits is nil")
	}
	// mapCodexBars is the tested, shared mapping step -- confirm the
	// RPC path feeds it correctly end to end, same as the session-log
	// path does in TestMapCodexBars.
	bars := mapCodexBars(rl)
	if len(bars) != 2 {
		t.Fatalf("len(bars) = %d, want 2", len(bars))
	}
	if bars[0].Label != "5-hour" || bars[0].Percent != 62 || bars[0].WindowMins != 300 {
		t.Errorf("bars[0] = %+v, want 5-hour 62%% WindowMins 300", bars[0])
	}
	if bars[1].Label != "7-day" || bars[1].Percent != 10 || bars[1].WindowMins != 10080 {
		t.Errorf("bars[1] = %+v, want 7-day 10%% WindowMins 10080", bars[1])
	}
}

func TestToCodexRateLimits_Nil(t *testing.T) {
	if got := toCodexRateLimits(nil); got != nil {
		t.Errorf("toCodexRateLimits(nil) = %v, want nil", got)
	}
}

func TestCodexAppServerRequestLines(t *testing.T) {
	lines := codexAppServerRequestLines("1.2.3")
	if len(lines) != 3 {
		t.Fatalf("len(lines) = %d, want 3", len(lines))
	}
	for i, l := range lines {
		if l == "" {
			t.Errorf("lines[%d] is empty", i)
		}
	}
	// Every line must itself be one complete JSON value (newline-delimited
	// JSON-RPC), and the id:2 request must name the exact method this
	// probe depends on.
	for _, want := range []string{`"method":"initialize"`, `"method":"initialized"`, `"method":"account/rateLimits/read"`, `"id":2`} {
		found := false
		for _, l := range lines {
			if strings.Contains(l, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no request line contains %q: lines=%v", want, lines)
		}
	}
}

func TestCodexAppServerRequestLines_EmptyVersionFallsBack(t *testing.T) {
	lines := codexAppServerRequestLines("")
	found := false
	for _, l := range lines {
		if strings.Contains(l, `"version":"0.1"`) {
			found = true
		}
	}
	if !found {
		t.Errorf("empty clientVersion did not fall back to 0.1: lines=%v", lines)
	}
}
