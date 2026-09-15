package bifrost

import "encoding/json"

// This file mirrors herdr's JSON shapes verbatim - field names match
// herdr's wire format exactly, not Go convention where they'd differ
// (e.g. AgentStatus not Status). herdr adds fields over time; keeping
// our struct field names identical to herdr's JSON keys means a
// passthrough (e.g. re-marshaling an Agent for the /bifrost page's own
// API) costs nothing to keep in sync, and the page's JS can key off the
// same names this package does.

// envelope is herdr's top-level response shape for every JSON-emitting
// subcommand (everything except `herdr status`, which is plaintext).
type envelope struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *herdrError     `json:"error,omitempty"`
}

// herdrError is the shape of envelope.Error when herdr itself rejects
// a request (unknown target, bad args, etc) rather than the ssh/sudo
// hop failing.
type herdrError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Agent is one entry from `herdr agent list`. Field names match
// herdr's JSON keys verbatim (see file doc).
type Agent struct {
	Agent       string `json:"agent"`
	AgentStatus string `json:"agent_status"`
	Cwd         string `json:"cwd"`
	Focused     bool   `json:"focused"`

	Name                  string `json:"name"`
	PaneID                string `json:"pane_id"`
	TerminalID            string `json:"terminal_id"`
	TerminalTitle         string `json:"terminal_title"`
	TerminalTitleStripped string `json:"terminal_title_stripped"`
	WorkspaceID           string `json:"workspace_id"`

	// Lane is the /bifrost lane (configured herdr user) this agent was
	// listed under. It is NOT part of herdr's wire format -- herdr has no
	// concept of "lane" -- so Client.Agents never sets it; only the
	// multi-lane fan-out in internal/cli/dashboard_web.go's
	// GET /api/bifrost/agents handler sets it, once per lane, before
	// merging results across lanes.
	Lane string `json:"lane"`
}

// AgentList is the `result` payload of `herdr agent list`.
type AgentList struct {
	Agents []Agent `json:"agents"`
	Type   string  `json:"type"`
}

// ReadResult is the `result` payload of `herdr agent read`.
type ReadResult struct {
	Read struct {
		Text string `json:"text"`
	} `json:"read"`
}

// ServerStatus is the parsed form of `herdr status`, which is
// plaintext, not JSON (see herdr surface notes in the package doc).
// Raw always holds the full unparsed report so the /bifrost page can
// fall back to showing it verbatim if a future herdr version drifts
// the format enough that the field scan below stops matching.
type ServerStatus struct {
	Running bool
	Version string
	Socket  string
	Raw     string
}

// Preset is one operator-configured "start an agent like this" recipe.
// StartPreset only ever launches a preset selected by name - see the
// doc comment on Client.StartPreset for why there is deliberately no
// method that accepts a caller-supplied argv.
type Preset struct {
	Name string `yaml:"name" json:"name"`
	// User is the /bifrost lane this preset belongs to. New rejects a
	// preset whose User doesn't match the Client's own user -- see New's
	// doc comment -- so on a *Client that has already validated, User is
	// always either empty or exactly that Client's user.
	User string   `yaml:"user,omitempty" json:"user,omitempty"`
	Cwd  string   `yaml:"cwd,omitempty" json:"cwd,omitempty"`
	Argv []string `yaml:"argv" json:"argv"`
}
