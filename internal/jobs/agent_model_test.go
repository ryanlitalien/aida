package jobs

import "testing"

// TestSetAgentModelRoundTrip exercises the manifest-v5 fleet-watch fields:
// SetAgentModel stamps Agent/Model, and they round-trip through BOTH the SQL
// row (Get) and the on-disk manifest (ReadManifest -> manifestToJob).
// Mirrors TestGovernanceRoundTrip's shape for the v4 fields.
func TestSetAgentModelRoundTrip(t *testing.T) {
	withTempHome(t)

	store, err := Open("work")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	j, err := store.Enqueue("fleet", "", "", "tracer: issue #978", "fleet:minty:ryan:scarlett-978")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if j.Agent != "" || j.Model != "" {
		t.Fatalf("fresh job should have empty Agent/Model, got %+v", j)
	}

	if err := store.SetAgentModel(j.RunID, "scarlett-978", "bedrock-opus-4-6"); err != nil {
		t.Fatalf("SetAgentModel: %v", err)
	}

	got, err := store.Get(j.RunID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Agent != "scarlett-978" || got.Model != "bedrock-opus-4-6" {
		t.Errorf("agent/model lost through SQL: %+v", got)
	}

	m, err := ReadManifest("work", j.RunID)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if m.Version != manifestVersion {
		t.Errorf("manifest version = %d, want %d", m.Version, manifestVersion)
	}
	if m.Agent != "scarlett-978" || m.Model != "bedrock-opus-4-6" {
		t.Errorf("agent/model not in manifest: %+v", m)
	}
}

// TestCompletePreservesAgentModelAndArtifactURL is a regression test for the
// manifest-rewrite hazard `aida fleet watch` used to depend on ordering
// around: Complete/Fail rewrites the ENTIRE manifest from the Job row
// (jobToManifest). Before manifest v6, ArtifactURL was not a Job/SQL field,
// so a Complete/Fail that ran after SetArtifactURL would silently drop it.
// Now that ArtifactURL, Agent, and Model are ALL Job/SQL fields, all three
// must survive Complete regardless of call order -- this test uses the
// order `aida fleet watch` happens to use (Complete then SetArtifactURL),
// TestSetArtifactURLSurvivesEitherOrderAroundComplete below covers the
// reverse.
func TestCompletePreservesAgentModelAndArtifactURL(t *testing.T) {
	withTempHome(t)

	store, err := Open("work")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	j, err := store.Enqueue("fleet", "", "", "tracer: issue #978", "fleet:minty:ryan:scarlett-978")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	host := "test-host"
	if err := store.MarkRunning(j.RunID, host, 1); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	if err := store.SetAgentModel(j.RunID, "scarlett-978", "bedrock-opus-4-6"); err != nil {
		t.Fatalf("SetAgentModel: %v", err)
	}

	if err := store.Complete(j.RunID); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	prURL := "https://github.com/ButterStack/butter_stack/pull/1617"
	if err := store.SetArtifactURL(j.RunID, prURL); err != nil {
		t.Fatalf("SetArtifactURL: %v", err)
	}

	got, err := store.Get(j.RunID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != StateDone {
		t.Errorf("State = %q, want %q", got.State, StateDone)
	}
	if got.Agent != "scarlett-978" || got.Model != "bedrock-opus-4-6" {
		t.Errorf("Complete's manifest rewrite dropped agent/model: %+v", got)
	}
	if got.ArtifactURL != prURL {
		t.Errorf("artifact_url lost through SQL: got %q want %q", got.ArtifactURL, prURL)
	}

	m, err := ReadManifest("work", j.RunID)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if m.ArtifactURL != prURL {
		t.Errorf("ArtifactURL = %q, want %q", m.ArtifactURL, prURL)
	}
	if m.Agent != "scarlett-978" || m.Model != "bedrock-opus-4-6" {
		t.Errorf("agent/model not in final manifest: %+v", m)
	}
}

// TestSetArtifactURLSurvivesEitherOrderAroundComplete is the reverse of the
// test above: SetArtifactURL BEFORE Complete used to be the exact hazard
// (Complete's WriteManifest(jobToManifest(j)) would rebuild the manifest
// from a Job that had no ArtifactURL field, silently dropping it). Now that
// ArtifactURL is a Job/SQL field (manifest v6), Complete's own Get+
// jobToManifest round-trip carries it forward, so this order works too --
// `aida fleet watch`'s ordering comment in internal/cli/fleet.go notes it
// no longer matters which order these two calls happen in.
func TestSetArtifactURLSurvivesEitherOrderAroundComplete(t *testing.T) {
	withTempHome(t)

	store, err := Open("work")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	j, err := store.Enqueue("fleet", "", "", "tracer: issue #978", "")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := store.MarkRunning(j.RunID, "test-host", 1); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}

	prURL := "https://github.com/ButterStack/butter_stack/pull/1617"
	// SetArtifactURL BEFORE Complete this time.
	if err := store.SetArtifactURL(j.RunID, prURL); err != nil {
		t.Fatalf("SetArtifactURL: %v", err)
	}
	if err := store.Complete(j.RunID); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	m, err := ReadManifest("work", j.RunID)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if m.ArtifactURL != prURL {
		t.Fatalf("ArtifactURL = %q, want %q (Complete should no longer clobber a pre-set artifact url)", m.ArtifactURL, prURL)
	}
}

// TestCompleteThenMarkNotifiedKeepsArtifactURL is the exact bug report this
// fix addresses: on the freshly installed daemon (post-restart, so the
// clobber wasn't just an old-binary artifact), a job that called
// SetArtifactURL then went through Complete still lost artifact_url about a
// second later -- MarkNotified is ALSO a Get+jobToManifest+WriteManifest
// rewrite (same shape as Complete/Fail), fired by the daemon's
// watchJobsForNotifications loop on every terminal job. Mirroring
// ArtifactURL onto the Job/SQL row fixes every one of these rewrite paths
// at once, not just Complete/Fail; this test exercises the exact sequence
// the daemon runs: Complete, then SetArtifactURL, then MarkNotified.
func TestCompleteThenMarkNotifiedKeepsArtifactURL(t *testing.T) {
	withTempHome(t)

	store, err := Open("work")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	j, err := store.Enqueue("fleet", "", "", "tracer: issue #978", "fleet:minty:ryan:scarlett-978")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := store.MarkRunning(j.RunID, "test-host", 1); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	if err := store.SetAgentModel(j.RunID, "scarlett-978", "bedrock-opus-4-6"); err != nil {
		t.Fatalf("SetAgentModel: %v", err)
	}
	if err := store.Complete(j.RunID); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	prURL := "https://github.com/ButterStack/butter_stack/pull/1617"
	if err := store.SetArtifactURL(j.RunID, prURL); err != nil {
		t.Fatalf("SetArtifactURL: %v", err)
	}

	// This is what watchJobsForNotifications does on its next 2s tick for
	// any terminal job with NotifiedAt == "" -- exactly what wiped
	// artifact_url (and, before manifest v5, agent/model) in production.
	if err := store.MarkNotified(j.RunID, "2026-09-02T21:43:04Z"); err != nil {
		t.Fatalf("MarkNotified: %v", err)
	}

	m, err := ReadManifest("work", j.RunID)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if m.ArtifactURL != prURL {
		t.Errorf("MarkNotified dropped artifact_url: got %q want %q", m.ArtifactURL, prURL)
	}
	if m.Agent != "scarlett-978" || m.Model != "bedrock-opus-4-6" {
		t.Errorf("MarkNotified dropped agent/model: %+v", m)
	}
	if m.NotifiedAt == "" {
		t.Errorf("NotifiedAt not stamped by MarkNotified")
	}

	row, err := store.Get(j.RunID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.ArtifactURL != prURL {
		t.Errorf("artifact_url lost from SQL after MarkNotified: got %q want %q", row.ArtifactURL, prURL)
	}
}

// TestSetDestinationSurvivesCompleteAndMarkNotified is the Destination
// analogue of TestCompleteThenMarkNotifiedKeepsArtifactURL (#437):
// Destination was manifest-only, so Complete/Fail/SetGovernance/
// SetWorktreePath/MarkNotified/SetArtifactURL -- every rewrite path
// that rebuilds the manifest from a Job via jobToManifest -- silently
// dropped a previously-set Destination, the exact same bug class
// ArtifactURL had before manifest v6. Now that Destination is a
// Job/SQL field too (manifest v7, via Store.SetDestination), it must
// survive Complete and MarkNotified just like ArtifactURL does.
func TestSetDestinationSurvivesCompleteAndMarkNotified(t *testing.T) {
	withTempHome(t)

	store, err := Open("work")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	j, err := store.Enqueue("draft", "clean-up-actions", "", "clean up github actions", "")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := store.MarkRunning(j.RunID, "test-host", 1); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}

	dest := &Destination{
		Type:         DestinationTypeGitHubPR,
		Repo:         "ryanlitalien/butter_stack",
		BranchPrefix: "aida/",
	}
	if err := store.SetDestination(j.RunID, dest); err != nil {
		t.Fatalf("SetDestination: %v", err)
	}

	if err := store.Complete(j.RunID); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if err := store.MarkNotified(j.RunID, "2026-09-02T21:43:04Z"); err != nil {
		t.Fatalf("MarkNotified: %v", err)
	}

	m, err := ReadManifest("work", j.RunID)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if m.Destination == nil {
		t.Fatalf("Complete+MarkNotified dropped Destination from the manifest")
	}
	if *m.Destination != *dest {
		t.Errorf("Destination changed: got %+v want %+v", *m.Destination, *dest)
	}

	row, err := store.Get(j.RunID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.Destination == nil {
		t.Fatalf("destination lost from SQL after Complete+MarkNotified")
	}
	if *row.Destination != *dest {
		t.Errorf("SQL destination changed: got %+v want %+v", *row.Destination, *dest)
	}

	// Idempotent re-apply (same Destination) -- must not error, must
	// not change on-disk state.
	if err := store.SetDestination(j.RunID, dest); err != nil {
		t.Fatalf("SetDestination idempotent: %v", err)
	}
}
