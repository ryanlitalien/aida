// golden-log runs every question in ~/.aida/golden-questions.txt through
// `aida`, parses the routing and answer, scores each result against the
// expectations in ~/.aida/golden-expectations.yaml, and writes a markdown
// log to test-runs/run-{ts}-{group}.md with per-question detail and an
// overall confidence summary.
//
// The questions/expectations files are personal seed data and live in
// ~/.aida/, not this repo -- `aida init` seeds them from the fictional
// examples/golden/ set so this runs out of the box; replace them with your
// own real questions once you have real sources configured.
//
// Usage:
//
//	go run ./cmd/golden-log -group group-a
//	go run ./cmd/golden-log -group group-a -aida /custom/path/aida
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type expectation struct {
	Q          int      `yaml:"q"`
	Expected   string   `yaml:"expected"`
	Acceptable []string `yaml:"acceptable"`
}

type result struct {
	Q          int
	Question   string
	Expected   string
	Acceptable []string
	Picked     []string
	Reason     string
	Answer     string
	Status     string
	RunID      string
	Confidence int // 0-100
	Pass       bool
}

func main() {
	home, _ := os.UserHomeDir()

	group := flag.String("group", "ungrouped", "label for this run, e.g. group-a")
	aida := flag.String("aida", "", "path to aida binary (default: ~/go/bin/aida)")
	questionsPath := flag.String("questions", filepath.Join(home, ".aida", "golden-questions.txt"), "path to questions file (default: the personal seed file aida init writes)")
	expectsPath := flag.String("expects", filepath.Join(home, ".aida", "golden-expectations.yaml"), "path to expectations yaml (default: the personal seed file aida init writes)")
	outDir := flag.String("out", "test-runs", "output directory for the log")
	flag.Parse()

	aidaBin := *aida
	if aidaBin == "" {
		aidaBin = filepath.Join(home, "go", "bin", "aida")
	}

	expects := loadExpectations(*expectsPath)
	questions := loadQuestions(*questionsPath)

	if err := os.MkdirAll(*outDir, 0755); err != nil {
		fmt.Fprintln(os.Stderr, "mkdir:", err)
		os.Exit(1)
	}

	ts := time.Now().Format("2006-01-02-1504")
	outPath := filepath.Join(*outDir, fmt.Sprintf("run-%s-%s.md", ts, *group))
	out, err := os.Create(outPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "create:", err)
		os.Exit(1)
	}
	defer out.Close()

	fmt.Fprintf(out, "# Aida Test Run - %s\n\n", *group)
	fmt.Fprintf(out, "Generated: %s\n", time.Now().Format("2006-01-02 15:04:05 -0700"))
	branch, _ := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD").Output()
	fmt.Fprintf(out, "Branch: %s\n", strings.TrimSpace(string(branch)))
	fmt.Fprintf(out, "Binary: %s\n\n", aidaBin)
	fmt.Fprintf(out, "---\n\n")

	var results []result
	for i, q := range questions {
		idx := i + 1
		fmt.Fprintf(os.Stderr, "[%d/%d] %s\n", idx, len(questions), truncate(q, 60))
		exp := expects[idx]

		raw, status := runAida(aidaBin, q)
		picked, reason, answer, runID := parseAidaOutput(raw)

		r := result{
			Q:          idx,
			Question:   q,
			Expected:   exp.Expected,
			Acceptable: exp.Acceptable,
			Picked:     picked,
			Reason:     reason,
			Answer:     answer,
			Status:     status,
			RunID:      runID,
		}
		r.Confidence = score(r)
		r.Pass = r.Confidence >= 70
		results = append(results, r)

		writeEntry(out, r)
	}

	writeSummary(out, results)
	fmt.Fprintf(os.Stderr, "\nDone. Wrote %s\n", outPath)
}

func loadExpectations(path string) map[int]expectation {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read expectations:", err)
		os.Exit(1)
	}
	var list []expectation
	if err := yaml.Unmarshal(data, &list); err != nil {
		fmt.Fprintln(os.Stderr, "parse expectations:", err)
		os.Exit(1)
	}
	out := make(map[int]expectation, len(list))
	for _, e := range list {
		out[e.Q] = e
	}
	return out
}

func loadQuestions(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read questions:", err)
		os.Exit(1)
	}
	var qs []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		qs = append(qs, line)
	}
	return qs
}

func runAida(aida, question string) (string, string) {
	cmd := exec.Command(aida, question, "--verbose")
	cmd.Env = append(os.Environ(), "NO_COLOR=1")
	out, err := cmd.CombinedOutput()
	clean := stripANSI(string(out))
	status := "ok"
	if err != nil {
		status = fmt.Sprintf("exit_%v", err)
	}
	return clean, status
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)
var spinnerRE = regexp.MustCompile(`[\x{2800}-\x{28ff}]`)

func stripANSI(s string) string {
	s = ansiRE.ReplaceAllString(s, "")
	s = spinnerRE.ReplaceAllString(s, "")
	return s
}

var (
	pickedRE = regexp.MustCompile(`LLM router: picked \[([^\]]+)\] -- (.+)`)
	routeRE  = regexp.MustCompile(`🔀 Routing to: \[([^\]]+)\]`)
	runIDRE  = regexp.MustCompile(`Run log: .*runs/([^.]+)\.json`)
)

func parseAidaOutput(raw string) (picked []string, reason, answer, runID string) {
	if m := pickedRE.FindStringSubmatch(raw); len(m) >= 3 {
		picked = strings.Split(m[1], ", ")
		reason = strings.TrimSpace(m[2])
	} else if m := routeRE.FindStringSubmatch(raw); len(m) >= 2 {
		picked = strings.Split(m[1], ", ")
	}
	// Trim everything to just the source name (strip score etc).
	for i, p := range picked {
		p = strings.TrimSpace(p)
		// Drop trailing " (score)" if present
		if idx := strings.Index(p, " ("); idx > 0 {
			p = p[:idx]
		}
		picked[i] = p
	}
	if m := runIDRE.FindStringSubmatch(raw); len(m) >= 2 {
		runID = m[1]
	}
	// Pull the answer block: lines between "Answer" header and the
	// trailing "Timing:" footer.
	lines := strings.Split(raw, "\n")
	inAnswer := false
	var ans []string
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "Answer" {
			inAnswer = true
			continue
		}
		if strings.HasPrefix(t, "Timing:") {
			inAnswer = false
		}
		if inAnswer {
			if strings.HasPrefix(t, "─") || t == "" {
				continue
			}
			ans = append(ans, t)
			if len(ans) >= 6 {
				break
			}
		}
	}
	answer = strings.Join(ans, " ")
	return picked, reason, answer, runID
}

// score computes a 0-100 confidence per the rules in the plan:
//   - start at 50
//   - +30 if routed source matches expected
//   - +15 if routed source is in acceptable
//   - +20 if answer contains specific data (numbers, dates, file paths)
//   - −20 if status is empty/error/timeout
//   - −15 if answer contains "no results" / "I cannot find" / "no data"
func score(r result) int {
	c := 50
	pickedFirst := ""
	if len(r.Picked) > 0 {
		pickedFirst = r.Picked[0]
	}
	if pickedFirst == r.Expected {
		c += 30
	} else if containsStr(r.Acceptable, pickedFirst) {
		c += 15
	}
	a := strings.ToLower(r.Answer)
	hasNumbers := regexp.MustCompile(`\b\d`).MatchString(a)
	hasFilePath := strings.Contains(a, "/") || strings.Contains(a, ".go") || strings.Contains(a, ".md")
	if hasNumbers || hasFilePath {
		c += 20
	}
	if r.Status != "ok" {
		c -= 20
	}
	for _, neg := range []string{"no results", "i cannot find", "no data", "i don't have", "not enough information", "do not contain", "did not return", "search returned no"} {
		if strings.Contains(a, neg) {
			c -= 15
			break
		}
	}
	if c < 0 {
		c = 0
	}
	if c > 100 {
		c = 100
	}
	return c
}

func containsStr(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func writeEntry(out *os.File, r result) {
	expectedDisp := r.Expected
	if len(r.Acceptable) > 0 {
		expectedDisp += " (or " + strings.Join(r.Acceptable, ", ") + ")"
	}
	pickedDisp := strings.Join(r.Picked, ", ")
	if pickedDisp == "" {
		pickedDisp = "(none)"
	}
	mark := "✗"
	if r.Pass {
		mark = "✓"
	}
	fmt.Fprintf(out, "## Q%d: %q\n\n", r.Q, r.Question)
	fmt.Fprintf(out, "**Expected source:** %s\n\n", expectedDisp)
	fmt.Fprintf(out, "**Routed to:** %s\n\n", pickedDisp)
	if r.Reason != "" {
		fmt.Fprintf(out, "**Routing reason (LLM):** %s\n\n", r.Reason)
	}
	if r.RunID != "" {
		fmt.Fprintf(out, "**Run id:** `%s`\n\n", r.RunID)
	}
	if r.Answer != "" {
		fmt.Fprintf(out, "**Answer:**\n\n> %s\n\n", truncate(r.Answer, 600))
	}
	fmt.Fprintf(out, "**Confidence:** %d%%  **Status:** %s %s\n\n", r.Confidence, r.Status, mark)
	fmt.Fprintf(out, "---\n\n")
}

func writeSummary(out *os.File, results []result) {
	total := len(results)
	correct := 0
	high, med, low := 0, 0, 0
	sum := 0
	var failures []result
	for _, r := range results {
		if r.Pass {
			correct++
		} else {
			failures = append(failures, r)
		}
		sum += r.Confidence
		switch {
		case r.Confidence >= 85:
			high++
		case r.Confidence >= 50:
			med++
		default:
			low++
		}
	}
	avg := 0
	if total > 0 {
		avg = sum / total
	}

	fmt.Fprintf(out, "## Summary\n\n")
	fmt.Fprintf(out, "| Metric | Value |\n|---|---|\n")
	fmt.Fprintf(out, "| Total questions | %d |\n", total)
	fmt.Fprintf(out, "| Pass (≥70%%) | %d/%d (%.0f%%) |\n", correct, total, float64(correct)/float64(total)*100)
	fmt.Fprintf(out, "| High confidence (≥85%%) | %d |\n", high)
	fmt.Fprintf(out, "| Medium confidence (50-84%%) | %d |\n", med)
	fmt.Fprintf(out, "| Low confidence (<50%%) | %d |\n", low)
	fmt.Fprintf(out, "| Average confidence | %d%% |\n\n", avg)

	if len(failures) > 0 {
		fmt.Fprintf(out, "## Failures (confidence < 70%%)\n\n")
		sort.Slice(failures, func(i, j int) bool { return failures[i].Confidence < failures[j].Confidence })
		for _, r := range failures {
			pickedFirst := ""
			if len(r.Picked) > 0 {
				pickedFirst = r.Picked[0]
			}
			fmt.Fprintf(out, "- **Q%d** (%d%%) - expected `%s`, got `%s`: %s\n",
				r.Q, r.Confidence, r.Expected, pickedFirst, truncate(r.Question, 60))
		}
	}
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// silence unused-import on platforms without strconv use
var _ = strconv.Itoa
