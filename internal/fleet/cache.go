package fleet

import (
	"context"
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/ryanlitalien/aida/internal/config"
)

// Cache holds the most recent fleet Snapshot and knows how to refresh it
// in the background. It is the piece that makes probing safe to call from
// an HTTP handler: Snapshot never touches the network itself.
type Cache struct {
	prober  *Prober
	devices []config.DeviceConfig
	fc      config.FleetConfig

	// now is injectable so staleness tests can jump the clock instead of
	// sleeping for real TTLs.
	now func() time.Time

	mu       sync.RWMutex
	snapshot Snapshot
	haveSnap bool

	// sf collapses concurrent Refresh() calls onto a single in-flight
	// probe round. This is not about the data race on snapshot (mu
	// already owns that) -- it's about probe pile-up: two browser tabs
	// polling every few seconds against a probe timeout of similar
	// magnitude would otherwise each open their own round of ssh
	// connections to a host that's already timing out, compounding the
	// exact slowness they're both trying to observe.
	sf singleflight.Group
}

// NewCache builds a Cache. No probing happens until Snapshot or Refresh is
// called -- construction alone makes zero network calls.
func NewCache(p *Prober, devices []config.DeviceConfig, fc config.FleetConfig) *Cache {
	return &Cache{
		prober:  p,
		devices: devices,
		fc:      fc,
		now:     time.Now,
	}
}

// Snapshot returns the fleet's current state. It NEVER blocks on I/O:
//   - Fresh (last probe round within TTL): returned as-is.
//   - Stale (past TTL): returned as-is but with Stale: true stamped onto
//     every device, plus an async refresh kicked off.
//   - Cold (no probe round has ever completed): placeholder cards with
//     Reach: ReachUnknown are returned immediately, plus an async refresh
//     kicked off.
//
// The dashboard polls this on an interval. If Snapshot could block, one
// sleeping laptop would make every single poll take the full probe
// timeout, and the whole page would feel dead even though every other
// device answered in milliseconds. Blocking is exactly what Refresh's
// async goroutine + singleflight dedup exists to avoid pushing onto the
// request path.
func (c *Cache) Snapshot(ctx context.Context) Snapshot {
	c.mu.RLock()
	snap := c.snapshot
	have := c.haveSnap
	c.mu.RUnlock()

	if !have {
		c.Refresh()
		return c.coldSnapshot()
	}

	if c.now().Sub(snap.GeneratedAt) > c.fc.TTL() {
		stale := snap
		stale.Devices = append([]DeviceStatus(nil), snap.Devices...)
		for i := range stale.Devices {
			stale.Devices[i].Stale = true
		}
		stale.Refreshing = true
		c.Refresh()
		return stale
	}

	return snap
}

// coldSnapshot builds the placeholder Snapshot returned before any probe
// round has ever completed: one card per configured device, all
// Reach: ReachUnknown, Aida: nil ("we don't know" -- not "known down").
func (c *Cache) coldSnapshot() Snapshot {
	devices := make([]DeviceStatus, len(c.devices))
	for i, d := range c.devices {
		devices[i] = DeviceStatus{
			Name:  d.Name,
			Role:  d.Role,
			Probe: d.ProbeMode(),
			Self:  d.Self,
			Herdr: d.Herdr,
			Notes: d.Notes,
			Reach: ReachUnknown,
		}
	}
	return Snapshot{
		Devices:     devices,
		GeneratedAt: c.now(),
		Refreshing:  true,
		TailscaleOK: false,
		// Machines is meaningful even before any probe round has run (it's
		// just a count of configured non-wsl devices); the numeric sums
		// all come out 0 since none of these placeholders has Hardware
		// yet.
		Totals: ComputeFleetTotals(devices),
	}
}

// Refresh kicks off an async probe round if one isn't already running,
// and returns immediately either way. Concurrent callers collapse onto a
// single round via singleflight (see the sf field comment).
func (c *Cache) Refresh() {
	go func() {
		// The goroutine, not singleflight.Do itself, is what makes this
		// call asynchronous from the caller's point of view: Do blocks
		// until the round it joins (or starts) finishes, so it must run
		// off of the caller's own goroutine.
		_, _, _ = c.sf.Do("refresh", func() (any, error) {
			c.doRefresh()
			return nil, nil
		})
	}()
}

// Run hosts a background ticker that calls Refresh every interval until
// ctx is cancelled. interval <= 0 means "no ticker" (poll-on-demand only,
// e.g. config.FleetConfig.RefreshSeconds < 0), matching
// FleetConfig.RefreshInterval's documented contract.
func (c *Cache) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.Refresh()
		}
	}
}

// doRefresh runs one full probe round -- the shared tailscale status call
// followed by a bounded fan-out over every device -- and publishes the
// result. It's synchronous; Refresh is what makes it non-blocking for
// callers.
func (c *Cache) doRefresh() {
	ctx := context.Background()

	tsCtx, cancel := context.WithTimeout(ctx, c.fc.ProbeTimeout())
	nodes, tsErr := c.prober.TailscaleStatus(tsCtx)
	cancel()

	var warnings []string
	if tsErr != nil {
		warnings = append(warnings, fmt.Sprintf("tailscale status: %v", tsErr))
	}

	// Each goroutine below writes to devices[i] and nowhere else, and
	// wg.Wait() is the happens-before edge that publishes every one of
	// those writes to this (the reading) goroutine -- so the slice itself
	// needs no mutex, only Cache.snapshot does once we're done building
	// it.
	devices := make([]DeviceStatus, len(c.devices))
	sem := make(chan struct{}, c.fc.Concurrency())
	var wg sync.WaitGroup
	for i, d := range c.devices {
		wg.Add(1)
		sem <- struct{}{} // blocks here once Concurrency() probes are in flight
		go func(i int, d config.DeviceConfig) {
			defer wg.Done()
			defer func() { <-sem }()

			// Per-host timeout so one slow/sleeping host can't eat into
			// another host's probe budget.
			hostCtx, hostCancel := context.WithTimeout(ctx, c.fc.ProbeTimeout())
			defer hostCancel()
			devices[i] = c.prober.ProbeOne(hostCtx, d, nodes)
		}(i, d)
	}
	wg.Wait()

	snap := Snapshot{
		Devices:     devices,
		GeneratedAt: c.now(),
		Refreshing:  false,
		TailscaleOK: tsErr == nil,
		Warnings:    warnings,
		Totals:      ComputeFleetTotals(devices),
	}

	c.mu.Lock()
	c.snapshot = snap
	c.haveSnap = true
	c.mu.Unlock()
}
