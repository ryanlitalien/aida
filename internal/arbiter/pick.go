package arbiter

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/burndown"
)

// ErrNoLane is returned by Pick when every lane was rejected. The plan is
// explicit that this is a legitimate outcome, not a bug: "never chooses
// new spend on its own" means Pick has no fallback beyond the lanes
// already configured and eligible -- a caller sees ErrNoLane, waits, and
// may suggest a lever (docs/arbiter-plan.md section 9's worked examples),
// but the arbiter itself never reaches past the allowlist and headroom
// rules to find capacity.
var ErrNoLane = errors.New("arbiter: no eligible lane")

// Request is one task's ask: what it is (tags, for ClassifyTags), what
// role it needs a model for, and when "now" is (so tests can pin it).
type Request struct {
	TaskSlug string
	TaskID   int
	Tags     []string
	// Role is the Pick role (RoleExecutor, RoleThinker, RoleTrivial).
	// Empty defaults to RoleExecutor.
	Role string
	// Now is the instant Pick evaluates signals and capacity against.
	// The zero value means time.Now().
	Now time.Time
}

// Rejection is one lane Pick skipped, with a precise, human-readable
// reason -- the build brief calls for these explicitly so a caller (or a
// test) never has to guess why a lane didn't win.
type Rejection struct {
	Lane   string
	Reason string
}

// Decision is Pick's result: the winning lane (nil when none was
// eligible -- see ErrNoLane), the model it should run the requested role
// with, the data class the request resolved to, and the full list of
// lanes rejected along the way (populated even on a winning Decision, so
// a caller can log why cheaper lanes were skipped).
type Decision struct {
	Lane           *Lane
	Model          string
	Class          DataClass
	Capacity       LaneCapacity
	NearExhaustion bool
	Rejected       []Rejection
	Reason         string
}

// SignalReader is the read side of a Store that Pick needs -- an
// interface so Pick can be tested without touching disk. A nil
// SignalReader means "no signal checking", not "everything is empty".
type SignalReader interface {
	EmptyUntil(lane string, now time.Time) (time.Time, bool)
}

// Pick walks cfg's lanes cheapest-first (Config.Sorted) and returns the
// first one eligible for req: allowed for req's resolved data class,
// serving req's role, not signalled empty, and carrying enough headroom
// on every window it tracks (or explicitly allowed to run unprobed).
//
// Every lane Pick skips is recorded in the returned Decision.Rejected
// with a specific reason, whether or not a later lane wins -- this is
// what lets a caller explain "why lane X and not the cheaper lane Y"
// without re-deriving the answer.
//
// Pick returns ErrNoLane, never a fallback lane, when nothing is
// eligible -- see ErrNoLane's doc comment for why that's by design, not
// a gap.
func Pick(cfg *Config, req Request, caps []burndown.Capacity, signals SignalReader) (Decision, error) {
	class, err := ClassifyTags(req.Tags)
	if err != nil {
		return Decision{}, err
	}

	role := req.Role
	if role == "" {
		role = RoleExecutor
	}
	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}

	var rejected []Rejection
	if cfg != nil {
		for _, lane := range cfg.Sorted() {
			if !lane.AllowsClass(class) {
				rejected = append(rejected, Rejection{
					Lane:   lane.ID,
					Reason: fmt.Sprintf("data class %q not allowed on this lane (allows %s)", class, formatClasses(lane.DataClasses)),
				})
				continue
			}
			if !lane.ServesRole(role) {
				rejected = append(rejected, Rejection{
					Lane:   lane.ID,
					Reason: fmt.Sprintf("lane does not serve role %q", role),
				})
				continue
			}
			if signals != nil {
				if until, ok := signals.EmptyUntil(lane.ID, now); ok {
					rejected = append(rejected, Rejection{
						Lane:   lane.ID,
						Reason: fmt.Sprintf("signalled empty until %s", until.Format(time.RFC3339)),
					})
					continue
				}
			}

			lc := LaneCapacityFor(lane, caps)
			switch {
			case !lc.Probed:
				if !lane.AllowUnprobed {
					rejected = append(rejected, Rejection{
						Lane:   lane.ID,
						Reason: "unprobed lane without allow_unprobed",
					})
					continue
				}
			case len(lc.Missing) > 0:
				rejected = append(rejected, Rejection{
					Lane:   lane.ID,
					Reason: fmt.Sprintf("missing window %s", strings.Join(lc.Missing, ", ")),
				})
				continue
			default:
				if reason, ok := headroomShortfall(lc); ok {
					rejected = append(rejected, Rejection{Lane: lane.ID, Reason: reason})
					continue
				}
			}

			return Decision{
				Lane:           &lane,
				Model:          lane.ModelFor(role),
				Class:          class,
				Capacity:       lc,
				NearExhaustion: NearExhaustion(lc, lane.EffectiveHandoffAtPct()),
				Rejected:       rejected,
				Reason:         "eligible",
			}, nil
		}
	}

	return Decision{Class: class, Rejected: rejected}, ErrNoLane
}

// headroomShortfall reports the first window (in lc.Windows order) whose
// Headroom falls below lc.MinHeadroom, formatted as a rejection reason.
// ok is false when every window clears the floor.
func headroomShortfall(lc LaneCapacity) (reason string, ok bool) {
	var shortfalls []string
	for _, c := range lc.Windows {
		if c.Headroom < lc.MinHeadroom {
			shortfalls = append(shortfalls, fmt.Sprintf("%s headroom %.1f%% < min %.1f%%", c.Label, c.Headroom, lc.MinHeadroom))
		}
	}
	if len(shortfalls) == 0 {
		return "", false
	}
	return strings.Join(shortfalls, "; "), true
}

// formatClasses renders a lane's DataClasses allowlist for a rejection
// message, e.g. "butterstack, public".
func formatClasses(classes []DataClass) string {
	strs := make([]string, len(classes))
	for i, c := range classes {
		strs[i] = string(c)
	}
	return strings.Join(strs, ", ")
}
