package models

import (
	"testing"
	"time"
)

func TestFormatDurationHuman(t *testing.T) {
	tests := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"under an hour", 42 * time.Minute, "42m"},
		{"hours and minutes", 3*time.Hour + 15*time.Minute, "3h15m"},
		{"just under a day", 23*time.Hour + 59*time.Minute, "23h59m"},
		{"exactly a day", 24 * time.Hour, "1d 0h"},
		{"multi-day, minutes dropped", 166*time.Hour + 6*time.Minute, "6d 22h"},
		{"whole week", 7 * 24 * time.Hour, "7d 0h"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FormatDurationHuman(tt.d); got != tt.want {
				t.Errorf("FormatDurationHuman(%v) = %q, want %q", tt.d, got, tt.want)
			}
		})
	}
}

func TestFormatDurationHuman_NegativeClampsToZero(t *testing.T) {
	if got := FormatDurationHuman(-5 * time.Minute); got != "0m" {
		t.Errorf("FormatDurationHuman(-5m) = %q, want %q", got, "0m")
	}
}

func TestFormatPaceChip(t *testing.T) {
	tests := []struct {
		name string
		p    *Pace
		want string
	}{
		{"nil", nil, ""},
		{"hot", &Pace{Verdict: PaceHot, Ratio: 2.849}, "2.8x HOT"},
		{"idle", &Pace{Verdict: PaceIdle, Ratio: 0.1512}, "0.2x IDLE"},
		{"on-pace", &Pace{Verdict: PaceOnPace, Ratio: 0.99}, "on pace"},
		{"early", &Pace{Verdict: PaceEarly}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FormatPaceChip(tt.p); got != tt.want {
				t.Errorf("FormatPaceChip(%+v) = %q, want %q", tt.p, got, tt.want)
			}
		})
	}
}

func TestRoundPricesForDisplay(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"$19.99", "$20"},
		{"$9.99/mo", "$10/mo"},
		{"$75.00", "$75"},
		{"$32.81", "$32.81"},
		{"$32.81 / $75.00", "$32.81 / $75"},
		{"Claude Pro ($20/mo)", "Claude Pro ($20/mo)"},
		{"$74.99/mo", "$75/mo"},
		{"no price here", "no price here"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := RoundPricesForDisplay(tt.in); got != tt.want {
				t.Errorf("RoundPricesForDisplay(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
