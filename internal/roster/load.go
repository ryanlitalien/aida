package roster

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// File is the on-disk shape of ~/.aida/roster.yaml: a versioned, top-level
// map of entry name to Entry.
type File struct {
	Version int               `yaml:"version"`
	Entries map[string]*Entry `yaml:"entries"`
}

// Roster is the loaded, profile-scoped view of the roster. `entries` is the
// active profile's subset (sorted by Name), used for listing and the first
// pass of Resolve. `all` retains every entry across every profile, post
// discovery expansion, for lint and `aida roster list --all`.
type Roster struct {
	profile string
	entries []*Entry
	all     []*Entry
	issues  []string
}

// Load reads aidaDir/roster.yaml, expands any discovery entries (see
// discover.go), dedupes hand-authored overrides against their discovered
// twins, and filters to profile. A missing roster.yaml is not an error --
// it returns an empty, profile-scoped *Roster so callers can resolve
// nothing gracefully rather than fail outright.
func Load(aidaDir, profile string) (*Roster, error) {
	path := filepath.Join(aidaDir, "roster.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Roster{profile: profile}, nil
		}
		return nil, fmt.Errorf("reading roster %q: %w", path, err)
	}

	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parsing roster %q: %w", path, err)
	}

	r := &Roster{profile: profile}

	// Lowercase every hand-authored entry's Name and expand discovery
	// parents into their virtual entries. A discovery parent is never
	// itself added to the working set.
	var working []*Entry
	for name, e := range f.Entries {
		e.Name = strings.ToLower(name)
		if e.Discover == "" {
			working = append(working, e)
			continue
		}
		expanded, err := expandDiscovery(e)
		if err != nil {
			r.issues = append(r.issues, fmt.Sprintf("%s: %s", e.Name, err))
			continue
		}
		working = append(working, expanded...)
	}

	// Dedupe by Name: a hand-authored entry wins over a discovered twin
	// with the same Name, regardless of which one was appended first (map
	// iteration order is randomized).
	byName := map[string]*Entry{}
	var order []string
	for _, e := range working {
		existing, ok := byName[e.Name]
		if !ok {
			byName[e.Name] = e
			order = append(order, e.Name)
			continue
		}
		if existing.DiscoveredFrom != "" && e.DiscoveredFrom == "" {
			byName[e.Name] = e // hand-authored override replaces the discovered twin
		}
	}

	all := make([]*Entry, 0, len(order))
	for _, name := range order {
		all = append(all, byName[name])
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	r.all = all

	var entries []*Entry
	for _, e := range all {
		if e.Kind == KindAida || len(e.Profiles) == 0 || containsProfile(e.Profiles, profile) {
			entries = append(entries, e)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	r.entries = entries

	return r, nil
}

func containsProfile(profiles []string, profile string) bool {
	for _, p := range profiles {
		if p == profile {
			return true
		}
	}
	return false
}

// Entries returns the active profile's roster entries, sorted by Name.
func (r *Roster) Entries() []*Entry { return r.entries }

// Profile returns the profile name this roster was loaded for.
func (r *Roster) Profile() string { return r.profile }

// Get looks up an entry by its exact, already-lowercased Name within the
// active profile's entries. It does not consult call-signs or aliases --
// use Resolve for fuzzy/spoken lookups.
func (r *Roster) Get(name string) (*Entry, bool) {
	name = strings.ToLower(name)
	for _, e := range r.entries {
		if e.Name == name {
			return e, true
		}
	}
	return nil, false
}

// Issues returns non-fatal problems encountered while loading, e.g. a
// discovery parent whose directory could not be scanned. The roster still
// loads successfully around them.
func (r *Roster) Issues() []string { return r.issues }

// AllEntries returns every entry across every profile, post discovery
// expansion -- for `aida roster list --all` and future lint use.
func (r *Roster) AllEntries() []*Entry { return r.all }
