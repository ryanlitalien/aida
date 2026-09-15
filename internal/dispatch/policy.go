package dispatch

import "github.com/ryanlitalien/aida/internal/roster"

// Decision is the sync-vs-background verdict for a set of routes from a given
// origin. It is pure and deterministic (never an LLM call), and is consumed by
// the voice layer to decide whether to answer inline or enqueue a job and
// speak a handle. The CLI ignores it and always runs Dispatch synchronously.
type Decision struct {
	Background bool
	Reason     string
}

// Decide returns whether a dispatch over these entries, from this origin,
// should run in the background. The rule set mirrors the latency budget the
// voice layer documents (fast tools 2-4s, aida_query 8-15s, subagents
// 30-120s): anything that cannot land inside the aida_query class from voice
// goes async.
func Decide(entries []*roster.Entry, origin string) Decision {
	for _, e := range entries {
		if e.Kind == roster.KindJob || e.Mode == "async" {
			return Decision{Background: true, Reason: "a routed entry is background by nature"}
		}
	}

	if origin == "voice" {
		if len(entries) > 1 {
			return Decision{Background: true, Reason: "fan-out from voice"}
		}
		for _, e := range entries {
			if e.Kind == roster.KindSubagent {
				return Decision{Background: true, Reason: "subagent call from voice exceeds the voice latency budget"}
			}
		}
	}

	return Decision{Background: false, Reason: "fits a synchronous reply"}
}
