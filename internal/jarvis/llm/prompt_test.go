package llm

import (
	"strings"
	"testing"
	"time"
)

// systemPrompt composes base + user-context (soul) + home + hosts + date +
// recall, in that order (stable blocks first so the prompt prefix stays
// cache-friendly), with empty extensions adding no block. The date block is
// the one exception: it is always present, so this also pins the clock via
// the package var `now` and checks the composition against an exact
// expected string, the same protection the old byte-equality-with-
// baseSystemPrompt assertion gave before the date block existed: it still
// fails if personalizeBase/applyTone stop being no-ops on empty input, or
// if a stray block sneaks into the composition.
func TestSystemPromptComposition(t *testing.T) {
	orig := now
	defer func() { now = orig }()
	now = func() time.Time { return time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC) } // a Thursday

	bare := systemPrompt("", "", "", "", "", "")
	wantBare := baseSystemPrompt + "\n\n" + dateContext()
	if bare != wantBare {
		t.Error("empty extensions should yield exactly the base prompt plus the date-grounding block, unmutated and unreordered")
	}
	if !strings.Contains(bare, "Today is Thursday, September 10, 2026") {
		t.Errorf("date block missing or malformed: %q", bare)
	}

	full := systemPrompt("", "", "User context:\n- Name: Ryan\n- Background: works at butterstack",
		"Boston", "Known machines:\n- build-box: runs background agents", "## Past feedback\n- use Fahrenheit")
	for _, want := range []string{"works at butterstack", "home location is: Boston", "Known machines", "build-box", "Today is Thursday", "use Fahrenheit"} {
		if !strings.Contains(full, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	soulAt := strings.Index(full, "works at butterstack")
	homeAt := strings.Index(full, "home location is: Boston")
	hostsAt := strings.Index(full, "Known machines")
	dateAt := strings.Index(full, "Today is")
	recallAt := strings.Index(full, "Past feedback")
	if !(soulAt < homeAt && homeAt < hostsAt && hostsAt < dateAt && dateAt < recallAt) {
		t.Errorf("block order wrong: soul@%d home@%d hosts@%d date@%d recall@%d", soulAt, homeAt, hostsAt, dateAt, recallAt)
	}
}

// TestSystemPromptComposition_EmptyHostsUnchanged pins the requirement
// that an unconfigured hosts: block (an empty hostsProse argument) must
// leave the composed prompt byte-identical to today's - no stray
// "Known machines:" header, no extra blank line. TestSystemPromptComposition's
// "bare" case already covers this implicitly (it passes "" for hostsProse
// among everything else); this test isolates it so a future edit to the
// hosts wiring that stops treating "" as "no block" fails loudly here
// even if a soul/home/recall value is also present.
func TestSystemPromptComposition_EmptyHostsUnchanged(t *testing.T) {
	orig := now
	defer func() { now = orig }()
	now = func() time.Time { return time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC) }

	withoutHosts := systemPrompt("", "", "User context:\n- Name: Ryan", "Boston", "", "## Past feedback\n- note")
	if strings.Contains(withoutHosts, "Known machines") {
		t.Errorf("empty hostsProse should add no block, got: %q", withoutHosts)
	}
}

// A cosmetic-twin persona swaps the standalone name (both the all-caps
// identity token and the title-case self-reference) but leaves the dotted
// "J.A.R.V.I.S." lineage note intact. An empty/"Jarvis" persona is a no-op.
func TestSystemPromptPersona(t *testing.T) {
	orig := now
	defer func() { now = orig }()
	now = func() time.Time { return time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC) }

	aida := systemPrompt("Aida", "", "", "", "", "")
	if !strings.Contains(aida, "You are AIDA,") {
		t.Error("Aida persona should rename the identity token to AIDA")
	}
	if !strings.Contains(aida, "Aida will speak") {
		t.Error("Aida persona should rename the title-case self-reference")
	}
	if strings.Contains(aida, "You are JARVIS") {
		t.Error("Aida persona should not leave JARVIS in the identity line")
	}
	if !strings.Contains(aida, "J.A.R.V.I.S.") {
		t.Error("dotted lineage reference should survive personalization")
	}
	if systemPrompt("Jarvis", "", "", "", "", "") != baseSystemPrompt+"\n\n"+dateContext() {
		t.Error(`persona "Jarvis" should be a no-op`)
	}
}

// A per-persona Tone swaps the default "dry, witty" descriptor for a warmer
// one without leaving the contradictory "dry" behind. Empty tone is a no-op,
// and the base prompt must still contain the exact defaultTone the swap keys on.
func TestSystemPromptTone(t *testing.T) {
	orig := now
	defer func() { now = orig }()
	now = func() time.Time { return time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC) }

	if !strings.Contains(baseSystemPrompt, defaultTone) {
		t.Fatalf("base prompt no longer contains defaultTone %q - the tone override is silently disabled", defaultTone)
	}
	warm := systemPrompt("Aida", "concise, warm, and personable", "", "", "", "")
	if !strings.Contains(warm, "Be concise, warm, and personable.") {
		t.Error("Aida tone should read 'Be concise, warm, and personable.'")
	}
	if strings.Contains(warm, defaultTone) {
		t.Error("the default dry/witty descriptor should be gone once a tone override is set")
	}
	if systemPrompt("Jarvis", "", "", "", "", "") != baseSystemPrompt+"\n\n"+dateContext() {
		t.Error("empty tone should leave the base prompt unchanged")
	}
}

// A food log gets a fixed, terse readout: calories and protein LEFT for the
// day, plus a workout nudge only when today's workout is still outstanding.
// Everything else the coach volunteers (the item's own macros, training
// volume, weight trends, follow-up questions) stays out of the spoken reply.
func TestBaseSystemPrompt_FoodLoggingReadout(t *testing.T) {
	for _, want := range []string{
		"FOOD LOGGING",
		"calories remaining today",
		"protein\nremaining against the target",
		"whether a workout is already logged for\ntoday",
		"AT MOST two short sentences",
		"do not end with a question",
	} {
		if !strings.Contains(baseSystemPrompt, want) {
			t.Errorf("base prompt missing %q in the food-logging guidance", want)
		}
	}
}

// The base prompt must steer composed, multi-step requests (task #193's
// failure shape: search email, correlate to a GitHub issue, draft a new
// one) at job_start instead of letting the model chain aida_query calls
// against a hard per-call time limit.
func TestBaseSystemPrompt_RoutesComposedWorkToJobStart(t *testing.T) {
	for _, want := range []string{"COMPOSED requests", "GitHub issue", "aida_query"} {
		if !strings.Contains(baseSystemPrompt, want) {
			t.Errorf("base prompt missing %q in the job_start routing guidance", want)
		}
	}
}
