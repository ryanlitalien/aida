package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
	"github.com/ryanlitalien/aida/internal/execx"
)

const (
	gitTimeout    = 60 * time.Second
	gitMaxRepos   = 50 // hard cap to avoid walking the entire filesystem
	gitPerRepoCap = 30 // max output lines kept from any single repo
)

// GitAdapter runs `git log` (and other git subcommands) across every git
// repository under src.Path. The LLM Call #2 output is expected to be a
// single line of arguments passed verbatim to `git log`, e.g.:
//
//	--author="Ralph" -5 --pretty=format:"%h %an %ad %s" --date=short
//
// The adapter loops every .git/ subdirectory (1-2 levels deep under
// src.Path) and runs `git log <args>` in each. Results are tagged with
// the repo name so the synthesizer can attribute them.
//
// If src.Path points directly at a single git repo, only that one is
// searched. Otherwise, every child directory that is itself a git
// working tree is included.
type GitAdapter struct{}

// gitForcedFormat is the pretty-format string the adapter ALWAYS appends
// to git log invocations, regardless of what the LLM produced. Tab-
// delimited fields are easy to parse, %aI gives strict ISO 8601 author
// date so cross-repo sorting works, and the order is fixed so the
// synthesizer always sees the same shape.
const gitForcedFormat = `--pretty=format:%aI%x09%h%x09%an%x09%s --date=iso-strict`

// Args we strip from LLM-provided input because they fight with our
// forced format. The LLM is allowed to set --author, --since, --until,
// --grep, -<N>, etc.; everything that controls OUTPUT shape is rewritten.
var stripGitArgs = []string{
	"--oneline",
	"--pretty=format:",
	"--pretty=oneline",
	"--pretty=short",
	"--pretty=medium",
	"--pretty=full",
	"--pretty=fuller",
	"--pretty=raw",
	"--pretty=reference",
	"--format=",
	"--date=",
}

// Execute runs the git log command across all discovered repos.
func (a *GitAdapter) Execute(ctx context.Context, command string, src config.Source) (SourceResult, error) {
	result := SourceResult{
		Source: "git",
		Status: "success",
	}

	// Strip leading "git log" / "git " if the LLM helpfully included them,
	// then strip any output-shape flags so our forced format wins.
	args := strings.TrimSpace(command)
	args = stripLeading(args, "git log ")
	args = stripLeading(args, "git ")
	// Issue #14 Bug B: the LLM occasionally wraps the entire args string
	// in quotes (e.g. `"--oneline -n 20"`). tokenizeRespectingQuotes
	// would treat that as one opaque token and pass it through verbatim,
	// producing `git log "--oneline -n 20"` which git rejects with
	// `unrecognized option '--oneline -n 20'`. Strip outer wrapping
	// quotes here so the args parse normally.
	args = stripOuterQuotes(args)
	args = removeStripArgs(args)
	// Default to "-20" rows if the LLM didn't constrain count -- prevents
	// pulling thousands of commits per repo.
	if !hasCountFlag(args) {
		args = strings.TrimSpace(args + " -20")
	}
	// Default to --all so commits on feature branches and remote-tracking
	// refs are visible. Without this, "latest commit by <author>" misses
	// any work that hasn't landed on the default branch yet -- which is
	// exactly the common case (PR-in-progress, local feature branch).
	// The LLM is allowed to override by specifying a ref explicitly.
	if !hasRefSelector(args) {
		args = strings.TrimSpace(args + " --all")
	}
	finalArgs := strings.TrimSpace(args + " " + gitForcedFormat)

	rootPath := config.ExpandPath(src.Path)
	if rootPath == "" {
		result.Status = "error"
		result.Summary = "git source has no path"
		return result, nil
	}

	repos, err := findGitRepos(rootPath)
	if err != nil {
		result.Status = "error"
		result.Summary = fmt.Sprintf("discovering repos under %s: %s", rootPath, err)
		return result, nil
	}
	if len(repos) == 0 {
		result.Status = "empty"
		result.Summary = fmt.Sprintf("No git repositories found under %s", rootPath)
		return result, nil
	}

	execCtx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	var artifacts []Artifact
	perRepoSuccess := 0
	for _, repo := range repos {
		cmdLine := fmt.Sprintf("git -C %q log %s 2>/dev/null", repo, finalArgs)
		res, err := execx.RunShell(execCtx, cmdLine, execx.RunOpts{})
		if err != nil {
			continue
		}
		out := strings.TrimSpace(string(res.Stdout))
		if out == "" {
			continue
		}
		perRepoSuccess++
		repoName := filepath.Base(repo)
		lines := strings.Split(out, "\n")
		if len(lines) > gitPerRepoCap {
			lines = lines[:gitPerRepoCap]
		}
		for i, line := range lines {
			fields := strings.SplitN(line, "\t", 4)
			art := Artifact{
				Type: "commit",
				ID:   fmt.Sprintf("%s#%d", repoName, i),
			}
			if len(fields) == 4 {
				art.Timestamp = fields[0]
				art.Snippet = fmt.Sprintf("%s: %s %s | %s | %s",
					repoName, fields[0], fields[1], fields[2], fields[3])
			} else {
				art.Snippet = fmt.Sprintf("%s: %s", repoName, line)
			}
			artifacts = append(artifacts, art)
			if len(artifacts) >= 200 {
				break
			}
		}
		if len(artifacts) >= 200 {
			break
		}
	}

	if len(artifacts) == 0 {
		result.Status = "empty"
		result.Summary = fmt.Sprintf("No matching commits in %d repo(s) under %s (args: %s)", len(repos), rootPath, finalArgs)
		return result, nil
	}

	// Sort by timestamp desc so the synthesizer sees the freshest commit
	// first regardless of which repo it came from.
	sort.SliceStable(artifacts, func(i, j int) bool {
		return artifacts[i].Timestamp > artifacts[j].Timestamp
	})

	result.Artifacts = artifacts
	data, _ := json.Marshal(map[string]any{
		"repos_searched":  len(repos),
		"repos_with_hits": perRepoSuccess,
		"args":            finalArgs,
	})
	result.Data = data
	result.Summary = fmt.Sprintf("git log %s: %d commit(s) across %d/%d repos (sorted newest first)",
		finalArgs, len(artifacts), perRepoSuccess, len(repos))
	return result, nil
}

// removeStripArgs removes any token (or token=value) starting with one of
// the strip prefixes. Operates on whitespace-tokenized args while
// preserving anything else (including quoted strings like --author="X Y").
func removeStripArgs(args string) string {
	if args == "" {
		return ""
	}
	tokens := tokenizeRespectingQuotes(args)
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		drop := false
		for _, p := range stripGitArgs {
			if t == strings.TrimSuffix(p, "=") || t == strings.TrimSuffix(p, ":") {
				drop = true
				break
			}
			if strings.HasPrefix(t, p) {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, t)
		}
	}
	return strings.Join(out, " ")
}

// tokenizeRespectingQuotes splits on whitespace but keeps quoted strings
// intact: `--author="Ralph" -5` -> [`--author="Ralph"`, `-5`].
func tokenizeRespectingQuotes(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			cur.WriteRune(r)
		case (r == ' ' || r == '\t') && !inQuote:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// hasCountFlag detects -N / -nN / --max-count=N so we don't tack on a
// default -20 when the LLM already set a limit.
func hasCountFlag(args string) bool {
	for _, t := range tokenizeRespectingQuotes(args) {
		if strings.HasPrefix(t, "--max-count") || strings.HasPrefix(t, "-n") {
			return true
		}
		if len(t) >= 2 && t[0] == '-' && t[1] >= '0' && t[1] <= '9' {
			return true
		}
	}
	return false
}

// hasRefSelector returns true if args already specify which refs to walk
// (--all, --branches, --remotes, --tags, HEAD~N, a branch name, etc.).
// We only auto-add --all when none of these are present, so explicit
// LLM intent wins.
func hasRefSelector(args string) bool {
	for _, t := range tokenizeRespectingQuotes(args) {
		switch {
		case t == "--all",
			t == "HEAD",
			strings.HasPrefix(t, "--branches"),
			strings.HasPrefix(t, "--remotes"),
			strings.HasPrefix(t, "--tags"),
			strings.HasPrefix(t, "HEAD~"),
			strings.HasPrefix(t, "HEAD^"):
			return true
		}
		// Bare branch/sha tokens (no leading dash, no = sign): assume
		// the LLM is naming a ref. Skip option-looking tokens and bare
		// numbers (the "1" from "-n 1" is NOT a branch name).
		if t != "" && !strings.HasPrefix(t, "-") && !strings.Contains(t, "=") && !strings.Contains(t, "\"") && !isAllDigits(t) {
			return true
		}
	}
	return false
}

// ParseOutput is unused by GitAdapter (Execute builds artifacts directly),
// but required by the Adapter interface.
func (a *GitAdapter) ParseOutput(raw []byte, src config.Source) ([]Artifact, error) {
	return nil, nil
}

// findGitRepos returns every git working tree under rootPath, up to
// gitMaxRepos. If rootPath IS a git repo itself, returns just that one.
// Otherwise walks 1 level deep (child directories only) -- deep enough
// to find ~/dev/<repo>, shallow enough to avoid walking entire
// filesystem trees.
func findGitRepos(rootPath string) ([]string, error) {
	info, err := os.Stat(rootPath)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("not a directory: %s", rootPath)
	}
	if _, err := os.Stat(filepath.Join(rootPath, ".git")); err == nil {
		return []string{rootPath}, nil
	}
	entries, err := os.ReadDir(rootPath)
	if err != nil {
		return nil, err
	}
	var repos []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		sub := filepath.Join(rootPath, e.Name())
		if _, err := os.Stat(filepath.Join(sub, ".git")); err == nil {
			repos = append(repos, sub)
			if len(repos) >= gitMaxRepos {
				break
			}
		}
	}
	sort.Strings(repos)
	return repos, nil
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}

func stripLeading(s, prefix string) string {
	if strings.HasPrefix(strings.ToLower(s), strings.ToLower(prefix)) {
		return strings.TrimSpace(s[len(prefix):])
	}
	return s
}

// stripOuterQuotes removes a single matching pair of leading/trailing
// quotes ("..." or '...') from s if present. Used to recover from LLM
// outputs that wrap the entire args string in quotes.
func stripOuterQuotes(s string) string {
	if len(s) < 2 {
		return s
	}
	if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}
