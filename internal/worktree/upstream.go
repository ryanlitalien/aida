package worktree

import (
	"os/exec"
	"strings"
)

// ResolveUpstream returns the remote name and remote-tracking ref for branch,
// e.g. ("public", "public/main"). It reads git's own record of where the
// branch lives (branch.<branch>.remote and branch.<branch>.merge), so it is
// correct in any repo without configuration: a repo whose upstream is named
// "public" or "upstream" resolves to that remote, not to a hardcoded "origin".
//
// It reads the config keys directly rather than asking for <branch>@{upstream},
// because the config records intent even before the remote-tracking ref has
// been fetched (a fresh clone with a hand-set upstream), whereas @{upstream}
// errors out in that state and would silently degrade to the fallback below.
//
// Cases:
//
//   - branch tracks a remote: (remote, remote/<merge-branch>), where the merge
//     branch defaults to branch itself when branch.<branch>.merge is unset.
//   - branch tracks a local branch (remote "."): ("", <merge-branch>). The ref
//     is the local branch it tracks, and the empty remote says there is
//     nothing to push to.
//   - branch has no upstream, or the repo has no remotes at all: the fallback
//     ("origin", "origin/<branch>"). This preserves the pre-resolver behavior
//     exactly for any repo where origin is the only remote.
//
// The returned ref is never empty.
func ResolveUpstream(repoRoot, branch string) (remote, ref string) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		branch = "main"
	}
	remote = gitConfig(repoRoot, "branch."+branch+".remote")
	if remote == "" {
		return "origin", "origin/" + branch
	}
	merge := strings.TrimPrefix(gitConfig(repoRoot, "branch."+branch+".merge"), "refs/heads/")
	if merge == "" {
		merge = branch
	}
	if remote == "." {
		return "", merge
	}
	return remote, remote + "/" + merge
}

// Remotes returns the names of the repo's configured remotes (git remote),
// or nil when there are none or git fails.
func Remotes(repoRoot string) []string {
	out, err := exec.Command("git", "-C", repoRoot, "remote").Output()
	if err != nil {
		return nil
	}
	var names []string
	for _, ln := range strings.Split(string(out), "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			names = append(names, ln)
		}
	}
	return names
}

// RemoteURL returns the fetch URL of the named remote, or "" when it is not
// configured.
func RemoteURL(repoRoot, remote string) string {
	if remote == "" {
		return ""
	}
	out, err := exec.Command("git", "-C", repoRoot, "remote", "get-url", remote).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// remoteOfRef returns the remote name that ref's first path segment names,
// or "" when that segment is not a configured remote (a local branch, a tag,
// a SHA, or "origin/x" in a repo that has no origin).
func remoteOfRef(repoRoot, ref string) string {
	name, _, ok := strings.Cut(ref, "/")
	if !ok || name == "" {
		return ""
	}
	for _, r := range Remotes(repoRoot) {
		if r == name {
			return name
		}
	}
	return ""
}

// gitConfig returns the value of a git config key in repoRoot, or "" when it
// is unset (git config --get exits 1 for a missing key).
func gitConfig(repoRoot, key string) string {
	out, err := exec.Command("git", "-C", repoRoot, "config", "--get", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
