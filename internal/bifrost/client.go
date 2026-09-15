// Package bifrost is a control client for herdr, a terminal
// agent-multiplexer running as the isolated `heimdall` user on the
// `minty` box. It backs the /bifrost dashboard page: a board of herdr
// agents (idle/working/blocked), a pane reader, and a way to send text
// into one.
//
// Every remote call is routed through internal/remotex.RunRemote,
// which carries the herdr argv as a base64 script on ssh's stdin so no
// browser-controlled byte ever crosses sshd's login shell, sudo -i's
// login shell, or any other argv-joining layer. This package never
// builds ssh argv or shell strings itself - see internal/remotex/script.go
// for why that matters.
package bifrost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/remotex"
)

const (
	defaultTimeout = 20 * time.Second

	defaultLines = 200
	maxLines     = 2000

	// maxSendBytes bounds the text a browser can push into a pane.
	// herdr agent send types the text literally (no shell involved on
	// the herdr side), so this isn't a shell-safety limit - it's a
	// sanity cap against an accidental (or malicious) multi-megabyte
	// paste wedging a pane or the ssh round trip.
	maxSendBytes = 8 * 1024
)

// validReadSources allowlists the `--source` values herdr's `agent
// read` accepts. Same rationale as remotex's target regex: an
// allowlist rejects unknown shapes outright instead of trying to
// escape them.
var validReadSources = map[string]bool{
	"visible":          true,
	"recent":           true,
	"recent-unwrapped": true,
}

// RemoteError means the ssh/sudo hop and herdr itself both ran, but
// herdr (or the transport) reported failure: nonzero exit, a timeout,
// or an explicit {"error": ...} envelope. This is "herdr/minty said
// no," not "our code is confused" - HTTP handlers should map this to
// a normal 200-with-{"error":...} response, not a 5xx.
type RemoteError struct {
	Op      string
	Message string
	Err     error
}

func (e *RemoteError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("bifrost: %s: %s: %v", e.Op, e.Message, e.Err)
	}
	return fmt.Sprintf("bifrost: %s: %s", e.Op, e.Message)
}

func (e *RemoteError) Unwrap() error { return e.Err }

// ParseError means the remote call itself succeeded but its output
// didn't look like anything this client understands - a real herdr
// version drift or wire-format change, i.e. our bug to fix, not a
// transient failure. HTTP handlers should map this to a 5xx.
//
// Received is a bounded prefix of what herdr actually sent, never the
// full unbounded output - this error is likely to end up in a log or
// an HTTP body, and herdr output is not something we want to echo back
// without a size limit.
type ParseError struct {
	Op       string
	Received string
	Err      error
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("bifrost: %s: parse: %v (received: %q)", e.Op, e.Err, e.Received)
}

func (e *ParseError) Unwrap() error { return e.Err }

// ValidationError is a caller mistake caught before any remote call
// was attempted: a bad target, an unknown `--source`, oversized send
// text, an unknown preset name. HTTP handlers should map this to a 400.
type ValidationError struct {
	Op      string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("bifrost: %s: %s", e.Op, e.Message)
}

// Client talks to herdr on a single remote host, as a single sudo
// user, over SSH.
type Client struct {
	Runner  remotex.Runner
	Host    string
	User    string
	Timeout time.Duration
	Presets []Preset
}

// New validates the host/user and the preset list up front, so a
// misconfigured preset (empty name, empty argv, a duplicate name, or a
// preset assigned to a different /bifrost lane) fails at startup
// instead of surfacing later as a confusing failure the first time
// someone tries to launch it.
//
// A preset with a non-empty User that doesn't match user is rejected:
// each lane's Client must only ever hold its own presets (see
// internal/cli/dashboard_web.go's per-lane construction), so a mismatch
// here means the caller wired a lane's presets wrong, not that herdr or
// the remote host did anything -- catching it at startup keeps that bug
// local to the Client that was misconfigured.
func New(r remotex.Runner, host, user string, presets []Preset) (*Client, error) {
	if err := remotex.ValidateHost(host); err != nil {
		return nil, fmt.Errorf("bifrost: %w", err)
	}
	if user != "" {
		// A POSIX username is a strict subset of what ValidateHost
		// already accepts (same rationale RunRemote itself uses for
		// sudoUser) - one validator to audit instead of two
		// near-duplicate regexes.
		if err := remotex.ValidateHost(user); err != nil {
			return nil, fmt.Errorf("bifrost: invalid user %q: %w", user, err)
		}
	}

	seen := make(map[string]bool, len(presets))
	for i, p := range presets {
		if p.Name == "" {
			return nil, fmt.Errorf("bifrost: preset[%d]: empty name", i)
		}
		if len(p.Argv) == 0 {
			return nil, fmt.Errorf("bifrost: preset %q: empty argv", p.Name)
		}
		if p.User != "" && p.User != user {
			return nil, fmt.Errorf("bifrost: preset %q: user %q does not match client user %q", p.Name, p.User, user)
		}
		if seen[p.Name] {
			return nil, fmt.Errorf("bifrost: duplicate preset name %q", p.Name)
		}
		seen[p.Name] = true
	}

	return &Client{Runner: r, Host: host, User: user, Presets: presets}, nil
}

func (c *Client) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return defaultTimeout
}

// run is the single choke point every herdr invocation passes through:
// it builds the RunRemote call and turns transport-level failure
// (nonzero exit, timeout, ssh error) into a typed *RemoteError so
// callers never have to look at remotex.Result directly.
func (c *Client) run(ctx context.Context, op string, argv []string) (remotex.Result, error) {
	res, err := remotex.RunRemote(ctx, c.Runner, c.Host, c.User, argv, c.timeout())
	if err != nil {
		return res, &RemoteError{Op: op, Message: "ssh invocation failed", Err: err}
	}
	if res.TimedOut {
		return res, &RemoteError{Op: op, Message: "timed out"}
	}
	if res.ExitCode != 0 {
		return res, &RemoteError{Op: op, Message: fmt.Sprintf("exit %d: %s", res.ExitCode, boundedPrefix(res.Stderr, 500))}
	}
	return res, nil
}

// Status runs `herdr status` and parses its plaintext report.
func (c *Client) Status(ctx context.Context) (ServerStatus, error) {
	res, err := c.run(ctx, "status", []string{"herdr", "status"})
	if err != nil {
		return ServerStatus{}, err
	}
	return parseStatus(string(res.Stdout)), nil
}

// Agents runs `herdr agent list` and returns the board of agents.
func (c *Client) Agents(ctx context.Context) ([]Agent, error) {
	const op = "agent list"

	res, err := c.run(ctx, op, []string{"herdr", "agent", "list"})
	if err != nil {
		return nil, err
	}
	env, err := parseEnvelope(op, res.Stdout)
	if err != nil {
		return nil, err
	}

	var list AgentList
	if err := json.Unmarshal(env.Result, &list); err != nil {
		return nil, &ParseError{Op: op, Received: boundedPrefix(res.Stdout, 500), Err: err}
	}
	return list.Agents, nil
}

// Read runs `herdr agent read` against a single agent's pane.
//
// source must be "visible", "recent", "recent-unwrapped", or empty
// (which defaults to "recent"). lines is clamped to 1..2000 and
// defaults to 200 when <= 0.
func (c *Client) Read(ctx context.Context, target, source string, lines int) (string, error) {
	const op = "agent read"

	if err := remotex.ValidateTarget(target); err != nil {
		return "", &ValidationError{Op: op, Message: err.Error()}
	}
	if source == "" {
		source = "recent"
	}
	if !validReadSources[source] {
		return "", &ValidationError{Op: op, Message: fmt.Sprintf("invalid source %q", source)}
	}
	lines = clampLines(lines)

	argv := []string{"herdr", "agent", "read", target, "--source", source, "--lines", strconv.Itoa(lines)}
	res, err := c.run(ctx, op, argv)
	if err != nil {
		return "", err
	}
	env, err := parseEnvelope(op, res.Stdout)
	if err != nil {
		return "", err
	}

	var rr ReadResult
	if err := json.Unmarshal(env.Result, &rr); err != nil {
		return "", &ParseError{Op: op, Received: boundedPrefix(res.Stdout, 500), Err: err}
	}
	return rr.Read.Text, nil
}

func clampLines(n int) int {
	if n <= 0 {
		return defaultLines
	}
	if n > maxLines {
		return maxLines
	}
	return n
}

// Send types literal text into a herdr agent pane via `herdr agent
// send`. It never appends Enter on its own - herdr's own semantics for
// this subcommand are "type this text," not "run this command line" -
// except when enter is true, in which case a trailing "\r" is
// appended so the target's shell/REPL actually executes what was
// typed.
//
// Do NOT change this to shell out via `pane run` or similar: text
// here is always delivered as one argv element through
// remotex.RunRemote's base64 stdin script, so it is never re-parsed by
// any shell, on this host or the remote one, regardless of content.
func (c *Client) Send(ctx context.Context, target, text string, enter bool) error {
	const op = "agent send"

	if err := remotex.ValidateTarget(target); err != nil {
		return &ValidationError{Op: op, Message: err.Error()}
	}
	if len(text) > maxSendBytes {
		return &ValidationError{Op: op, Message: fmt.Sprintf("text exceeds %d bytes", maxSendBytes)}
	}
	if strings.IndexByte(text, 0) >= 0 {
		// NUL can't survive process argv on either end of the hop
		// (see remotex's package doc) - reject it here rather than
		// let it silently truncate somewhere downstream.
		return &ValidationError{Op: op, Message: "text contains a NUL byte"}
	}
	if enter {
		text += "\r"
	}

	// herdr agent send's own stdout isn't a documented JSON result
	// shape worth depending on (unlike list/read) - a nonzero exit or
	// timeout via c.run is already a typed RemoteError, which is all
	// the signal this method promises.
	_, err := c.run(ctx, op, []string{"herdr", "agent", "send", target, text})
	return err
}

// StartPreset launches a herdr agent from one of the presets
// configured on this Client (c.Presets), selected by name.
//
// There is deliberately no method that accepts a caller-supplied argv.
// A browser POSTing an argv that this package would execute as another
// user on another machine is a remote shell with a JSON wrapper: any
// CSRF weakness on the /bifrost page would become RCE on minty. If a
// future feature genuinely needs to launch an arbitrary command, that
// needs its own explicit, audited, allowlisted design - not a
// parameter bolted onto this method.
func (c *Client) StartPreset(ctx context.Context, presetName string) error {
	const op = "agent start"

	var preset *Preset
	for i := range c.Presets {
		if c.Presets[i].Name == presetName {
			preset = &c.Presets[i]
			break
		}
	}
	if preset == nil {
		return &ValidationError{Op: op, Message: fmt.Sprintf("unknown preset %q", presetName)}
	}

	argv := []string{"herdr", "agent", "start", preset.Name}
	if preset.Cwd != "" {
		argv = append(argv, "--cwd", preset.Cwd)
	}
	argv = append(argv, "--")
	argv = append(argv, preset.Argv...)

	_, err := c.run(ctx, op, argv)
	return err
}

// parseEnvelope tolerates leading noise on stdout - ssh MOTDs are
// real, and -o LogLevel=ERROR (see remotex.SSHArgs) only silences
// ssh's OWN diagnostics, not whatever the remote login/sudo shell
// prints on its way to running herdr - by scanning for the first '{'
// and decoding from there. json.Decoder (not json.Unmarshal) is used
// so trailing bytes after the JSON value don't cause a spurious
// failure either.
func parseEnvelope(op string, stdout []byte) (envelope, error) {
	idx := bytes.IndexByte(stdout, '{')
	if idx < 0 {
		return envelope{}, &ParseError{Op: op, Received: boundedPrefix(stdout, 500), Err: errors.New("no JSON object found in output")}
	}

	var env envelope
	dec := json.NewDecoder(bytes.NewReader(stdout[idx:]))
	if err := dec.Decode(&env); err != nil {
		return envelope{}, &ParseError{Op: op, Received: boundedPrefix(stdout, 500), Err: err}
	}
	if env.Error != nil {
		return envelope{}, &RemoteError{Op: op, Message: fmt.Sprintf("%s: %s", env.Error.Code, env.Error.Message)}
	}
	return env, nil
}

// parseStatus extracts the server: section's status/version/socket
// keys from herdr's plaintext status report (herdr status is the one
// subcommand that does NOT emit JSON). This is manual line scanning
// rather than a YAML library on purpose: the output isn't guaranteed
// to actually be valid YAML across herdr versions, and Raw always
// carries the full text regardless, so a parse miss degrades to "show
// the raw report" instead of a hard failure.
func parseStatus(raw string) ServerStatus {
	st := ServerStatus{Raw: raw}

	inServer := false
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue // blank lines don't end a section
		}
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			key := strings.TrimSuffix(strings.TrimSpace(line), ":")
			inServer = key == "server"
			continue
		}
		if !inServer {
			continue
		}

		key, val, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		switch key {
		case "status":
			st.Running = val == "running"
		case "version":
			st.Version = val
		case "socket":
			st.Socket = val
		}
	}
	return st
}

// boundedPrefix trims and truncates b for safe inclusion in an error
// message that may end up in a log line or an HTTP response body -
// herdr output is not something this package echoes back unbounded.
func boundedPrefix(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return s[:n] + "…(truncated)"
	}
	return s
}
