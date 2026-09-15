package cli

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/ryanlitalien/aida/internal/models"
)

// modelsCacheTTL bounds how long GET /api/models serves a cached result
// before re-probing. A dashboard tab can poll every few seconds; without
// this, that would re-run every provider's keychain/ssh/network probe on
// every single poll.
const modelsCacheTTL = 60 * time.Second

// modelsCache holds the most recently computed modelsAPIResponse behind
// modelsCacheTTL. There is exactly one roster (one path) per daemon, so
// unlike flowCache (keyed by window/limit) this only ever needs to hold
// one entry.
type modelsCache struct {
	mu        sync.Mutex
	resp      modelsAPIResponse
	have      bool
	expiresAt time.Time
	// forceNext is set by invalidate() and consumed by the very next
	// get(), regardless of which caller triggers it: POST
	// /api/models/refresh must force a full re-probe of every provider
	// (bypassing each one's own probeTTLFor cache, not just this outer
	// 60s cache), not merely let the next GET land inside a provider's
	// still-fresh TTL window and quietly serve near-identical numbers.
	forceNext bool
}

func newModelsCache() *modelsCache { return &modelsCache{} }

// modelsProbeAdapter is the production probeFn registerModelsRoutes runs:
// a thin force-bool wrapper over models.ProbeWithOptions so
// registerDashboardRoutes can wire it in directly (see dashboard_web.go).
func modelsProbeAdapter(ctx context.Context, r *models.Roster, force bool) []models.Usage {
	return models.ProbeWithOptions(ctx, r, models.ProbeOptions{Force: force})
}

// get returns the cached response when it's still fresh and forceRefresh
// is false; otherwise it reloads the roster from path and re-probes
// (via probeFn), caches the result for modelsCacheTTL, and returns it.
// probeFn's bool argument is "force every provider's own probe, ignoring
// its per-provider TTL cache" -- true when this call is forceRefresh
// (?refresh=1) or a pending invalidate()-set forceNext (POST
// /api/models/refresh), false for a plain poll.
func (c *modelsCache) get(ctx context.Context, path string, probeFn func(context.Context, *models.Roster, bool) []models.Usage, forceRefresh bool) (modelsAPIResponse, error) {
	c.mu.Lock()
	if !forceRefresh && c.have && time.Now().Before(c.expiresAt) {
		resp := c.resp
		c.mu.Unlock()
		return resp, nil
	}
	force := forceRefresh || c.forceNext
	c.forceNext = false
	c.mu.Unlock()

	r, err := models.Load(path)
	if err != nil {
		return modelsAPIResponse{}, err
	}
	var usages []models.Usage
	if probeFn != nil {
		usages = probeFn(ctx, r, force)
	} else {
		usages = make([]models.Usage, len(r.Providers))
	}
	resp := buildModelsResponse(r, usages)

	c.mu.Lock()
	c.resp = resp
	c.have = true
	c.expiresAt = time.Now().Add(modelsCacheTTL)
	c.mu.Unlock()
	return resp, nil
}

// invalidate drops the cached response and arms forceNext, so the next
// GET (from anyone) re-probes every provider unconditionally.
func (c *modelsCache) invalidate() {
	c.mu.Lock()
	c.have = false
	c.forceNext = true
	c.mu.Unlock()
}

// registerModelsRoutes mounts GET /api/models and POST /api/models/refresh.
// probeFn is a thin models.ProbeWithOptions adapter in production
// (registerDashboardRoutes wires it that way); tests pass a fake so they
// never touch the real keychain, ssh, or network -- see
// TestModelsEndpointServesJSON.
//
// GET /api/models serves the roster at path plus each provider's live
// Usage, behind modelsCache's 60s TTL (itself layered on top of each
// provider's own probeTTLFor cache in internal/models); ?refresh=1
// bypasses both caches for that one request without invalidating them for
// other callers. POST /api/models/refresh invalidates the outer cache
// AND forces the next GET's per-provider probing outright; the next GET
// (from anyone) re-probes everything.
func registerModelsRoutes(handle func(string, http.HandlerFunc), path string, probeFn func(context.Context, *models.Roster, bool) []models.Usage) {
	cache := newModelsCache()

	handle("GET /api/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		forceRefresh := r.URL.Query().Get("refresh") == "1"
		resp, err := cache.get(r.Context(), path, probeFn, forceRefresh)
		if err != nil {
			httpError(w, http.StatusInternalServerError, "loading models roster: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, resp)
	})

	handle("POST /api/models/refresh", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		cache.invalidate()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
}
