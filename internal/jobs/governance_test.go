package jobs

import (
	"reflect"
	"testing"
)

// TestGovernanceRoundTrip exercises the manifest-v4 per-job governance fields:
// SetGovernance stamps SpendCapUSD/TimeoutSec/ToolAllowlist/SandboxTier, and
// they round-trip through BOTH the SQL row (Get) and the on-disk manifest
// (ReadManifest -> manifestToJob), including the JSON-encoded ToolAllowlist.
func TestGovernanceRoundTrip(t *testing.T) {
	withTempHome(t)

	store, err := Open("work")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	j, err := store.Enqueue("loop", "do-thing", "", "do thing", "")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// A fresh job has no governance set.
	if j.SandboxTier != "" || j.TimeoutSec != 0 || j.SpendCapUSD != 0 || j.ToolAllowlist != nil {
		t.Fatalf("fresh job should have zero governance, got %+v", j)
	}

	want := Governance{
		SpendCapUSD:   2.5,
		TimeoutSec:    450,
		ToolAllowlist: []string{"read", "edit", "bash"},
		SandboxTier:   "docker",
	}
	if err := store.SetGovernance(j.RunID, want); err != nil {
		t.Fatalf("SetGovernance: %v", err)
	}

	// Round-trips through SQL.
	got, err := store.Get(j.RunID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.SpendCapUSD != want.SpendCapUSD || got.TimeoutSec != want.TimeoutSec || got.SandboxTier != want.SandboxTier {
		t.Errorf("governance scalars lost through SQL: %+v", got)
	}
	if !reflect.DeepEqual(got.ToolAllowlist, want.ToolAllowlist) {
		t.Errorf("tool allowlist lost through SQL: got %v want %v", got.ToolAllowlist, want.ToolAllowlist)
	}

	// Round-trips through the on-disk manifest too.
	m, err := ReadManifest("work", j.RunID)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if m.Version != manifestVersion {
		t.Errorf("manifest version = %d, want %d", m.Version, manifestVersion)
	}
	if m.SandboxTier != "docker" || m.TimeoutSec != 450 || m.SpendCapUSD != 2.5 {
		t.Errorf("governance not in manifest: %+v", m)
	}
	if !reflect.DeepEqual(manifestToJob(m).ToolAllowlist, want.ToolAllowlist) {
		t.Errorf("tool allowlist lost through manifest: %v", m.ToolAllowlist)
	}
}

// TestGovernanceEmptyAllowlistIsNil verifies that an unset/empty allowlist
// encodes to "" and decodes back to nil (not an empty non-nil slice), so the
// default-empty column and "no allowlist" are indistinguishable on read.
func TestGovernanceEmptyAllowlistIsNil(t *testing.T) {
	if got := encodeAllowlist(nil); got != "" {
		t.Errorf("encodeAllowlist(nil) = %q, want \"\"", got)
	}
	if got := encodeAllowlist([]string{}); got != "" {
		t.Errorf("encodeAllowlist([]) = %q, want \"\"", got)
	}
	if got := decodeAllowlist(""); got != nil {
		t.Errorf("decodeAllowlist(\"\") = %v, want nil", got)
	}
	if got := decodeAllowlist(`["a","b"]`); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("decodeAllowlist = %v", got)
	}
}
