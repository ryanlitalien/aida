package library

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/ui"
)

// LoadSources walks every present root in the registry and loads each
// source-config YAML file referenced by the manifest's `sources:` map.
// Sources are merged in registry order; later roots override earlier ones
// on name collision.
//
// Each source file is expected to contain ONE source's config in the same
// shape as one entry of the legacy ~/.aida/sources.yaml -- so migration
// is mechanical and reversible.
//
// Returns an empty Sources map if no roots have any source entries.
//
// Sources rejected by validateSource are dropped from the result and
// recorded on the Registry for later retrieval via Issues(). The
// rejection messages are remediation-shaped so callers can surface
// them as agent context (cli/agent.go) or lint findings (cli/lint.go).
func (r *Registry) LoadSources() (config.Sources, error) {
	out := make(config.Sources)
	r.loadIssues = nil // reset on each load so repeat calls don't accumulate
	// Iterate the resolved Sources map (populated by mergeRoot from
	// manifests + mergeAutoDiscovered for self-owned roots). Using the
	// resolved map instead of walking root manifests directly is what
	// lets a YAML dropped into `sources/` get picked up without a
	// library.yaml edit on single-root setups.
	for name, resolved := range r.Sources {
		abs := resolved.AbsFile
		data, err := os.ReadFile(abs)
		if err != nil {
			return nil, fmt.Errorf("source %q: reading %s: %w", name, abs, err)
		}
		var src config.Source
		if err := yaml.Unmarshal(data, &src); err != nil {
			return nil, fmt.Errorf("source %q: parsing %s: %w", name, abs, err)
		}
		if issue := validateSource(name, &src, abs); issue != nil {
			r.loadIssues = append(r.loadIssues, *issue)
			ui.PrintVerbose("Source disabled", issue.Format())
			continue
		}
		out[name] = &src
	}
	return out, nil
}

// Issues returns library-load issues recorded by the most recent
// LoadSources call. Read-only - the returned slice is a copy so
// callers can't mutate the registry's internal state.
//
// Each issue's Reason describes what's wrong; Fix is the
// remediation guidance suitable for surfacing as agent context
// (see LibraryIssue.AgentContext) or lint output.
func (r *Registry) Issues() []LibraryIssue {
	if len(r.loadIssues) == 0 {
		return nil
	}
	out := make([]LibraryIssue, len(r.loadIssues))
	copy(out, r.loadIssues)
	return out
}

// validateSource returns a populated *LibraryIssue when the source
// is unsafe to execute, nil otherwise. A non-nil return drops the
// source from the loaded registry and is recorded on the registry
// for later retrieval.
//
// Phase 1.4: reject sources whose exec template is a raw `{query}`
// passthrough with no command prefix - those feed the LLM's text
// output directly into `sh -c`, producing shell syntax errors when
// the LLM emits prose. Rejection message is remediation-shaped so
// it doubles as the agent's context for "this source could exist
// if you fix it like so".
func validateSource(name string, src *config.Source, path string) *LibraryIssue {
	if src == nil {
		return &LibraryIssue{
			SourceName: name,
			Severity:   IssueSeverityError,
			Type:       "nil-config",
			Reason:     "source config parsed to nil - manifest entry references an empty or malformed file",
			Fix: "Open the source's YAML file and ensure it has at least the required keys (`type:` and either `path:` or `exec:`).\n" +
				"Then re-run `aida index --generate` to refresh the manifest.",
			Path: path,
		}
	}
	if len(src.Exec) == 0 {
		return nil
	}
	for key, tmpl := range src.Exec {
		trimmed := strings.TrimSpace(tmpl)
		// Strip surrounding quotes that YAML sometimes preserves.
		trimmed = strings.Trim(trimmed, "'\"")
		if trimmed == "{query}" || trimmed == "{search}" || trimmed == "{command}" {
			return &LibraryIssue{
				SourceName: name,
				Severity:   IssueSeverityError,
				Type:       IssueTypeRawPassthrough,
				Reason: fmt.Sprintf(
					"exec.%s is a bare {%s} placeholder with no command prefix - sh -c would execute the LLM's raw text and shell-inject when the LLM emits prose",
					key, strings.Trim(trimmed, "{}")),
				Fix: "Prepend a real command to the template, for example:\n" +
					"  exec:\n" +
					fmt.Sprintf("    %s: 'gh pr list --repo owner/repo {%s}'   # for GitHub\n", key, strings.Trim(trimmed, "{}")) +
					fmt.Sprintf("    %s: 'sqlite3 mydb.db \"{%s}\"'              # for a SQL data source\n", key, strings.Trim(trimmed, "{}")) +
					"Then re-run `aida index --generate` to refresh the manifest. The validator only rejects bare placeholders; any prefix is accepted.",
				Path: path,
			}
		}
	}
	return nil
}
