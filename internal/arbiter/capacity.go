package arbiter

import (
	"strings"

	"github.com/ryanlitalien/aida/internal/burndown"
)

// WindowKey maps a burndown.Capacity row's Label to the window key a
// Lane's Windows list names ("5h", "7d", "7d-fable", "image"). This is an
// exported twin of internal/burndown's own unexported windowKeyForLabel
// (burndown intentionally doesn't export it -- see capacity.go there),
// kept in lock-step with it by inspection since floors and lanes both key
// off the exact same set of live bar labels. An unrecognized label
// returns "", the same "never blocks, never matches" convention
// burndown's own version uses.
func WindowKey(label string) string {
	lower := strings.ToLower(label)
	switch {
	case strings.Contains(lower, "fable"):
		return "7d-fable"
	case strings.HasPrefix(lower, "5-hour"):
		return "5h"
	case strings.HasPrefix(lower, "7-day"):
		return "7d"
	case strings.Contains(lower, "image"):
		return "image"
	default:
		return ""
	}
}

// LaneCapacity is one lane's live headroom, assembled from the subset of
// a burndown.Report run that matches the lane's Provider and Windows (or
// Spend rows, for a lane with Spend set).
type LaneCapacity struct {
	Lane string
	// Windows are the matched burndown.Capacity rows -- one per lane
	// window that a live row exists for. A lane with Spend true has at
	// most one entry here (the provider's spend row).
	Windows []burndown.Capacity
	// Probed is true when burndown.Report returned at least one row for
	// this lane's Provider at all (regardless of whether every window
	// the lane wants was among them) -- false means the provider was
	// never probed, or has no Provider configured (e.g. qwen-ec2), and
	// the lane is only eligible with AllowUnprobed.
	Probed bool
	// MinHeadroom is the lane's EffectiveMinHeadroomPct(), carried along
	// so a caller building a rejection message doesn't need the Lane
	// value again.
	MinHeadroom float64
	// MaxUsedPct is the highest UsedPct across Windows, 0 when Windows is
	// empty.
	MaxUsedPct float64
	// Missing lists lane window keys (or "spend" for a Spend lane) that
	// Probed found no matching row for -- a provider that IS probed but
	// doesn't carry every window the lane wants (e.g. only a 5-hour bar
	// when the lane also wants a 7-day one).
	Missing []string
}

// LaneCapacityFor assembles a LaneCapacity for lane from caps -- see the
// LaneCapacity type's field docs for exactly what Probed/Missing mean.
//
// Named LaneCapacityFor rather than LaneCapacity (the build brief's
// working name) because Go doesn't allow a function and a type to share
// one identifier in the same package scope -- see this package's design
// notes in the implementation report for why LaneCapacityFor was picked
// over renaming the type instead: Pick and its rejection-reason strings
// read more naturally referencing a "LaneCapacity" value than a
// "Capacity" or "LaneUsage" one.
func LaneCapacityFor(lane Lane, caps []burndown.Capacity) LaneCapacity {
	lc := LaneCapacity{Lane: lane.ID, MinHeadroom: lane.EffectiveMinHeadroomPct()}

	var providerRows []burndown.Capacity
	for _, c := range caps {
		if c.Provider == lane.Provider {
			providerRows = append(providerRows, c)
		}
	}
	lc.Probed = len(providerRows) > 0

	if lane.Spend {
		for _, c := range providerRows {
			if c.IsSpend {
				lc.Windows = append(lc.Windows, c)
			}
		}
		if len(lc.Windows) == 0 && lc.Probed {
			lc.Missing = append(lc.Missing, "spend")
		}
	} else {
		for _, w := range lane.Windows {
			found := false
			for _, c := range providerRows {
				if c.IsSpend {
					continue
				}
				if WindowKey(c.Label) == w {
					lc.Windows = append(lc.Windows, c)
					found = true
				}
			}
			if !found {
				lc.Missing = append(lc.Missing, w)
			}
		}
	}

	for _, c := range lc.Windows {
		if c.UsedPct > lc.MaxUsedPct {
			lc.MaxUsedPct = c.UsedPct
		}
	}
	return lc
}

// NearExhaustion reports whether any of lc's matched windows has reached
// atPct used -- the signal that arms a HANDOFF.md (plan decision 1).
// Callers normally pass Lane.EffectiveHandoffAtPct() for atPct.
func NearExhaustion(lc LaneCapacity, atPct float64) bool {
	for _, c := range lc.Windows {
		if c.UsedPct >= atPct {
			return true
		}
	}
	return false
}
