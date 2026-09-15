package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/remotex"
)

// HerdrBin is the absolute path to herdr on minty. herdr is only on PATH
// inside a login shell (see ~/dev/aida-agents/AGENTS.md's Lane 4 notes:
// /etc/profile.d/herdr.sh only runs for one); remotex.RunRemote's sudo
// path already runs `sudo -iu <user> /bin/sh -s`, a login shell, but this
// package targets herdr by absolute path anyway rather than betting on
// that -- it costs nothing and never depends on shell-init ordering.
const HerdrBin = "/opt/herdr/bin/herdr"

// nameRe is the fleet-start agent-name allowlist. name becomes a herdr
// agent/workspace label, a filename component (<name>.task, <name>.log
// under agents-lane4/), and part of a job's source_ref -- restricting it
// up front to POSIX-filename-safe, herdr-safe characters means every
// downstream use is safe for free, no distinct escaping needed anywhere
// name is threaded through.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// maxNameLen bounds name the same way remotex.ValidateTarget bounds a
// herdr target -- generous for any real run name, small enough to keep
// it out of any path-length or argv-length edge case.
const maxNameLen = 128

// ValidateName rejects anything that isn't a plain, filename-safe herdr
// agent name. Checked before any remote call so a typo fails instantly,
// not three ssh round trips in.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("fleet: name is required")
	}
	if len(name) > maxNameLen {
		return fmt.Errorf("fleet: name too long (max %d): %q", maxNameLen, name)
	}
	if !nameRe.MatchString(name) {
		return fmt.Errorf("fleet: invalid name %q (must match %s)", name, nameRe.String())
	}
	return nil
}

// TaskPath returns the absolute path on host where a lane 4 run's task
// text lives, e.g. /home/ryan/agents-lane4/scarlett-978.task -- what
// lane4-run.sh reads via its own $TASK_FILE.
func TaskPath(user, name string) string {
	return fmt.Sprintf("/home/%s/agents-lane4/%s.task", user, name)
}

// LogPath mirrors TaskPath for the launcher's log file -- what `aida
// fleet watch --log` tails for the EXIT marker.
func LogPath(user, name string) string {
	return fmt.Sprintf("/home/%s/agents-lane4/%s.log", user, name)
}

// LauncherPath returns the absolute path to the generic lane 4 launcher
// on host for user, e.g. /home/ryan/agents-lane4/lane4-run.sh (the
// twin of ~/dev/aida-agents/scripts/lane4-run.sh).
func LauncherPath(user string) string {
	return fmt.Sprintf("/home/%s/agents-lane4/lane4-run.sh", user)
}

// TaskFileWriteArgv builds the argv for the one remote command that
// delivers a lane 4 run's task text: mkdir -p the agents-lane4
// directory (idempotent -- it may not exist yet, e.g. a user's very
// first fleet start run) and write content to path via printf, never
// via a shell-interpolated heredoc.
//
// This is the ONLY place in aida that ever writes lane 4 task text, and
// it is reached ONLY from `aida fleet start` (see
// internal/cli/fleet_start.go's package doc) -- deliberately never
// registered on an HTTP route. remotex.RunRemote's own BuildRemoteScript
// already base64-encodes every argv element before it ever reaches a
// shell (internal/remotex/script.go), so this function's only job is
// choosing a script whose *shape* is safe: content becomes "$2", never
// spliced into the script text itself.
func TaskFileWriteArgv(path, content string) []string {
	return []string{
		"sh", "-c",
		`mkdir -p "$(dirname "$1")" && printf '%s' "$2" > "$1"`,
		"_", path, content,
	}
}

// WriteTask writes task's text to path on host as user, via
// remotex.RunRemote / TaskFileWriteArgv. See TaskFileWriteArgv's doc
// comment for why this is the only code path that ever does this.
func WriteTask(ctx context.Context, r remotex.Runner, host, user, path, task string, timeout time.Duration) error {
	res, err := remotex.RunRemote(ctx, r, host, user, TaskFileWriteArgv(path, task), timeout)
	return checkTransport(res, err, "write task file")
}

// shellQuote wraps s in single quotes, escaping any embedded single
// quote the POSIX way (close the quote, emit an escaped quote via
// backslash-quote, reopen). Defense in depth -- every value LauncherArgv
// quotes already passed ValidateName or a fixed flag charset, except cwd
// (validated absolute by the caller) -- but this is the string that ends
// up INSIDE a `bash -lc "..."` argv element, so it gets the same
// treatment regardless of what validated the value going in.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// LauncherArgv builds the `bash -lc "..."` argv that `herdr agent
// start ... --` launches: an invocation of lane4-run.sh (see
// ~/dev/aida-agents/scripts/lane4-run.sh) with the flags it understands.
// bash -l (a login shell) is required both for herdr's own PATH note
// and for the launcher's own `. "$ENV_FILE"` sourcing to behave like an
// interactive `sudo -iu <user>` session would.
//
// keepOpen <= 0 omits --keep-open, leaving the launcher's own 600s
// default; a positive value is normally only useful for a quick
// self-test run so the pane doesn't sit open for ten minutes.
func LauncherArgv(user, name, claudeAgent, cwd, effort, model string, keepOpen int) []string {
	parts := []string{
		shellQuote(LauncherPath(user)),
		shellQuote(name),
		"--agent", shellQuote(claudeAgent),
		"--repo", shellQuote(cwd),
		"--effort", shellQuote(effort),
		"--opus", shellQuote(model),
	}
	if keepOpen > 0 {
		parts = append(parts, "--keep-open", strconv.Itoa(keepOpen))
	}
	return []string{"bash", "-lc", strings.Join(parts, " ")}
}

// AgentListArgv builds `herdr agent list`'s argv -- the pre-flight
// existence check StartAgent runs before ever creating a workspace.
func AgentListArgv() []string {
	return []string{HerdrBin, "agent", "list"}
}

// WorkspaceCreateArgv builds `herdr workspace create`'s argv. label is
// the run name so a workspace shows up on /bifrost (or `herdr workspace
// list`) tagged with the run it belongs to.
func WorkspaceCreateArgv(cwd, label string) []string {
	return []string{HerdrBin, "workspace", "create", "--cwd", cwd, "--label", label, "--no-focus"}
}

// AgentStartArgv builds `herdr agent start`'s argv: start name in
// workspaceID, cwd, running launch (see LauncherArgv) after `--`.
func AgentStartArgv(name, workspaceID, cwd string, launch []string) []string {
	argv := []string{HerdrBin, "agent", "start", name, "--workspace", workspaceID, "--cwd", cwd, "--no-focus", "--"}
	return append(argv, launch...)
}

// AlreadyExistsError means StartAgent's pre-flight `herdr agent list`
// found an existing agent by this name -- the run was refused before
// any workspace was created or anything was started.
type AlreadyExistsError struct {
	Name string
}

func (e *AlreadyExistsError) Error() string {
	return fmt.Sprintf("fleet: herdr agent %q already exists on this lane (see herdr agent list / herdr agent attach %s)", e.Name, e.Name)
}

// StartResult is what StartAgent learned from the herdr-side start
// sequence.
type StartResult struct {
	// WorkspaceID is the herdr workspace the agent was started in --
	// useful for `herdr workspace close <id>` cleanup and for debugging
	// a run by hand.
	WorkspaceID string
}

// StartAgent runs the herdr-side lane 4 launch sequence: exactly three
// remote commands, in order:
//
//  1. `herdr agent list` -- refuse with *AlreadyExistsError if name is
//     already a herdr agent, before anything is created.
//  2. `herdr workspace create --cwd cwd --label name --no-focus` --
//     parse the new workspace_id out of the result.
//  3. `herdr agent start name --workspace <id> --cwd cwd --no-focus --
//     launchArgv...` -- the actual launch.
//
// name is validated (ValidateName) before any of the three calls, so an
// invalid name never reaches the runner at all. Task text delivery
// (WriteTask) is deliberately NOT part of this sequence -- see
// TaskFileWriteArgv's doc comment -- callers write the task file first.
func StartAgent(ctx context.Context, r remotex.Runner, host, user, cwd, name string, launchArgv []string, timeout time.Duration) (StartResult, error) {
	if err := ValidateName(name); err != nil {
		return StartResult{}, err
	}

	listOut, err := runHerdr(ctx, r, host, user, AgentListArgv(), timeout, "herdr agent list")
	if err != nil {
		return StartResult{}, err
	}
	exists, err := AgentExists(listOut, name)
	if err != nil {
		return StartResult{}, err
	}
	if exists {
		return StartResult{}, &AlreadyExistsError{Name: name}
	}

	wsOut, err := runHerdr(ctx, r, host, user, WorkspaceCreateArgv(cwd, name), timeout, "herdr workspace create")
	if err != nil {
		return StartResult{}, err
	}
	workspaceID, err := ParseWorkspaceID(wsOut)
	if err != nil {
		return StartResult{}, err
	}

	if _, err := runHerdr(ctx, r, host, user, AgentStartArgv(name, workspaceID, cwd, launchArgv), timeout, "herdr agent start"); err != nil {
		return StartResult{WorkspaceID: workspaceID}, err
	}
	return StartResult{WorkspaceID: workspaceID}, nil
}

// herdrEnvelope is herdr's top-level response shape for every
// JSON-emitting subcommand -- mirrors internal/bifrost/types.go's
// envelope. Declared separately here (not imported from bifrost)
// because this package deliberately calls herdr directly over
// remotex.RunRemote rather than through the bifrost.Client -- see
// bifrost.Client.StartPreset's doc comment on why that package never
// accepts a caller-supplied argv; internal/fleet's callers (aida fleet
// start, a CLI-only surface) are exactly the exception that rule
// carves out.
type herdrEnvelope struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *herdrError     `json:"error,omitempty"`
}

type herdrError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// parseHerdrEnvelope tolerates leading noise on stdout -- ssh MOTDs are
// real -- by scanning for the first '{' and decoding from there, same
// approach as internal/bifrost's parseEnvelope.
func parseHerdrEnvelope(op string, stdout []byte) (herdrEnvelope, error) {
	idx := bytes.IndexByte(stdout, '{')
	if idx < 0 {
		return herdrEnvelope{}, fmt.Errorf("fleet: %s: no JSON object found in output (received: %q)", op, boundedPrefix(stdout, 500))
	}
	var env herdrEnvelope
	dec := json.NewDecoder(bytes.NewReader(stdout[idx:]))
	if err := dec.Decode(&env); err != nil {
		return herdrEnvelope{}, fmt.Errorf("fleet: %s: parse: %w (received: %q)", op, err, boundedPrefix(stdout, 500))
	}
	if env.Error != nil {
		return herdrEnvelope{}, fmt.Errorf("fleet: %s: %s: %s", op, env.Error.Code, env.Error.Message)
	}
	return env, nil
}

// workspaceCreateResult is the `result` payload of `herdr workspace
// create` -- only the field this package needs (see the real captured
// shape in start_test.go: herdr also returns root_pane/tab siblings we
// don't use).
type workspaceCreateResult struct {
	Workspace struct {
		WorkspaceID string `json:"workspace_id"`
	} `json:"workspace"`
}

// ParseWorkspaceID extracts the workspace_id `herdr workspace create`
// assigned from stdout's full envelope. Returns an error if stdout
// isn't a valid herdr envelope, herdr reported an error, or the result
// has no non-empty workspace_id.
func ParseWorkspaceID(stdout []byte) (string, error) {
	const op = "herdr workspace create"
	env, err := parseHerdrEnvelope(op, stdout)
	if err != nil {
		return "", err
	}
	var wc workspaceCreateResult
	if err := json.Unmarshal(env.Result, &wc); err != nil {
		return "", fmt.Errorf("fleet: %s: parse result: %w", op, err)
	}
	if wc.Workspace.WorkspaceID == "" {
		return "", fmt.Errorf("fleet: %s: no workspace_id in result (received: %q)", op, boundedPrefix(stdout, 500))
	}
	return wc.Workspace.WorkspaceID, nil
}

// agentListResult is the `result` payload of `herdr agent list`.
// Both Name and Agent are accepted -- the herdr version this was built
// against (0.7.4) emits "name"; "agent" is accepted defensively in
// case a future/older herdr uses it instead, same belt-and-braces
// spirit as internal/bifrost.Agent carrying both fields.
type agentListResult struct {
	Agents []struct {
		Name  string `json:"name"`
		Agent string `json:"agent"`
	} `json:"agents"`
}

// AgentExists reports whether `herdr agent list`'s stdout (full
// envelope) already lists an agent named name.
func AgentExists(stdout []byte, name string) (bool, error) {
	const op = "herdr agent list"
	env, err := parseHerdrEnvelope(op, stdout)
	if err != nil {
		return false, err
	}
	var list agentListResult
	if err := json.Unmarshal(env.Result, &list); err != nil {
		return false, fmt.Errorf("fleet: %s: parse result: %w", op, err)
	}
	for _, a := range list.Agents {
		if a.Name == name || a.Agent == name {
			return true, nil
		}
	}
	return false, nil
}

// runHerdr runs one herdr subcommand through remotex.RunRemote, checks
// the transport-level outcome (ssh error, timeout, nonzero exit), and
// returns raw stdout for the caller to parse with ParseWorkspaceID /
// AgentExists / etc.
func runHerdr(ctx context.Context, r remotex.Runner, host, user string, argv []string, timeout time.Duration, op string) ([]byte, error) {
	res, err := remotex.RunRemote(ctx, r, host, user, argv, timeout)
	if cerr := checkTransport(res, err, op); cerr != nil {
		return nil, cerr
	}
	return res.Stdout, nil
}

// checkTransport turns a remotex.Result/error pair into a single error
// when the ssh/sudo hop itself failed, timed out, or the remote command
// exited nonzero -- distinct from herdr reporting its own {"error":...}
// envelope on a zero exit, which parseHerdrEnvelope catches instead.
func checkTransport(res remotex.Result, err error, op string) error {
	if err != nil {
		return fmt.Errorf("fleet: %s: %w", op, err)
	}
	if res.TimedOut {
		return fmt.Errorf("fleet: %s: timed out", op)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("fleet: %s: exit %d: %s", op, res.ExitCode, boundedPrefix(res.Stderr, 500))
	}
	return nil
}

// boundedPrefix trims and truncates b for safe inclusion in an error
// message -- mirrors internal/bifrost's helper of the same name; herdr
// output isn't something this package echoes back unbounded.
func boundedPrefix(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return s[:n] + "…(truncated)"
	}
	return s
}

// DefaultQuestion returns the job question `aida fleet start` records
// when --question is omitted: task's text collapsed to one line (so a
// multi-line task doesn't wrap /runs' table) and truncated to 80
// runes.
func DefaultQuestion(task string) string {
	line := strings.Join(strings.Fields(task), " ")
	r := []rune(line)
	if len(r) > 80 {
		return string(r[:80])
	}
	return line
}
