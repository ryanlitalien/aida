package engine

// CrossProjectSource describes a source registered on another machine
// or profile that was discovered via the shared brain. It is surfaced
// by DiscoverCrossProjectSources so callers can suggest tools the
// local library does not know about.
type CrossProjectSource struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description"`
	FromBrain   bool   `json:"from_brain"`
}

// DiscoverCrossProjectSources searches the supplied brain entity
// pages (typically of type "tool") for sources absent from the local
// library. Entities whose name is already present in localSources are
// skipped; entities without a summary are ignored.
func DiscoverCrossProjectSources(brainEntities []struct {
	Name    string
	Summary string
}, localSources map[string]bool) []CrossProjectSource {
	var discovered []CrossProjectSource
	for _, e := range brainEntities {
		if localSources[e.Name] {
			continue
		}
		if e.Summary != "" {
			discovered = append(discovered, CrossProjectSource{
				Name:        e.Name,
				Type:        "brain-discovered",
				Description: e.Summary,
				FromBrain:   true,
			})
		}
	}
	return discovered
}
