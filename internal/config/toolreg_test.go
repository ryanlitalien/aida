package config

import "testing"

// TestInjectAvailableTools_CurrentTimeAlwaysInjected verifies that the
// current-time builtin (SelfContained: true, no Exec templates) is
// injected unconditionally -- it isn't a CLI wrapper, so there's nothing
// on PATH to gate it on.
func TestInjectAvailableTools_CurrentTimeAlwaysInjected(t *testing.T) {
	sources := make(Sources)
	InjectAvailableTools(sources)

	src, ok := sources["current-time"]
	if !ok {
		t.Fatal("expected 'current-time' source to be injected unconditionally")
	}
	if src.Type != "current-time" {
		t.Errorf("expected type 'current-time', got %q", src.Type)
	}
	if !src.HasCapability("current-time") {
		t.Error("expected current-time source to have 'current-time' capability")
	}
}

// TestInjectAvailableTools_ExplicitSourceTakesPrecedence verifies that a
// user-defined "current-time" source in sources.yaml is never clobbered by
// the builtin.
func TestInjectAvailableTools_ExplicitSourceTakesPrecedence(t *testing.T) {
	sources := Sources{
		"current-time": {
			Type:        "custom",
			Description: "user override",
		},
	}
	InjectAvailableTools(sources)

	if sources["current-time"].Type != "custom" {
		t.Errorf("explicit source should not be overwritten, got type %q", sources["current-time"].Type)
	}
}

// TestInjectAvailableTools_NoExecButNotSelfContainedStillGatedByPath is a
// regression test: "no Exec templates" alone must NOT bypass the PATH
// check. A builtin can declare no Exec templates (its adapter builds
// commands programmatically) but still require its CLI binary on PATH. A
// fake registry entry shaped the same way (no Exec, SelfContained left
// false) must behave identically -- injected only when its "binary" is
// actually found on PATH.
func TestInjectAvailableTools_NoExecButNotSelfContainedStillGatedByPath(t *testing.T) {
	const fakeToolName = "aida-toolreg-test-nonexistent-binary"
	BuiltinToolSources[fakeToolName] = &Source{
		Type:        "tool",
		Description: "fake PATH-gated builtin with no Exec templates",
	}
	defer delete(BuiltinToolSources, fakeToolName)

	sources := make(Sources)
	InjectAvailableTools(sources)

	if _, ok := sources[fakeToolName]; ok {
		t.Error("expected non-self-contained builtin with no Exec templates to stay PATH-gated (binary does not exist), but it was injected")
	}
}
