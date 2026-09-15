package runs

import (
	"regexp"
	"strconv"
)

var taskRefPattern = regexp.MustCompile(`(?i)task #(\d+)`)

// extractTaskRef pulls a `task #N` reference out of a run's answer string.
// Tied to the saveTaskRun answer format ("Showed task #N", "Task #N added",
// "Completed task #N") - enough for anaphor resolution. Returns 0 when no
// reference is present.
func extractTaskRef(answer string) int {
	m := taskRefPattern.FindStringSubmatch(answer)
	if len(m) < 2 {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return n
}

// LatestTaskRefInCwd returns the task ID referenced by the most recent
// task-action run in cwd within withinMinutes, or 0 if none. Used to
// resolve task anaphors ("mark that one as done") to the task the user
// just shown/created/completed instead of falling back to "first open
// task in the list."
func LatestTaskRefInCwd(cwd string, withinMinutes int) int {
	prior, err := FindLatestInCwd(cwd, withinMinutes)
	if err != nil || prior == nil {
		return 0
	}
	if prior.Action != "task" {
		return 0
	}
	return extractTaskRef(prior.Answer)
}
