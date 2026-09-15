package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ryanlitalien/aida/internal/models"
)

// registerModelsRoutesOnMux is a small test helper: wraps a plain
// http.ServeMux's HandleFunc as the `handle` registerModelsRoutes wants,
// with none of guardLocal's origin/content-type checks -- this file tests
// registerModelsRoutes directly, not through registerDashboardRoutes, so
// the probe function passed in is always the test's own fake and the
// real keychain/ssh/network are never touched regardless of what the
// fixture roster's providers declare.
func registerModelsRoutesOnMux(mux *http.ServeMux, path string, probeFn func(context.Context, *models.Roster, bool) []models.Usage) {
	registerModelsRoutes(func(pattern string, h http.HandlerFunc) { mux.HandleFunc(pattern, h) }, path, probeFn)
}

func writeTestModelsRoster(t *testing.T, yamlContent string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.yaml")
	if err := os.WriteFile(path, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("write roster fixture: %v", err)
	}
	return path
}

const testModelsRosterYAML = `updated: 2026-09-08
guidance:
  - "test guidance line"
providers:
  - name: testprov
    label: Test Provider
    plan: Test Plan
    account: test@example.com
    probe: none
    models:
      - id: test-model
        nicknames: [tm]
`

func fakeModelsProbe(_ context.Context, r *models.Roster, _ bool) []models.Usage {
	out := make([]models.Usage, len(r.Providers))
	for i, p := range r.Providers {
		out[i] = models.Usage{Provider: p.Name, Plan: p.Plan}
	}
	return out
}

func TestModelsEndpointServesJSON(t *testing.T) {
	rosterPath := writeTestModelsRoster(t, testModelsRosterYAML)

	mux := http.NewServeMux()
	registerModelsRoutesOnMux(mux, rosterPath, fakeModelsProbe)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/models")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Errorf("Content-Type = %q, want json", ct)
	}

	var got modelsAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Updated != "2026-09-08" {
		t.Errorf("Updated = %q, want 2026-09-08", got.Updated)
	}
	if len(got.Guidance) != 1 || got.Guidance[0] != "test guidance line" {
		t.Errorf("Guidance = %v, want [\"test guidance line\"]", got.Guidance)
	}
	if len(got.Providers) != 1 {
		t.Fatalf("len(Providers) = %d, want 1", len(got.Providers))
	}
	p := got.Providers[0]
	if p.Name != "testprov" {
		t.Errorf("Providers[0].Name = %q, want testprov", p.Name)
	}
	if p.Usage.Plan != "Test Plan" {
		t.Errorf("Providers[0].Usage.Plan = %q, want \"Test Plan\"", p.Usage.Plan)
	}
	if len(p.Models) != 1 || p.Models[0].ID != "test-model" {
		t.Errorf("Providers[0].Models = %v, want one test-model entry", p.Models)
	}
}

func TestModelsEndpointMissingRosterIs500(t *testing.T) {
	mux := http.NewServeMux()
	registerModelsRoutesOnMux(mux, filepath.Join(t.TempDir(), "nope.yaml"), fakeModelsProbe)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/models")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 for a missing roster file", resp.StatusCode)
	}
}

func TestModelsEndpointCachesAndRefreshInvalidates(t *testing.T) {
	rosterPath := writeTestModelsRoster(t, testModelsRosterYAML)

	var calls int32
	var lastForce atomic.Bool
	countingProbe := func(ctx context.Context, r *models.Roster, force bool) []models.Usage {
		atomic.AddInt32(&calls, 1)
		lastForce.Store(force)
		return fakeModelsProbe(ctx, r, force)
	}

	mux := http.NewServeMux()
	registerModelsRoutesOnMux(mux, rosterPath, countingProbe)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	get := func() {
		resp, err := http.Get(srv.URL + "/api/models")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		resp.Body.Close()
	}

	get()
	get()
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("probe calls after 2 GETs = %d, want 1 (second GET should hit the cache)", got)
	}
	if lastForce.Load() {
		t.Errorf("force = true on a plain GET, want false")
	}

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/models/refresh", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST refresh: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("refresh status = %d, want 200", resp.StatusCode)
	}

	get()
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("probe calls after refresh = %d, want 2 (refresh must invalidate the cache)", got)
	}
	if !lastForce.Load() {
		t.Errorf("force = false on the GET after POST /api/models/refresh, want true (refresh must force every provider's own probe too)")
	}
}
