package roster

import (
	"strings"
	"testing"
	"time"
)

func TestBuildSubagentPrompt(t *testing.T) {
	cases := []struct {
		name  string
		agent string
		task  string
		want  string
	}{
		{
			name:  "no agent: task passes through verbatim",
			agent: "",
			task:  "what's in the ready queue",
			want:  "what's in the ready queue",
		},
		{
			name:  "named agent: wrapped with a delegation directive",
			agent: "product-manager",
			task:  "what's in the ready queue",
			want:  "Use the product-manager subagent for this request, and reply with only its final report.\n\nRequest: what's in the ready queue",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildSubagentPrompt(tc.agent, tc.task)
			if got != tc.want {
				t.Errorf("buildSubagentPrompt(%q, %q) = %q, want %q", tc.agent, tc.task, got, tc.want)
			}
		})
	}
}

func TestBuildSubagentPrompt_QuotingIsInert(t *testing.T) {
	// The prompt is passed as a single argv element via execx.Run, never
	// through sh -c, so quote characters in the task must survive
	// untouched rather than being escaped or stripped.
	task := `it's a "test" with quotes`
	got := buildSubagentPrompt("qa", task)
	if !strings.Contains(got, task) {
		t.Errorf("buildSubagentPrompt did not preserve quoting: %q", got)
	}
}

func TestNewSubagentBackend_RequiresDir(t *testing.T) {
	if _, err := newSubagentBackend(&Entry{Name: "x", Kind: KindSubagent}, Deps{}); err == nil {
		t.Fatal("expected an error when Subagent is nil")
	}
	if _, err := newSubagentBackend(&Entry{Name: "x", Kind: KindSubagent, Subagent: &SubagentSpec{}}, Deps{}); err == nil {
		t.Fatal("expected an error when Subagent.Dir is empty")
	}
}

func TestNewSubagentBackend_DefaultTimeout(t *testing.T) {
	e := &Entry{Name: "x", Kind: KindSubagent, Subagent: &SubagentSpec{Dir: "/tmp/somewhere"}}
	b, err := newSubagentBackend(e, Deps{})
	if err != nil {
		t.Fatalf("newSubagentBackend: %v", err)
	}
	sb, ok := b.(*subagentBackend)
	if !ok {
		t.Fatalf("newSubagentBackend returned %T, want *subagentBackend", b)
	}
	if sb.timeout != DefaultSubagentTimeout {
		t.Errorf("timeout = %v, want default %v", sb.timeout, DefaultSubagentTimeout)
	}
	if sb.Kind() != KindSubagent {
		t.Errorf("Kind() = %q, want %q", sb.Kind(), KindSubagent)
	}
}

func TestNewSubagentBackend_TimeoutOverride(t *testing.T) {
	e := &Entry{
		Name: "x", Kind: KindSubagent,
		Subagent: &SubagentSpec{Dir: "/tmp/somewhere", Timeout: 30},
	}
	b, err := newSubagentBackend(e, Deps{})
	if err != nil {
		t.Fatalf("newSubagentBackend: %v", err)
	}
	sb := b.(*subagentBackend)
	if sb.timeout != 30*time.Second {
		t.Errorf("timeout = %v, want 30s", sb.timeout)
	}
}

func TestBumpDispatchDepthEnv_UnsetInParent(t *testing.T) {
	env := []string{"PATH=/usr/bin", "HOME=/home/x"}
	got := BumpDispatchDepthEnv(env)
	if !containsEnv(got, "AIDA_DISPATCH_DEPTH=1") {
		t.Errorf("BumpDispatchDepthEnv(%v) = %v, want AIDA_DISPATCH_DEPTH=1", env, got)
	}
}

func TestBumpDispatchDepthEnv_ParentHasTwo(t *testing.T) {
	env := []string{"PATH=/usr/bin", "AIDA_DISPATCH_DEPTH=2", "HOME=/home/x"}
	got := BumpDispatchDepthEnv(env)
	if !containsEnv(got, "AIDA_DISPATCH_DEPTH=3") {
		t.Errorf("BumpDispatchDepthEnv(%v) = %v, want AIDA_DISPATCH_DEPTH=3", env, got)
	}
	if containsEnv(got, "AIDA_DISPATCH_DEPTH=2") {
		t.Errorf("BumpDispatchDepthEnv(%v) = %v, want the old depth entry replaced, not duplicated", env, got)
	}
}

func TestBumpDispatchDepthEnv_UnparseableTreatedAsZero(t *testing.T) {
	env := []string{"AIDA_DISPATCH_DEPTH=not-a-number"}
	got := BumpDispatchDepthEnv(env)
	if !containsEnv(got, "AIDA_DISPATCH_DEPTH=1") {
		t.Errorf("BumpDispatchDepthEnv(%v) = %v, want AIDA_DISPATCH_DEPTH=1 (unparseable treated as 0)", env, got)
	}
}

func containsEnv(env []string, want string) bool {
	for _, kv := range env {
		if kv == want {
			return true
		}
	}
	return false
}

func TestNewSubagentBackend_ExpandsHomeDir(t *testing.T) {
	e := &Entry{
		Name: "x", Kind: KindSubagent,
		Subagent: &SubagentSpec{Dir: "~/dev/butter_stack", Agent: "qa"},
	}
	b, err := newSubagentBackend(e, Deps{})
	if err != nil {
		t.Fatalf("newSubagentBackend: %v", err)
	}
	sb := b.(*subagentBackend)
	if strings.HasPrefix(sb.dir, "~") {
		t.Errorf("dir = %q, want ~ expanded", sb.dir)
	}
	if sb.agent != "qa" {
		t.Errorf("agent = %q, want qa", sb.agent)
	}
}
