package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/dispatch"
	"github.com/ryanlitalien/aida/internal/jarvis"
	"github.com/ryanlitalien/aida/internal/jobs"
	"github.com/ryanlitalien/aida/internal/llm"
	"github.com/ryanlitalien/aida/internal/mcp"
	"github.com/ryanlitalien/aida/internal/roster"
	"github.com/spf13/cobra"
)

// mcpConnectTimeout bounds how long `aida ask` waits to connect to every
// configured MCP server before giving up. Mirrors the timeout the agent
// loop's own discovery step uses (internal/cli/agent.go).
const mcpConnectTimeout = 15 * time.Second

// dispatchDepthEnvVar is the one-hop depth guard set by
// roster.BumpDispatchDepthEnv on every subagent/charter process it spawns
// (internal/roster/subagent.go). An agent calling `aida ask` sees this at
// 1 (allowed: one hop between agents); a second hop would land at 2 or
// more, which dispatchDepthExceeded refuses so agents can't chain `aida
// ask` calls into each other indefinitely.
const dispatchDepthEnvVar = "AIDA_DISPATCH_DEPTH"

// dispatchDepthExceeded reports whether the raw AIDA_DISPATCH_DEPTH
// environment value (as read via os.Getenv) is at or past the one-hop
// limit. Factored out of newAskCmd's RunE so it's testable without cobra;
// an unset or unparseable value is treated as depth 0 (not exceeded).
func dispatchDepthExceeded(raw string) bool {
	depth, err := strconv.Atoi(raw)
	if err != nil {
		return false
	}
	return depth >= 2
}

// buildMCPDiscovery connects to every configured MCP server -- the same
// precedence and entrypoints internal/cli/agent.go's agent-loop discovery
// uses (~/.claude.json, ~/.claude/.mcp.json, ./.mcp.json) -- and returns a
// ready *mcp.MCPDiscovery for the roster mcp backend to call tools
// against. The caller owns Close()ing it.
func buildMCPDiscovery(ctx context.Context) (*mcp.MCPDiscovery, error) {
	configs, err := mcp.LoadMCPConfigs()
	if err != nil {
		return nil, fmt.Errorf("loading MCP configs: %w", err)
	}
	if len(configs) == 0 {
		return nil, fmt.Errorf("no MCP servers configured (checked ~/.claude.json, ~/.claude/.mcp.json, ./.mcp.json)")
	}

	discovery := mcp.NewMCPDiscovery(verbose)
	connectCtx, cancel := context.WithTimeout(ctx, mcpConnectTimeout)
	defer cancel()
	if err := discovery.ConnectAll(connectCtx, configs); err != nil {
		discovery.Close()
		return nil, fmt.Errorf("connecting to MCP servers: %w", err)
	}
	if _, err := discovery.DiscoverTools(ctx); err != nil {
		discovery.Close()
		return nil, fmt.Errorf("discovering MCP tools: %w", err)
	}
	return discovery, nil
}

// newAskCmd builds `aida ask <who> <task>`: a named-entry-only dispatch
// against the roster (see internal/roster). Full Aida orchestration
// (fan-out, background jobs, synthesis) is Phase 4; for now the target
// must be a specific roster entry, not "aida" herself.
func newAskCmd() *cobra.Command {
	var raw bool

	cmd := &cobra.Command{
		Use:   "ask <who> <task>",
		Short: "Ask a roster agent (or Aida) to do something",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			if dispatchDepthExceeded(os.Getenv(dispatchDepthEnvVar)) {
				return fmt.Errorf("aida ask: dispatch depth 2 reached (one hop between agents is the limit); answer from what you already have")
			}

			who := args[0]
			task := strings.Join(args[1:], " ")

			cfg, err := config.LoadConfig()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			_, profileName := cfg.ActiveProfileConfig()

			r, err := roster.Load(config.Dir(), profileName)
			if err != nil {
				return fmt.Errorf("loading roster: %w", err)
			}

			entry, resolveErr := r.Resolve(who)
			if resolveErr != nil {
				var otherProfile *roster.ErrOtherProfile
				switch {
				case errors.As(resolveErr, &otherProfile):
					fmt.Fprintf(os.Stderr, "note: %s is on your %s roster; asking anyway\n",
						otherProfile.Entry.Display(), otherProfile.Profile)
					entry = otherProfile.Entry
				case errors.Is(resolveErr, roster.ErrNotFound):
					return fmt.Errorf("no roster entry matches %q (see `aida roster list`)", who)
				case errors.Is(resolveErr, roster.ErrAmbiguous):
					return fmt.Errorf("%q matches more than one roster entry; be more specific", who)
				default:
					return fmt.Errorf("resolving %q: %w", who, resolveErr)
				}
			}

			if entry.Kind == roster.KindAida {
				return runAidaDispatch(cfg, r, profileName, task, raw)
			}

			ctx := context.Background()
			deps := roster.Deps{Profile: profileName}

			switch entry.Kind {
			case roster.KindJob:
				js, err := jobs.Open(profileName)
				if err != nil {
					return fmt.Errorf("opening jobs store: %w", err)
				}
				defer js.Close()
				deps.Jobs = js
			case roster.KindMCP:
				discovery, err := buildMCPDiscovery(ctx)
				if err != nil {
					return fmt.Errorf("connecting MCP discovery: %w", err)
				}
				defer discovery.Close()
				deps.Discovery = discovery
			}

			backend, err := r.BackendFor(entry, deps)
			if err != nil {
				return err
			}

			result, err := backend.Ask(ctx, roster.Request{
				Task:    task,
				Profile: profileName,
				Origin:  "cli",
			})
			if err != nil {
				return err
			}

			if result.Status == roster.StatusDelegated {
				fmt.Printf("Delegated to a background job: %s\n", jobs.DeriveHandle(result.JobID))
				return nil
			}

			if result.Status != roster.StatusSuccess {
				fmt.Fprintf(os.Stderr, "%s: %s\n", result.Status, result.Text)
				return fmt.Errorf("%s did not answer (%s)", entry.Display(), result.Status)
			}

			if raw {
				fmt.Println(result.Text)
			} else {
				fmt.Printf("%s:\n\n%s\n", entry.Display(), result.Text)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&raw, "raw", false, "print the answer with no attribution prefix")
	return cmd
}

// runAidaDispatch handles `aida ask aida <task>`: full orchestration. Aida
// resolves or selects the right roster entries, runs them (fanning out in
// parallel), and aggregates one answer. The LLM client is built the same way
// the query path builds it; MCP discovery is only connected when the active
// profile actually has an mcp-backed entry, so a subagent/source dispatch pays
// no MCP connect cost. Jobs and discovery are best-effort: if either is
// unavailable, routes that need it degrade to a status note rather than failing
// the whole dispatch.
func runAidaDispatch(cfg *config.Config, r *roster.Roster, profileName, task string, raw bool) error {
	ctx := context.Background()

	apiKey := cfg.GetAPIKey()
	model := cfg.Model.Primary
	isOffline := offline || cfg.Model.OfflineMode
	var client *llm.Client
	if isOffline || apiKey != "" {
		client = llm.NewClient(apiKey, model, isOffline)
		if cfg.Model.Stages != nil {
			client.SetStageModels(cfg.Model.Stages)
		}
	}
	// A nil client is fine: Aida still routes named call-signs deterministically
	// and falls back to a general query; only the LLM select/aggregate edges go dark.

	deps := roster.Deps{Profile: profileName}
	if js, err := jobs.Open(profileName); err == nil {
		defer js.Close()
		deps.Jobs = js
	}
	if rosterHasMCP(r) {
		if discovery, err := buildMCPDiscovery(ctx); err == nil {
			defer discovery.Close()
			deps.Discovery = discovery
		}
	}

	d := &dispatch.Dispatcher{Roster: r, Deps: deps}
	if client != nil {
		d.LLM = client // guard the typed-nil interface trap: a nil *llm.Client must stay a nil Completer
	}
	report, err := d.Dispatch(ctx, task, "cli")
	if err != nil {
		return err
	}
	fmt.Println(strings.TrimSpace(report.Answer))
	return nil
}

// rosterHasMCP reports whether the active profile has any mcp-backed entry, so
// the dispatcher only pays the MCP connect cost when a route could need it.
func rosterHasMCP(r *roster.Roster) bool {
	for _, e := range r.Entries() {
		if e.Kind == roster.KindMCP {
			return true
		}
	}
	return false
}

// buildAidaDispatcher constructs the dispatcher `aida serve` hands to the Aida
// voice persona. It loads the active profile's roster, reuses the base
// assistant's shared jobs store + MCP discovery for Deps (no double-open), and
// builds an engine LLM client for the select/aggregate edges. A nil client
// (no API key, not offline) is left unset so Aida degrades to named-only
// routing plus fallback rather than tripping the typed-nil interface trap.
func buildAidaDispatcher(cfg *config.Config, profileName string, a *jarvis.Assistant) (*dispatch.Dispatcher, error) {
	r, err := roster.Load(config.Dir(), profileName)
	if err != nil {
		return nil, fmt.Errorf("loading roster: %w", err)
	}
	d := &dispatch.Dispatcher{
		Roster: r,
		Deps: roster.Deps{
			Profile:   profileName,
			Jobs:      a.JobsStore(),
			Discovery: a.Discovery(),
		},
	}
	apiKey := cfg.GetAPIKey()
	if apiKey != "" || cfg.Model.OfflineMode {
		client := llm.NewClient(apiKey, cfg.Model.Primary, cfg.Model.OfflineMode)
		if cfg.Model.Stages != nil {
			client.SetStageModels(cfg.Model.Stages)
		}
		d.LLM = client
	}
	return d, nil
}
