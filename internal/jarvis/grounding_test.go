package jarvis

import (
	"testing"

	"github.com/ryanlitalien/aida/internal/jarvis/llm"
)

func TestClaimsBackgroundWork(t *testing.T) {
	fire := []string{
		"Working on it, sir. Run id dome-50-layer-5. The agent will place the fifth layer.",
		"I'll call this one amber-otter.",
		"I've got that running in the background, sir.",
		"Started the job, sir.",
	}
	for _, r := range fire {
		if !claimsBackgroundWork(r) {
			t.Errorf("expected a background-job claim for %q", r)
		}
	}
	noFire := []string{
		"Functioning nominally, sir.",
		"It's eight thirty-five PM, sir.",
		"Working on it, sir.", // generic - intentionally NOT flagged (accompanies sync tools)
		"One cat spawned, sir.",
	}
	for _, r := range noFire {
		if claimsBackgroundWork(r) {
			t.Errorf("did NOT expect a background-job claim for %q", r)
		}
	}
}

func TestUsedJobTool(t *testing.T) {
	if usedJobTool(nil) {
		t.Error("nil calls should be false")
	}
	if usedJobTool([]llm.ToolCallStat{{Name: "minecraft_ask"}}) {
		t.Error("non-job calls should be false")
	}
	// Rating tools are jarvis_*, not job_* - must not ground a claim.
	if usedJobTool([]llm.ToolCallStat{{Name: "jarvis_thumbs_up"}}) {
		t.Error("rating tools should be false")
	}
	for _, name := range []string{"job_start", "job_status", "job_list", "job_send_input", "job_cancel"} {
		if !usedJobTool([]llm.ToolCallStat{{Name: name}}) {
			t.Errorf("%s should be true", name)
		}
	}
	if !usedJobTool([]llm.ToolCallStat{{Name: "weather"}, {Name: "job_start"}}) {
		t.Error("job tool among others should be true")
	}
}

// The guard rewrites iff the reply claims background work AND no job_*
// tool actually fired. This is the exact condition applied in askAndSpeak.
func TestGroundingDecision(t *testing.T) {
	cases := []struct {
		name  string
		reply string
		calls []llm.ToolCallStat
		want  bool // true = should rewrite
	}{
		{"the dome confabulation", "Working on it, sir. Run id dome-50-layer-5.", nil, true},
		{"legit job_start", "Working on it, sir. I'll call this one amber-otter.", []llm.ToolCallStat{{Name: "job_start"}}, false},
		{"plain reply", "Functioning nominally, sir.", nil, false},
		{"claim with only sync tool", "The agent will continue in the background.", []llm.ToolCallStat{{Name: "minecraft_ask"}}, true},
		// Status turns legitimately mention background jobs while reporting
		// real state - any job_* tool grounds the claim, not just job_start.
		{"status check via job_list", "You have one background job running, sir - amber-otter.", []llm.ToolCallStat{{Name: "job_list"}}, false},
		{"status check via job_status", "The amber-otter background job is awaiting your input, sir.", []llm.ToolCallStat{{Name: "job_status"}}, false},
		{"reply delivered via job_send_input", "I've passed that along; the agent will continue in the background.", []llm.ToolCallStat{{Name: "job_send_input"}}, false},
	}
	for _, c := range cases {
		got := claimsBackgroundWork(c.reply) && !usedJobTool(c.calls)
		if got != c.want {
			t.Errorf("%s: rewrite=%v, want %v", c.name, got, c.want)
		}
	}
}
