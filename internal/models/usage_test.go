package models

import (
	"context"
	"testing"
	"time"
)

func TestProbe_UnknownOrNoneDegradesToNoop(t *testing.T) {
	r := &Roster{
		Providers: []Provider{
			{Name: "moonshot", Plan: "not subscribed", Probe: "none"},
			{Name: "mystery", Plan: "n/a", Probe: "some-future-probe-kind"},
		},
	}
	got := Probe(context.Background(), r)
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	for i, u := range got {
		if u.Err != "" {
			t.Errorf("providers[%d].Err = %q, want empty", i, u.Err)
		}
		if len(u.Bars) != 0 || len(u.Spend) != 0 {
			t.Errorf("providers[%d] = %+v, want no bars/spend", i, u)
		}
		if u.Provider != r.Providers[i].Name {
			t.Errorf("providers[%d].Provider = %q, want %q", i, u.Provider, r.Providers[i].Name)
		}
	}
}

func TestProbe_NilRoster(t *testing.T) {
	if got := Probe(context.Background(), nil); got != nil {
		t.Errorf("Probe(nil) = %v, want nil", got)
	}
}

func TestSeverityForPercent(t *testing.T) {
	tests := []struct {
		pct  float64
		want string
	}{
		{0, SeverityNormal},
		{69.9, SeverityNormal},
		{70, SeverityWarning},
		{89.9, SeverityWarning},
		{90, SeverityCritical},
		{100, SeverityCritical},
	}
	for _, tt := range tests {
		if got := severityForPercent(tt.pct); got != tt.want {
			t.Errorf("severityForPercent(%v) = %q, want %q", tt.pct, got, tt.want)
		}
	}
}

func TestParseTimeLoose(t *testing.T) {
	rfc := "2026-09-08T12:00:00Z"
	got := parseTimeLoose(rfc)
	want, _ := time.Parse(time.RFC3339, rfc)
	if !got.Equal(want) {
		t.Errorf("parseTimeLoose(%q) = %v, want %v", rfc, got, want)
	}

	unixStr := "1780000000"
	got = parseTimeLoose(unixStr)
	if got.Unix() != 1780000000 {
		t.Errorf("parseTimeLoose(%q).Unix() = %d, want 1780000000", unixStr, got.Unix())
	}

	if got := parseTimeLoose(""); !got.IsZero() {
		t.Errorf("parseTimeLoose(\"\") = %v, want zero", got)
	}
	if got := parseTimeLoose("not-a-time"); !got.IsZero() {
		t.Errorf("parseTimeLoose(garbage) = %v, want zero", got)
	}
}

// probeTimeout is a package internal but the field IS exercised indirectly
// by every probeXxx test that passes a context -- this just documents the
// invariant so a future change to the constant doesn't silently remove
// the bound.
func TestProbeTimeoutIsBounded(t *testing.T) {
	if probeTimeout <= 0 || probeTimeout > 30*time.Second {
		t.Errorf("probeTimeout = %v, want a small positive bound (dashboard must never hang on a wedged probe)", probeTimeout)
	}
}

func TestProbeTimeoutFor(t *testing.T) {
	if got := probeTimeoutFor("claude-oauth"); got != probeTimeout {
		t.Errorf("probeTimeoutFor(claude-oauth) = %v, want the shared probeTimeout %v", got, probeTimeout)
	}
	if got := probeTimeoutFor("codex-sessions"); got != probeTimeout {
		t.Errorf("probeTimeoutFor(codex-sessions) = %v, want the shared probeTimeout %v", got, probeTimeout)
	}
	if got := probeTimeoutFor("agy-quota"); got != agyProbeTimeout {
		t.Errorf("probeTimeoutFor(agy-quota) = %v, want agyProbeTimeout %v", got, agyProbeTimeout)
	}
	if agyProbeTimeout <= agyQuotaTimeout+agyModelsTimeout {
		t.Errorf("agyProbeTimeout (%v) must exceed agyQuotaTimeout+agyModelsTimeout (%v) or the outer ctx can kill agy models mid-flight on a normal run", agyProbeTimeout, agyQuotaTimeout+agyModelsTimeout)
	}
}

func TestFillBarLeft(t *testing.T) {
	u := Usage{Bars: []Bar{
		{Label: "5-hour", Percent: 62},
		{Label: "7-day", Percent: 10},
	}}
	got := fillBarLeft(u)
	if got.Bars[0].Left != 38 {
		t.Errorf("Bars[0].Left = %v, want 38", got.Bars[0].Left)
	}
	if got.Bars[1].Left != 90 {
		t.Errorf("Bars[1].Left = %v, want 90", got.Bars[1].Left)
	}
}

func TestFillBarLeft_NoBars(t *testing.T) {
	u := Usage{Err: "no key"}
	got := fillBarLeft(u)
	if len(got.Bars) != 0 {
		t.Errorf("Bars = %v, want empty", got.Bars)
	}
	if got.Err != "no key" {
		t.Errorf("Err = %q, want unchanged \"no key\"", got.Err)
	}
}
