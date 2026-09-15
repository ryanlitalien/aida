package listener

import (
	"strings"
	"testing"
	"time"

	"github.com/ryanlitalien/aida/internal/jarvis/audit"
)

// Jarvis accepts the full prefix set; Aida is narrowed to just "hey aida" (one
// crisp phrase reads more reliably than "ok aida"). Each must match its own
// phrase and reject the other's - plus reject longer words the short Aida name
// branches could otherwise swallow ("hey Idaho").
func TestWakeRegexRouting(t *testing.T) {
	jarvis := WakeRegexFor("jarvis")                       // full prefixes
	aida := WakeRegexForPrefix("hey", "aida|ada|ayda|ida") // hey-only

	cases := []struct {
		transcript         string
		jarvisHit, aidaHit bool
	}{
		{"Ok Jarvis, what's the weather?", true, false},
		{"Hey Jarvis", true, false},
		{"good morning, jarvis", true, false},
		{"Hey Aida, add a task", false, true},
		{"hey aida what time is it", false, true},
		{"Hey Ada.", false, true},                 // whisper mangles AY-duh → "Ada"
		{"hey ayda", false, true},                 // …or "Ayda"
		{"Ok Aida, add a task", false, false},     // "ok" no longer wakes Aida
		{"good morning Ada", false, false},        // dropped prefix
		{"okay ayda", false, false},               // dropped prefix
		{"hey Adam is coming over", false, false}, // "ada" must not swallow "Adam"
		{"hey Idaho is nice", false, false},       // "ida" must not swallow "Idaho"
		{"what's the weather", false, false},      // no wake at all
	}
	for _, c := range cases {
		if got := jarvis.MatchString(c.transcript); got != c.jarvisHit {
			t.Errorf("jarvis.MatchString(%q) = %v, want %v", c.transcript, got, c.jarvisHit)
		}
		if got := aida.MatchString(c.transcript); got != c.aidaHit {
			t.Errorf("aida.MatchString(%q) = %v, want %v", c.transcript, got, c.aidaHit)
		}
	}
}

func TestTimeOfDayAutoQuery(t *testing.T) {
	cases := []struct {
		in   string
		want string // substring expected in the synthetic query, "" for no match
	}{
		{"Good morning, Jarvis", "weather at home"},
		{"good  Morning Jarvis,", "weather at home"},
		{"Good afternoon, Jarvis.", "afternoon"},
		{"good evening Jarvis", "evening"},
		{"Hey Jarvis", ""},
		{"Ok Jarvis", ""},
		{"okay, jarvis", ""},
	}
	for _, c := range cases {
		got := timeOfDayAutoQuery(c.in)
		if c.want == "" {
			if got != "" {
				t.Errorf("timeOfDayAutoQuery(%q) = %q, want empty", c.in, got)
			}
			continue
		}
		if !strings.Contains(strings.ToLower(got), strings.ToLower(c.want)) {
			t.Errorf("timeOfDayAutoQuery(%q) = %q, want substring %q", c.in, got, c.want)
		}
	}
}

func TestExpectsAnswer(t *testing.T) {
	cases := []struct {
		reply string
		want  bool
	}{
		{`Is it OK for me to commit this to memory, sir?`, true},
		{`Is that $35 per bag or per yard, sir?"`, true},
		// The regression: a question NOT at the end of the reply. Jarvis
		// appends a trailing clause, so an ends-with test missed it and the
		// follow-up window never armed.
		{`At four dollars each for what, sir? I need to know the quantity.`, true},
		{`Saved to your brain's long-term memory, sir.`, false},
		{`I'll pull that up now.`, false},
		{``, false},
		{`Really?   `, true},
	}
	for _, c := range cases {
		if got := expectsAnswer(c.reply); got != c.want {
			t.Errorf("expectsAnswer(%q) = %v, want %v", c.reply, got, c.want)
		}
	}
}

// TestResolveArm covers the follow-up arming truth table used by both the
// VAD and PTT call sites in Run: a question with a positive window arms
// with a bounded deadline, a negative window disables follow-up entirely
// (even for a question), a bare-wake arm is left untouched (persona and
// deadline both), and no arm plus no follow-up is the zero state.
func TestResolveArm(t *testing.T) {
	jarvisP := &Persona{Name: "Jarvis"}
	aidaP := &Persona{Name: "Aida"}
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	window := 15 * time.Second
	oldDeadline := now.Add(-5 * time.Second) // a deadline set by some earlier arm

	cases := []struct {
		name       string
		armed      *Persona
		armedUntil time.Time
		followUp   bool
		window     time.Duration
		wantArmed  *Persona
		wantUntil  time.Time
	}{
		{
			name:       "question with positive window arms and sets deadline to now+window",
			armed:      jarvisP,
			armedUntil: time.Time{},
			followUp:   true,
			window:     window,
			wantArmed:  jarvisP,
			wantUntil:  now.Add(window),
		},
		{
			name:       "negative window disables follow-up, so a question disarms instead of arming",
			armed:      jarvisP,
			armedUntil: time.Time{},
			followUp:   true,
			window:     -1,
			wantArmed:  nil,
			wantUntil:  time.Time{},
		},
		{
			name:       "no arm and no follow-up yields the zero state",
			armed:      nil,
			armedUntil: time.Time{},
			followUp:   false,
			window:     window,
			wantArmed:  nil,
			wantUntil:  time.Time{},
		},
		{
			name:       "bare-wake arm keeps its persona and its unbounded zero deadline",
			armed:      aidaP,
			armedUntil: time.Time{},
			followUp:   false,
			window:     window,
			wantArmed:  aidaP,
			wantUntil:  time.Time{},
		},
		{
			name:       "bare-wake arm keeps its persona and its existing bounded deadline unchanged",
			armed:      aidaP,
			armedUntil: oldDeadline,
			followUp:   false,
			window:     window,
			wantArmed:  aidaP,
			wantUntil:  oldDeadline,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotArmed, gotUntil := resolveArm(c.armed, c.armedUntil, c.followUp, c.window, now)
			// Identity, not just non-nil: routing the follow-up to the wrong
			// persona is the failure mode that matters.
			if gotArmed != c.wantArmed {
				t.Errorf("resolveArm(...) armed = %p (%v), want %p (%v)", gotArmed, gotArmed, c.wantArmed, c.wantArmed)
			}
			if !gotUntil.Equal(c.wantUntil) {
				t.Errorf("resolveArm(...) armedUntil = %v, want %v", gotUntil, c.wantUntil)
			}
		})
	}
}

// TestResolveArmPTTQuestionArmsPersona documents the original bug: a PTT
// reply that expects an answer must arm that exact PTT persona with a
// bounded deadline so the mic stays open for a wake-less answer. This is
// the regression that let "no, I'm good" go unheard - the PTT path armed
// nothing at all when the reply was a question.
func TestResolveArmPTTQuestionArmsPersona(t *testing.T) {
	pttPersona := &Persona{Name: "Jarvis-PTT"}
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	window := 15 * time.Second

	gotArmed, gotUntil := resolveArm(pttPersona, time.Time{}, true, window, now)

	if gotArmed != pttPersona {
		t.Fatalf("resolveArm armed = %p, want the exact PTT persona %p", gotArmed, pttPersona)
	}
	if want := now.Add(window); !gotUntil.Equal(want) {
		t.Errorf("resolveArm armedUntil = %v, want bounded deadline %v", gotUntil, want)
	}
}

// TestToolCallRecovered covers the auto-recovery suppression: a failure that
// the model itself retried and fixed later in the same turn should not file
// a jarvis-error task, since the user never experienced a failure.
func TestToolCallRecovered(t *testing.T) {
	calls := []audit.ToolCall{
		{Name: "minecraft_ask", Error: "ssh: exit status 1"},
		{Name: "weather", Error: ""},
		{Name: "minecraft_ask", Error: ""},
	}
	if !toolCallRecovered(calls, 0) {
		t.Error("index 0 (minecraft_ask failure) should be recovered by the later successful minecraft_ask call")
	}
	if toolCallRecovered(calls, 1) {
		t.Error("index 1 already succeeded; there is nothing to recover from")
	}

	onlyFailure := []audit.ToolCall{{Name: "minecraft_ask", Error: "timeout"}}
	if toolCallRecovered(onlyFailure, 0) {
		t.Error("a failure with no later call at all must not count as recovered")
	}

	differentTool := []audit.ToolCall{
		{Name: "minecraft_ask", Error: "timeout"},
		{Name: "weather", Error: ""},
	}
	if toolCallRecovered(differentTool, 0) {
		t.Error("a later success on a different tool must not mask this tool's failure")
	}

	stillFailing := []audit.ToolCall{
		{Name: "minecraft_ask", Error: "timeout"},
		{Name: "minecraft_ask", Error: "timeout again"},
	}
	if toolCallRecovered(stillFailing, 0) {
		t.Error("a retry that also failed must not count as recovered")
	}
}
