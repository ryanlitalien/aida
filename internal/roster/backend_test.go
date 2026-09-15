package roster

import (
	"strings"
	"testing"
)

func TestBackendFor_AidaIsNotADispatchTarget(t *testing.T) {
	r := &Roster{}
	aida := &Entry{Name: "aida", CallSign: "Aida", Kind: KindAida}
	_, err := r.BackendFor(aida, Deps{})
	if err == nil {
		t.Fatal("expected an error for kind aida")
	}
	if !strings.Contains(err.Error(), "orchestrator") {
		t.Errorf("error = %q, want to mention the orchestrator", err.Error())
	}
}

func TestBackendFor_UnknownKind(t *testing.T) {
	r := &Roster{}
	e := &Entry{Name: "mystery", Kind: "not-a-real-kind"}
	_, err := r.BackendFor(e, Deps{})
	if err == nil {
		t.Fatal("expected an error for an unregistered kind")
	}
}

func TestBackendFor_Subagent(t *testing.T) {
	r := &Roster{}
	e := &Entry{
		Name:     "workouts",
		Kind:     KindSubagent,
		Subagent: &SubagentSpec{Dir: "/tmp/workouts"},
	}
	b, err := r.BackendFor(e, Deps{})
	if err != nil {
		t.Fatalf("BackendFor: %v", err)
	}
	if b.Kind() != KindSubagent {
		t.Errorf("Kind() = %q, want %q", b.Kind(), KindSubagent)
	}
}
