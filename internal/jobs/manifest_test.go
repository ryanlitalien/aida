package jobs

import (
	"testing"
)

// TestManifestDestinationRoundTrip exercises the issue #58 fields:
// Destination is preserved through Write→Read, and an absent
// destination stays nil rather than becoming a zero-struct (the
// `omitempty` JSON tag depends on this for clean manifests).
func TestManifestDestinationRoundTrip(t *testing.T) {
	withTempHome(t)

	m := &Manifest{
		Version:    manifestVersion,
		RunID:      "20260511-aaaaaa",
		Kind:       "draft",
		Profile:    "work",
		State:      StateQueued,
		EnqueuedAt: "2026-05-11T12:00:00Z",
		Destination: &Destination{
			Type:         DestinationTypeGitHubPR,
			Repo:         "ryanlitalien/butter_stack",
			BranchPrefix: "aida/",
		},
	}
	if err := WriteManifest("work", m); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}

	got, err := ReadManifest("work", m.RunID)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if got.Destination == nil {
		t.Fatalf("destination lost in round-trip")
	}
	if got.Destination.Type != DestinationTypeGitHubPR {
		t.Errorf("type: want %q got %q", DestinationTypeGitHubPR, got.Destination.Type)
	}
	if got.Destination.Repo != "ryanlitalien/butter_stack" {
		t.Errorf("repo: got %q", got.Destination.Repo)
	}
	if got.Destination.BranchPrefix != "aida/" {
		t.Errorf("branch_prefix: got %q", got.Destination.BranchPrefix)
	}
	if got.ArtifactURL != "" {
		t.Errorf("artifact_url should default empty, got %q", got.ArtifactURL)
	}
}

func TestManifestDestinationNilOmitted(t *testing.T) {
	withTempHome(t)
	m := &Manifest{
		Version:    manifestVersion,
		RunID:      "20260511-bbbbbb",
		Kind:       "draft",
		Profile:    "work",
		State:      StateQueued,
		EnqueuedAt: "2026-05-11T12:00:00Z",
		// Destination intentionally nil
	}
	if err := WriteManifest("work", m); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	got, err := ReadManifest("work", m.RunID)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if got.Destination != nil {
		t.Errorf("nil destination round-tripped as non-nil: %#v", got.Destination)
	}
}

// TestSetArtifactURL exercises Store.SetArtifactURL (manifest v6: the
// field lives on the Job/SQL row now, not just the manifest -- see
// internal/jobs/agent_model_test.go for the Complete/MarkNotified
// survival tests that motivated the move). It must:
//  1. read -> mutate -> write atomically (manifest stays valid),
//  2. preserve other Job-backed fields (task_slug),
//  3. be idempotent on the same URL,
//  4. round-trip through the SQL row too, not just the manifest.
//
// Note: Destination is intentionally NOT exercised here. As of
// manifest v7 it is also a Job/SQL field (see Store.SetDestination),
// so it now survives Store method rewrites the same way ArtifactURL
// does; internal/jobs/agent_model_test.go covers that survival case
// directly (TestSetDestinationSurvivesCompleteAndMarkNotified).
func TestSetArtifactURL(t *testing.T) {
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

	prURL := "https://github.com/ryanlitalien/butter_stack/pull/42"
	if err := store.SetArtifactURL(j.RunID, prURL); err != nil {
		t.Fatalf("SetArtifactURL: %v", err)
	}

	got, err := ReadManifest("work", j.RunID)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if got.ArtifactURL != prURL {
		t.Errorf("artifact_url: want %q got %q", prURL, got.ArtifactURL)
	}
	if got.TaskSlug != "clean-up-actions" {
		t.Errorf("task_slug clobbered: %q", got.TaskSlug)
	}

	// Idempotent re-apply (same URL) -- must not error, must not
	// change the on-disk state's URL.
	if err := store.SetArtifactURL(j.RunID, prURL); err != nil {
		t.Fatalf("SetArtifactURL idempotent: %v", err)
	}

	// Round-trips through SQL too, not just the manifest.
	row, err := store.Get(j.RunID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.ArtifactURL != prURL {
		t.Errorf("artifact_url lost through SQL: got %q want %q", row.ArtifactURL, prURL)
	}
}
