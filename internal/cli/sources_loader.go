package cli

import (
	"fmt"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/library"
)

// resolveSources loads source configs, preferring the library (multi-root,
// machine-filtered) and falling back to the legacy ~/.aida/sources.yaml when
// the library has none.
//
// The second return value is a short string ("library" or "legacy") suitable
// for verbose output so the user can see which path was taken.
func resolveSources() (config.Sources, string, error) {
	libRegistry, libErr := library.LoadRegistry(config.Dir())
	if libErr == nil && libRegistry != nil {
		libSrcs, err := libRegistry.LoadSources()
		if err != nil {
			return nil, "", fmt.Errorf("loading library sources: %w", err)
		}
		if len(libSrcs) > 0 {
			// Library is the source of truth. Merge in legacy ONLY for
			// names not present in the library, so half-migrated machines
			// keep working.
			legacySrcs, _ := config.LoadSources()
			for name, src := range legacySrcs {
				if _, ok := libSrcs[name]; !ok {
					libSrcs[name] = src
				}
			}
			injectAvailableTools(libSrcs)
			libSrcs = filterByActiveProfile(libSrcs)
			return libSrcs, "library", nil
		}
	}

	// Library has nothing -- legacy is authoritative.
	srcs, err := config.LoadSources()
	if err != nil {
		return nil, "", fmt.Errorf("loading sources: %w", err)
	}
	injectAvailableTools(srcs)
	srcs = filterByActiveProfile(srcs)
	return srcs, "legacy", nil
}

// filterByActiveProfile removes sources whose profiles list does not
// include the currently active profile. Sources with no profiles field
// are kept (available to all profiles).
func filterByActiveProfile(srcs config.Sources) config.Sources {
	cfg, err := config.LoadConfig()
	if err != nil {
		return srcs
	}
	_, profileName := cfg.ActiveProfileConfig()
	filtered := config.FilterByProfile(srcs, profileName)
	return config.FilterByReachablePath(filtered)
}

// injectAvailableTools auto-detects tools on PATH from the built-in
// registry and injects sources for those found. Also merges any
// explicit profile.Tools overrides.
func injectAvailableTools(srcs config.Sources) {
	// First: auto-detect from PATH (no config needed)
	config.InjectAvailableTools(srcs)

	// Second: merge explicit profile.Tools overrides (for tools
	// that need custom config or aren't on PATH detection list)
	cfg, err := config.LoadConfig()
	if err != nil {
		return
	}
	profile, _ := cfg.ActiveProfileConfig()
	config.InjectToolSources(srcs, profile)
}
