package library

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// RoutesFile is the well-known filename inside a library root.
const RoutesFile = "routes.yaml"

// Route is one rule from routes.yaml. A route matches when ALL its specified
// match_* predicates are satisfied. `always: true` matches unconditionally.
type Route struct {
	MatchCwd    string   `yaml:"match_cwd,omitempty"`
	MatchEntity string   `yaml:"match_entity,omitempty"`
	Always      bool     `yaml:"always,omitempty"`
	Layers      []string `yaml:"layers,omitempty"`
	Skills      []string `yaml:"skills,omitempty"`
	Sources     []string `yaml:"sources,omitempty"`

	// compiledRegex is populated lazily when MatchEntity is set.
	compiledRegex *regexp.Regexp
}

// RoutesFile parsed shape.
type routesDoc struct {
	Routes []Route `yaml:"routes"`
}

// Resolution is the accumulated, deduplicated set of layers/skills/sources
// activated by all matching routes for a query.
type Resolution struct {
	Layers       []string
	Skills       []string
	Sources      []string
	MatchedRules int
}

// LoadRoutes reads <rootPath>/routes.yaml. Returns an empty list if the file
// does not exist (an empty routes file is a valid root state).
func LoadRoutes(rootPath string) ([]Route, error) {
	path := filepath.Join(rootPath, RoutesFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var doc routesDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return doc.Routes, nil
}

// Resolve walks every route in every present root and accumulates the
// active layers/skills/sources for the given cwd and parsed entity values.
//
// Multiple routes can match; their outputs accumulate (deduped, declared
// order preserved within each list). Roots are visited in registry order.
//
// match_entity is the only entity-routing signal (the partner registry that
// used to synthesize these routes automatically was removed 2026-09-12 with
// no replacement): each route in routes.yaml explicitly lists the entity
// tokens it matches and the layers/skills/sources it activates.
func (r *Registry) Resolve(cwd string, entities []string) (*Resolution, error) {
	res := &Resolution{}
	seenLayer := make(map[string]bool)
	seenSkill := make(map[string]bool)
	seenSource := make(map[string]bool)

	apply := func(route *Route, rootName string, routeIdx int) error {
		match, err := route.matches(cwd, entities)
		if err != nil {
			return fmt.Errorf("root %q route %d: %w", rootName, routeIdx, err)
		}
		if !match {
			return nil
		}
		res.MatchedRules++
		for _, l := range route.Layers {
			if !seenLayer[l] {
				seenLayer[l] = true
				res.Layers = append(res.Layers, l)
			}
		}
		for _, s := range route.Skills {
			if !seenSkill[s] {
				seenSkill[s] = true
				res.Skills = append(res.Skills, s)
			}
		}
		for _, s := range route.Sources {
			if !seenSource[s] {
				seenSource[s] = true
				res.Sources = append(res.Sources, s)
			}
		}
		return nil
	}

	for _, root := range r.Roots {
		routes, err := LoadRoutes(root.AbsPath)
		if err != nil {
			return nil, fmt.Errorf("root %q: %w", root.Ref.Name, err)
		}
		for i := range routes {
			if err := apply(&routes[i], root.Ref.Name, i); err != nil {
				return nil, err
			}
		}
	}

	// Auto-include the per-source layer (`sources/<name>`) for every
	// picked source whose layer is registered in the manifest. Without
	// this, a source like `nytimes` whose context lives in
	// `layers/sources/nytimes.md` is silently ignored unless every
	// route remembers to list `sources/nytimes` in its `layers:` block -
	// the historical NYT bug. The layer is only included when the
	// registry actually has it, so this is safe for sources that ship
	// without a context doc (lint warns separately).
	for _, name := range res.Sources {
		layerName := "sources/" + name
		if _, ok := r.Layers[layerName]; !ok {
			continue
		}
		if seenLayer[layerName] {
			continue
		}
		seenLayer[layerName] = true
		res.Layers = append(res.Layers, layerName)
	}

	return res, nil
}

// matches reports whether this route applies to the given cwd and entity
// list. Semantics: every specified predicate must be satisfied (AND). A
// route with `always: true` matches unconditionally regardless of other
// fields. A route with NO predicates and `always: false` never matches.
func (r *Route) matches(cwd string, entities []string) (bool, error) {
	if r.Always {
		return true, nil
	}
	matched := false
	if r.MatchCwd != "" {
		ok, err := matchGlob(r.MatchCwd, cwd)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		matched = true
	}
	if r.MatchEntity != "" {
		if r.compiledRegex == nil {
			re, err := regexp.Compile(r.MatchEntity)
			if err != nil {
				return false, fmt.Errorf("invalid match_entity regex %q: %w", r.MatchEntity, err)
			}
			r.compiledRegex = re
		}
		hit := false
		for _, e := range entities {
			if r.compiledRegex.MatchString(e) {
				hit = true
				break
			}
		}
		if !hit {
			return false, nil
		}
		matched = true
	}
	return matched, nil
}

// matchGlob expands ~ in the pattern, expands ~ in the path, then performs
// a simple glob match supporting `**` as "any depth". The implementation is
// intentionally minimal -- it covers the common case (path prefix with /**)
// without pulling in a third-party glob library.
func matchGlob(pattern, path string) (bool, error) {
	pattern = expandPath(pattern)
	path = expandPath(path)

	// Normalize trailing slash.
	pattern = strings.TrimRight(pattern, "/")
	path = strings.TrimRight(path, "/")

	// Fast path: literal prefix with /** suffix means "anywhere under this dir".
	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		return path == prefix || strings.HasPrefix(path, prefix+"/"), nil
	}
	// /** in the middle: split, both halves must match.
	if i := strings.Index(pattern, "/**/"); i >= 0 {
		head := pattern[:i]
		tail := pattern[i+len("/**/"):]
		if !strings.HasPrefix(path, head+"/") && path != head {
			return false, nil
		}
		// Find a tail match anywhere in the remaining path.
		rest := strings.TrimPrefix(path, head+"/")
		for rest != "" {
			if ok, _ := filepath.Match(tail, rest); ok {
				return true, nil
			}
			// Drop one path component and retry.
			idx := strings.Index(rest, "/")
			if idx < 0 {
				break
			}
			rest = rest[idx+1:]
		}
		return false, nil
	}
	// No **: regular filepath.Match.
	return filepath.Match(pattern, path)
}
