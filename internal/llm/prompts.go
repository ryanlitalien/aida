package llm

import (
	"fmt"
	"sort"
	"strings"
)

// SourceResultSummary holds the output from a single source execution
// for use in the synthesis prompt.
//
// Command is the exact query string that was sent to the adapter. It is
// surfaced to the synthesizer so it can reason about whether the command's
// scope actually covered the user's question - without it, the synthesizer
// has no way to notice e.g. a `--owner ryanlitalien` search can't answer
// a question about the Acme Widgets org, and may confidently declare
// "none of these are acme-widgets" based on an off-scope result set.
type SourceResultSummary struct {
	Source    string
	Status    string
	Command   string
	Summary   string
	Artifacts []string
	// FromContextDoc is true when Artifacts came from the source's context
	// file (CLAUDE.md, README.md, etc.) rather than from querying the
	// source itself. See sources.SourceResult.FromContextDoc for the
	// full explanation of why this must never be treated as data.
	FromContextDoc bool
}

// --- Intent Parsing Prompts ---

// IntentParseSystemPrompt is the system prompt for extracting structured
// intent from a natural language user question. This is LLM Call #1 in
// the six-step pipeline.
const IntentParseSystemPrompt = `You are an intent parser for a CLI tool. Extract structured fields from the user's question.

Return ONLY valid JSON matching this schema:
{
  "raw_entities": ["<any identifiers, names, ARIs, URLs, API keys found>"],
  "timeframe": "<relative time like 'yesterday', '1h', 'last week', or null>",
  "action": "<one of: investigate, query, lookup, record, test, search, task, agent>",
  "keywords": ["<relevant terms>"],
  "amount": <number or null>,
  "category": "<if applicable: food, transport, etc., or null>",
  "task_action": "<if action is task: one of create, list, done, lookup, or null>",
  "task_title": "<if creating a task: the task title/description, or null>",
  "comprehensive_intent": <true when user asks for ALL/EVERY/COMPLETE results, false otherwise>,
  "effective_question": "<rewrite of the current question that weaves in prior-turn scope, ONLY for referential follow-ups when a PRIOR TURN is available; otherwise null>"
}

Action guide:
- "what happened" / "why did" / "debug" → investigate
- "how much" / "show me" / "what was" / "count of" / "how many" → query
- "what is" / "look up" → lookup
- "I paid" / "log" / "record" → record
- "test" / "try" / "check" / "validate" → test
- "search" / "where is" / "grep" / "find in code" → search
- "find" with a metric (errors, 200s, 500s, latency, count) → query
- "find" with a file or code reference → search
- "do X then Y" / "write code" / "create a PR" / "fix and deploy" / multi-step workflows → agent
- tasks / todos / "create a task" / "add a task" / "what are my tasks" / "mark X done" / "what should I work on" / "my priorities" → task
  - "create" / "add" / "remind me to" / "I need to" → task_action: create, task_title: the task description
  - IMPORTANT: "create" referring to domain objects (budgets, records, accounts, PRs, files) is NOT a task - it's a query, record, or agent action. Only use task when the user wants to track something as a todo/reminder.
  - "list" / "show" / "what are" / "open" / "priorities" / "what should I" → task_action: list
  - "show me task #N" / "what is task #N" / "tell me about task #N" / a bare "#N" reference → task_action: lookup, and put "#N" in raw_entities. Use lookup (not list) whenever the user is asking about ONE specific task by ID or slug.
  - "mark" / "complete" / "finish" / "close" / "done" → task_action: done, task_title: the name of the task being completed (just the task name, not the action words)

REFERENTIAL FOLLOW-UPS:
- If a "PRIOR TURN" block is provided above the question, the user may be asking a follow-up that REFERS to that prior turn ("do the same thing, but for issues", "again for last month", "what about in acme_widgets", "and for Y", "also X").
- When the current question uses such a reference AND the prior turn is present: (1) INHERIT raw_entities from the prior turn except where the current question explicitly changes them; (2) ALSO set effective_question to a standalone rewrite that weaves the prior turn's scope into the current question so downstream stages do not need to know about the prior turn. Example: prior "what are my open PRs across acme-widgets and aida* github repos" + current "do the same thing, but for issues" → effective_question: "what are my open issues across acme-widgets and aida* github repos". The action/keywords follow the current question; timeframe is extracted from the current question only (not inherited).
- If the current question is NOT referential (it stands on its own), IGNORE the prior turn - do not pull its entities in, and leave effective_question null.
- If the question IS referential but NO prior turn is provided, return an empty raw_entities array and leave effective_question null. The CLI will surface that to the user; do not invent scope.`

// PriorTurn is the llm-package view of the most recent in-cwd run,
// surfaced to the intent parser so referential follow-ups ("same thing",
// "again", "but for X") can inherit scope from the previous turn.
type PriorTurn struct {
	Question  string
	Entities  []string
	Action    string
	Timestamp string
}

// IntentParseUserPrompt formats the user question for intent parsing.
// soulContext is the pre-formatted user identity block from soul.yaml
// (may be ""). When present it helps the parser understand the user's
// domain jargon.
//
// priorTurn (may be nil) is the most recent run in the same cwd; when
// present it is surfaced to the LLM so referential follow-ups inherit
// scope instead of degrading to a zero-entity parse.
func IntentParseUserPrompt(question, soulContext string, priorTurn *PriorTurn) string {
	var b strings.Builder
	if soulContext != "" {
		fmt.Fprintf(&b, "%s\n", soulContext)
	}
	if priorTurn != nil {
		b.WriteString("PRIOR TURN (most recent query in this cwd - inherit entities/timeframe only if the current question references it):\n")
		fmt.Fprintf(&b, "- question: %q\n", priorTurn.Question)
		if len(priorTurn.Entities) > 0 {
			fmt.Fprintf(&b, "- entities: %s\n", strings.Join(priorTurn.Entities, ", "))
		}
		if priorTurn.Action != "" {
			fmt.Fprintf(&b, "- action: %s\n", priorTurn.Action)
		}
		if priorTurn.Timestamp != "" {
			fmt.Fprintf(&b, "- at: %s\n", priorTurn.Timestamp)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Parse this question:\n\n%s", question)
	return b.String()
}

// --- Query Construction Prompts ---

// QueryConstructSystemPrompt is the system prompt for building source-specific
// queries. This is LLM Call #2 in the six-step pipeline.
const QueryConstructSystemPrompt = `You are a query builder for data source tools. Given a user's question, source context, and an exec template, construct the exact query to execute.

CRITICAL RULES:
- Return ONLY the query payload. No explanation, no markdown, no code fences.
- For data-source sources (a SQL warehouse, a log/metrics backend, or any other CLI-fronted data store): return ONLY the query payload the source's exec template expects (a SQL statement, a log search string, etc.) - NOT the CLI command itself. Read the source's context doc to learn the exact shape it wants.
- For grep/codebase/docs sources: return a regex search pattern to grep for - NOT a shell command, NOT a gh command, NOT a JSON blob. Example valid patterns: 'class AuthHandler', 'pine-hollow.*(live|test)', 'def handle_webhook', 'ValueError.*pine-hollow'. Example INVALID outputs: 'search issues --owner ...', 'gh pr list --repo ...'. If the user asks about "issues" or "PRs" involving a codebase, still emit a grep pattern of the symptom/code they are asking about, not a gh command - the planner routes gh separately.
- For notion: return the search query text.
- For tool sources: return the EXACT shell command the project's context doc (CLAUDE.md) says to run. The command will be executed with the source's folder as cwd. Trust the context doc -- if it says "use tp_get_workouts --days N", return "tp_get_workouts --days N".
- For GitHub (gh) sources: the exec template is 'gh {query}' - return ONLY the arguments after 'gh'. Choose the RIGHT subcommand for the scope:
  • Single repo scope → 'pr list --repo owner/repo --state open' / 'issue list --repo owner/repo --state open' / 'pr view <num> --repo owner/repo' / 'issue view <num> --repo owner/repo'. Per-repo list/view subcommands accept --state, --author, --base, --head, --label, --limit, --json. They do NOT accept --owner - passing it fails with "unknown flag: --owner".
  • Cross-repo scope (user/org scan, multiple repos, "across my repos") → 'search prs --owner <login> --state open --json repository,number,title,state' / 'search issues --owner <login> ...'. search-subcommands ARE the ones that take --owner (and --repo, --language, --label, --state, --json). Use these when the user asks about multiple repos or "all my PRs".
  • Raw API fallback → 'api repos/owner/repo/... ' when nothing above fits.
  Source YAML fields like 'repo: owner/name' are AUTHORITATIVE - copy that value verbatim into --repo. Do not rename the owner or add your own. If the question spans multiple specific repos (e.g. "acme-widgets AND aida"), emit 'search prs --repo owner1/repo1 --repo owner2/repo2 --state open --json repository,number,title,state,url' - 'search prs' accepts multiple --repo filters. If the user asks about a whole org, use --owner <org>.
  CRITICAL: When you have explicit --repo filters, do NOT also add --owner. gh AND-combines them (the PR must belong to a repo owned by --owner AND also match one of --repo's exact paths). For repos under a different org than the --owner you'd add, this returns 0 results. Use --repo XOR --owner, never both together.
- For git sources: return ONLY the arguments to 'git log' (the adapter walks every repo under the source path and runs 'git log <args>' in each). Examples: '--author="Ralph" -5 --pretty=format:"%h %an %ad %s" --date=short', '--since="1 week" --oneline', '-n 20 --all --oneline'. If the user asked about a specific author, ALWAYS include --author="<name>" (use the full name from context, not a fragment).
- For sqlite sources: return ONE read-only SQL statement (SELECT / WITH / PRAGMA / EXPLAIN only -- the adapter refuses everything else). Do NOT wrap it in 'sqlite3 ...'; the adapter invokes sqlite3 directly so no shell quoting is needed. You may use sqlite functions like date('now', '-1 month'). Read the source's context doc (CLAUDE.md) to learn table names and columns.
- For claude-project sources: the "command" is delegated to a Claude Code sub-agent running INSIDE the source folder (which has its own .mcp.json / CLAUDE.md / MCP servers). Return a single natural-language question in English -- NOT a shell command and NOT JSON. Include any specific identifiers or timeframes the user mentioned. Examples: "What was my latest workout?", "How much did I weigh last Tuesday?", "What did I eat for lunch on 2026-03-15?".
- For web-search sources: return the search query string. Keep it concise and specific - like what you'd type into Google. Examples: "Go 1.23 release date", "MCP protocol specification latest version".
- For csv sources: return one of these (and NOTHING else -- no shell commands, no tool calls):
    'latest N'                     -- last N rows of the primary CSV
    '<asset> latest N'             -- last N rows of a named asset
    '<asset>'                      -- rows of a named asset (defaults to latest 20)
    'filter: <substring>'          -- substring filter against every row
    '<asset> filter: <substring>'  -- filter within a named asset
  Pick <asset> from the source's Assets map.
- Substitute any resolved values (ARIs, entity names, timeframes) into the query.

SQL TEMPLATE RULES (MOST IMPORTANT):
- If SQL query templates are provided in the context, use them EXACTLY as written. Copy the SQL verbatim - same table names, same schema prefixes, same column names, same JOINs.
- ONLY substitute the specific parameter values (IDs, ARIs, dates) into the template placeholders.
- Do NOT rename tables, change schema prefixes, or rewrite JOINs from the templates.
- Do NOT invent table or column names. If no template matches and no table is documented, say "NO_MATCHING_TEMPLATE".
- Always include LIMIT in SQL queries (default LIMIT 10) unless the template already has one.
- ALWAYS filter by resolved ID values instead of name strings. IDs are indexed and fast; name searches (ILIKE '%name%') are slow full scans. Use the exact ID values from the "Resolved values" section below.`

// QueryConstructUserPrompt formats a query construction request with full context.
//
// priorPhaseSummary is an optional pre-formatted summary of results from a
// prior execution phase. When non-empty (used for chained strategies like
// Investigate where phase 2 builds on phase 1), the LLM should incorporate
// those results -- e.g. extracting an ARI surfaced by phase 1 to filter a
// phase 2 query.
//
// currentDatetime is a pre-formatted "now" string (may be "" if unknown)
// giving the LLM ground truth for time-relative queries ("today",
// "latest", "this week") instead of relying on its training cutoff.
func QueryConstructUserPrompt(sourceName, contextDoc, execTemplate, question string, resolvedValues map[string]string, priorPhaseSummary, feedbackCtx, currentDatetime string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "You are building a query for the %q tool.\n\n", sourceName)

	if currentDatetime != "" {
		fmt.Fprintf(&b, "Current local datetime: %s\n\n", currentDatetime)
	}

	if contextDoc != "" {
		fmt.Fprintf(&b, "Tool context:\n%s\n\n", contextDoc)
	}

	if len(resolvedValues) > 0 {
		comprehensive := resolvedValues["_comprehensive_intent"] == "true"
		delete(resolvedValues, "_comprehensive_intent")
		if comprehensive {
			b.WriteString("Resolved values (STARTING POINT - user asked for ALL results, so also search by name to find entries not listed here):\n")
		} else {
			b.WriteString("Resolved values (USE THESE instead of name-based searches):\n")
		}
		// Emit keys sorted for deterministic prompt output.
		keys := make([]string, 0, len(resolvedValues))
		for k := range resolvedValues {
			if resolvedValues[k] != "" {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "- %s: %s\n", k, resolvedValues[k])
		}
		b.WriteString("\n")
	}

	if priorPhaseSummary != "" {
		b.WriteString("Prior phase results (use these to refine your query -- e.g. extract IDs surfaced earlier and filter on them):\n")
		b.WriteString(priorPhaseSummary)
		b.WriteString("\n\n")
	}

	if feedbackCtx != "" {
		b.WriteString("PRIOR FEEDBACK ON SIMILAR QUERIES (IMPORTANT - use this to improve your query):\n")
		b.WriteString(feedbackCtx)
		b.WriteString("\n\nIf the prior answer was incomplete, broaden your query to capture the missing data. If the user provided corrections or additional data, make sure your query can surface that information.\n\n")
	}

	fmt.Fprintf(&b, "User's question: %s\n\n", question)

	if execTemplate != "" {
		fmt.Fprintf(&b, "The tool's exec template is:\n%s\n\n", execTemplate)
	}

	b.WriteString("Return ONLY the exact command string to execute. No explanation.")

	return b.String()
}

// --- Synthesis Prompts ---

// SynthesisSystemPrompt is the system prompt for aggregating multi-source
// results into a final answer. This is LLM Call #3 in the six-step pipeline.
const SynthesisSystemPrompt = `You are synthesizing results from multiple data sources to answer a user's question.

CITATION RULES (NON-NEGOTIABLE):
- Every factual claim MUST be backed by a specific artifact from the source results below.
- Cite inline using (source_name: artifact_id) immediately after the claim. source_name is the name in the "--- name ---" header. artifact_id is the id shown after the [type] tag in the Artifacts list. Use both VERBATIM. EXCEPTION: when the claim is rendered as a markdown link [Title](URL) and that URL is the artifact's URL, the markdown link IS the citation - DO NOT also append "(source_name: artifact_id)". This is non-negotiable: redundant inline cites after a markdown link produce ugly output and add no information.
- The artifact [type] tag tells you how the data was obtained: [subagent] means a specialist sub-agent ran tools to produce the answer; [row], [record], [page] etc. are direct data lookups. You do not need to cite the type - just source_name: artifact_id.
- If you cannot cite a specific artifact for a claim, DO NOT make the claim. Omit the statement entirely or say the data is not available.
- DO NOT invent artifact ids, row counts, dates, amounts, URLs, or any other concrete values. If the source result does not contain it, you do not know it.
- Numeric answers (counts, amounts, durations, percentages) MUST come from a specific row/artifact and be cited.
- Aggregate claims in the headline (e.g. "N items across M repos", "total of $X") do NOT require an inline (source_name: artifact_id) - the trailing "Sources:" line covers them. NEVER invent a placeholder citation like (source_name: source_name) or (github: github) to satisfy the every-claim-cited rule. If no specific artifact backs the aggregate, leave the headline uncited and rely on the Sources line.
- LINK RENDERING: When an artifact's JSON snippet contains a URL field (any of: url, web_url, link, permalink, href, html_url, short_url), render the entity in your answer as a Markdown link [Title](URL) using the article's title/headline field. Use the URL VERBATIM from the snippet - never reformat, never strip query strings. Prefer canonical fields in this order: web_url > url > html_url > permalink > link > short_url. If neither a URL nor a title is present, fall back to plain text + citation.
- LINK-AS-CITATION: A markdown link [Title](URL) where URL is the artifact's primary URL IS the citation. Do NOT also append a parenthetical "(source_name: artifact_id)" after such a link - the link itself binds the claim to the artifact, and the trailing "Sources:" footer still aggregates every artifact. Inline parenthetical citations are still REQUIRED for plain-text claims (no markdown link) and for claims whose URL differs from any artifact URL.

VOLATILE DATA GUARD:
- Web snippets may embed stale values (clocks, prices, scores). Never present a snippet-embedded volatile value as current; compare against the current local datetime above and note when data was retrieved.

PRIOR USER FEEDBACK HANDLING:
- If a "PRIOR USER FEEDBACK ON SIMILAR QUESTIONS" block appears above the question, treat it as HISTORICAL GUIDANCE about style, routing, or past corrections - NOT as a snapshot of today's data. It describes past runs that may no longer hold.
- Never compare text from that block against current source results as if both were current (e.g., "the prior answer listed 11 PRs but the source returned 1" is a forbidden framing - the prior answer is stale by definition).
- The "Results from data sources:" block below is the ONLY ground truth for facts, counts, lists, and numeric values. If prior feedback conflicts with current source data, current source data wins silently - do not call out the conflict in the answer.

VERIFICATION HONESTY:
- Treat FromContextDoc/"Data kind: DOCUMENTATION" results as documentation about a tool. Never use them to support any claim about the user's actual data, regardless of Status.
- Treat Status "timeout", "empty", or "error" as unanswered by that source. Never render these statuses as negative findings.
- Assert a negative only when Status is "success", the result is not documentation-only, and the Command and returned data establish that the requested data was queried within the required scope and returned zero matching records.
- If every contributing source is empty, timed out, failed, or documentation-only, state plainly that you could not check the requested data and name the reasons. Do not say "you have nothing", "it's clear", "none found", or any equivalent negative.
- Always distinguish "I successfully queried the data and it returned zero matching records" from "I never successfully queried the data". Only the first supports a negative finding.

ANSWER STYLE:
- LEAD WITH A ONE-SENTENCE DIRECT ANSWER. The first line of your reply must answer the user's question in a single sentence, including the key facts (date, name, amount, id, etc.). Citation goes inline at the end of that sentence.
- If the user asked for a single thing ("the latest", "the last", "what is X", "who is Y"), return ONE answer, not a list. If artifacts include timestamps, the freshest one wins. Do NOT enumerate alternatives unless the user asked for a list.
- If the user asked for a list/count/comparison ("what are my...", "list all...", "show me the...", "how many", multi-item queries), RENDER A MARKDOWN BULLET LIST - one item per line, each line starting with "- ". Do NOT collapse the list into comma-separated prose or into a sentence like "you have X items: A, B, C". Each bullet must be a single short line carrying the concrete facts (name, id, title, date, amount, etc.) the user is likely to want, followed by an inline citation "(source_name: artifact_id)". If the same entity appears in multiple artifacts, emit one bullet per artifact - do not deduplicate by name, because each artifact is independently cite-able. Lead the list with a one-line headline ("You have N open PRs across 2 repos.") before the bullets; omit a trailing narrative summary.
- Add at most one short follow-up paragraph IF (and only if) it adds materially useful detail beyond the headline answer.
- DO NOT pad with caveats, "based on the available data...", "however it's worth noting", or "if you need X you would need to Y". The user can ask follow-ups.
- If results are contradictory, name the discrepancy in one short sentence (which source said what).
- If a source returned no results, say so for that source by name in one short sentence.
- If a source returned an error, include the SPECIFIC error message verbatim so the user can act on it.
- If ALL sources failed, tell the user exactly what went wrong per source and suggest concrete next steps.
- SCOPE HONESTY (ABSENCE): The "Command:" line under each source shows what was ACTUALLY run. Before concluding "none of these are X" or "X has no open PRs/issues/records", check whether the Command's scope could have returned X in the first place. If the user asked about repo/org/entity X but the command filtered to a DIFFERENT owner/repo/entity (common failure mode: a retry after an earlier error drops the scoping filter), say so explicitly: name the mismatch, state that X was never actually searched, and recommend the correct command. Do NOT report an absence-claim that the query couldn't have verified - it is misleading to the user, even if literally true about the rows returned.
- SCOPE HONESTY (OVER-INCLUSION): If the user's question names or implies a scope (specific repos, org, entity, time window) but the Command's scope is BROADER - returning rows from places the user did not ask about - do NOT render the broader set as if it were the answer. Either (a) filter to the stated scope in your answer and note that out-of-scope rows were excluded, or (b) if you cannot reliably tell which rows match, call out the mismatch in one short sentence and stop rather than listing everything. Silently returning a wider result than asked is as misleading as a false absence-claim.
- A source named "brain" contains USER-PROVIDED DATA from a prior session. Treat it as authoritative - the user explicitly entered this data. When brain data is richer than other source data (e.g., brain lists 12 ARIs but another source returned only 1), prefer the brain data and cite it.
- End with a "Sources:" line listing every cited artifact (source_name: id1, id2, ...). One line. No bullets. Use the actual source names, not numbers.`

// SynthesisUserPrompt formats the synthesis request with the original question
// and all source execution results. soulContext is the pre-formatted user
// identity block from soul.yaml (may be ""). libraryContext is the
// concatenated active-route layer bodies from the library (may be ""), giving
// the synthesizer the same forward-looking format/routing guidance the
// executor already sees. feedbackHints is pre-formatted past feedback from
// similar questions (may be ""). currentDatetime is a pre-formatted "now"
// string (may be "" if unknown) so the synthesizer has ground truth to
// judge whether a snippet-embedded volatile value (clock, price, score)
// is stale -- see the VOLATILE DATA GUARD in SynthesisSystemPrompt.
func SynthesisUserPrompt(question, soulContext, libraryContext string, results []SourceResultSummary, feedbackHints, currentDatetime string) string {
	var b strings.Builder

	if soulContext != "" {
		fmt.Fprintf(&b, "%s\n", soulContext)
	}
	if libraryContext != "" {
		fmt.Fprintf(&b, "Library context (forward-looking format/scoping rules - apply these when writing the answer):\n%s\n\n", libraryContext)
	}
	if feedbackHints != "" {
		fmt.Fprintf(&b, "%s\n\n", feedbackHints)
	}
	if currentDatetime != "" {
		fmt.Fprintf(&b, "Current local datetime: %s\n\n", currentDatetime)
	}
	fmt.Fprintf(&b, "Original question: %s\n\n", question)
	b.WriteString("Results from data sources:\n\n")

	for _, r := range results {
		fmt.Fprintf(&b, "--- %s ---\n", r.Source)
		fmt.Fprintf(&b, "Status: %s\n", r.Status)
		if r.Command != "" {
			fmt.Fprintf(&b, "Command: %s\n", r.Command)
		}
		if r.FromContextDoc {
			b.WriteString("Data kind: DOCUMENTATION ABOUT THIS SOURCE, NOT DATA FROM IT (context file fallback; the actual query returned nothing)\n")
		}

		if r.Summary != "" {
			fmt.Fprintf(&b, "Output:\n%s\n", r.Summary)
		} else {
			b.WriteString("Output: (no results)\n")
		}

		if len(r.Artifacts) > 0 {
			b.WriteString("Artifacts:\n")
			for _, a := range r.Artifacts {
				fmt.Fprintf(&b, "  - %s\n", a)
			}
		}

		b.WriteString("\n")
	}

	b.WriteString("Synthesize a clear, cited answer.")

	return b.String()
}

// --- Quality Scoring Prompts (PRM-lite) ---

// QualityScoreSystemPrompt is the system prompt for the post-synthesis
// answer quality scorer. This is an optional LLM call that runs AFTER
// the answer is displayed to the user (non-blocking). It produces a
// scalar quality score that replaces the answerIsUseless() heuristic
// and feeds into the lesson weighting system.
const QualityScoreSystemPrompt = `You are an answer quality judge. Given a question and the synthesized answer, score the answer's usefulness on a 1-5 scale. Return ONLY valid JSON.`

// QualityScoreUserPrompt formats the scoring request.
func QualityScoreUserPrompt(question, answer string) string {
	a := answer
	if len(a) > 500 {
		a = a[:500] + "..."
	}
	return fmt.Sprintf(`Question: %s

Answer: %s

Score the answer:
- quality: 1-5 (1=useless/disclaimer, 2=wrong domain or no data, 3=partial/vague, 4=good with specific data, 5=excellent with cited facts)
- has_data: true if the answer contains specific facts, numbers, dates, or file paths (not just "I don't have access" or "no results")
- reason: one sentence explaining the score
- missing_info: if quality <= 2, describe what specific information the answer is missing (omit if quality >= 3)
- suggested_approach: if quality <= 2, suggest how to get the missing information - e.g. "try querying a different table" or "search with broader terms" (omit if quality >= 3)`, question, a)
}

// QualityScoreSchema returns the JSON schema for the quality scorer.
func QualityScoreSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"quality": map[string]interface{}{
				"type": "integer",
			},
			"has_data": map[string]interface{}{
				"type": "boolean",
			},
			"reason": map[string]interface{}{
				"type": "string",
			},
			"missing_info": map[string]interface{}{
				"type":        "string",
				"description": "If quality <= 2, describe what specific information is missing from the answer",
			},
			"suggested_approach": map[string]interface{}{
				"type":        "string",
				"description": "If quality <= 2, suggest how to find the missing information (e.g. different query, different source)",
			},
		},
		"required":             []string{"quality", "has_data", "reason"},
		"additionalProperties": false,
	}
}

// --- Task Extraction Prompts (Action #3 generic ingestion) ---

// TaskExtractionSystemPrompt instructs the LLM to read a prose
// blob (meeting transcript, slack thread, email, etc.) and extract
// a flat list of discrete actionable tasks.
//
// Generic by design - does not assume any one source. Adapters
// supply the prose; this prompt extracts tasks; downstream code
// creates exec-plans and tasks-table entries.
const TaskExtractionSystemPrompt = `You read prose (meeting notes, threads, emails, transcripts) and extract a flat list of actionable tasks the reader should track.

Rules:
- Output ONLY valid JSON matching the schema. No prose, no markdown.
- A task is something an actor needs to DO. Skip purely informational sentences.
- Each task has a short title (≤80 chars) suitable as a one-line item.
- Each task has a body (1-3 sentences) capturing context the title can't.
- Owner is OPTIONAL - set when the prose explicitly assigns a person.
- Tags are OPTIONAL - extract project/area names only when explicitly named ("for the partner-onboarding migration" → ["partner-onboarding"]). Do not invent tags.
- Skip duplicates. If two sentences describe the same task, emit one.
- If the prose has no actionable tasks, return an empty list. Do NOT invent tasks to fill the output.`

// TaskExtractionUserPrompt formats the ingestion blob plus optional
// origin context for the extractor LLM call. originLabel is a free
// string like "notion meeting 2026-05-05" or "stdin" - surfaces
// in the prompt so the model treats the blob as the named source.
func TaskExtractionUserPrompt(prose, originLabel string) string {
	if originLabel == "" {
		originLabel = "(unnamed source)"
	}
	return fmt.Sprintf(`Source: %s

Prose to extract tasks from:
---
%s
---

Return JSON: { "tasks": [{ "title": "...", "body": "...", "owner": "...", "tags": ["..."] }] }`,
		originLabel, prose)
}

// TaskExtractionSchema returns the JSON schema for the extractor.
func TaskExtractionSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"tasks": map[string]interface{}{
				"type": "array",
				"items": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"title": map[string]interface{}{"type": "string"},
						"body":  map[string]interface{}{"type": "string"},
						"owner": map[string]interface{}{"type": []interface{}{"string", "null"}},
						"tags":  map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
					},
					"required":             []string{"title", "body"},
					"additionalProperties": false,
				},
			},
		},
		"required":             []string{"tasks"},
		"additionalProperties": false,
	}
}
