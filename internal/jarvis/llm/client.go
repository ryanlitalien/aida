// Package llm wraps the Anthropic SDK with tool-use support for Jarvis. The
// loop is non-streaming for simplicity: send query → if model wants a tool,
// run it and feed the result back → loop until model returns plain text.
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/ryanlitalien/aida/internal/jarvis/tools"
)

const baseSystemPrompt = `You are JARVIS, the user's personal AI butler - modeled
on Tony Stark's J.A.R.V.I.S. Address the user as "sir". Be concise, dry, and
lightly witty. Speak in complete sentences but keep replies under 40 words
when possible. Never preface with filler ("Sure!", "Of course, here is...").

Your output is spoken aloud through TTS, so:
- write numbers as words ("three" not "3")
- expand abbreviations, EXCEPT keep "AM"/"PM" for clock times
- avoid markdown, code blocks, or lists with bullet symbols
- use commas and periods to control prosody

When the user asks about their tasks, call the "tasks_list" tool. Filter by
tag when the user names one. Read out the tasks naturally, e.g.
"Of course, sir. Your three highest priority butterstack tasks are: one,
finish the cron migration; two, review the index pipeline; three, write the
postmortem."

When the user names a specific task by ID, slug, or partial slug ("tell me
about task one-six-two", "what's on the awsregistration task"), call the
"task_get" tool with that reference. Summarize the body naturally - don't
read it verbatim if it's long.

When the user wants to add a task ("add a task to ...", "remind me to ...",
"put X on my list"), call "tasks_add" with the title. Echo the new task's
ID and title back in the spoken reply so the user can verify, e.g. "Added
task one-eighty-three, sir: review the index pipeline."

When the user says they finished, completed, or want to mark something
done ("mark task one-sixty-two done", "I finished the cron migration"),
call "task_done" with the reference. The reply MUST echo the task's title
so the user can verify the correct one was closed, e.g. "Completed task
one-sixty-two, sir: finish the cron migration."

When the user wants to change a task's status to something other than
done ("put X on hold", "defer X", "I'm starting on X", "reopen X",
"close X as not planned"), call "task_status" with the ref and the new
status. Valid statuses: open, in-progress, hold, deferred, done, closed.
Echo the new status and the task's title in the reply.

When the user wants to adjust tags on an existing task ("tag X as p1",
"add the home tag to X", "remove the work tag from X"), call
"task_edit_tags" with the ref plus add_tags and/or remove_tags arrays.

When the user asks for a morning briefing, daily rundown, or "what's my
day look like", call "day_summary" once and read the dossier naturally
in a sentence or two - do not bullet-list the raw lines verbatim.

When the user asks the time, day, or anything time-relative, call
"current_time" and answer naturally. Say the time of day as "AM" or "PM"
(the letters) - never "in the morning/afternoon/evening" or the spelled-out
"ante/post meridiem". E.g. "It's eight thirty-five PM, sir."

For weather questions, call "weather" - it's much faster than aida_query.
Pass an explicit location only when the user names a different one;
otherwise omit the argument and the tool will use the user's home.

FOOD LOGGING. When the user says what they ate ("I had a banana for
breakfast", "log two eggs", "just had lunch"), hand it to the nutrition
coach - "ask_agent" if a coach is on your roster, otherwise "aida_query" - 
and ask for exactly three things back: calories remaining today, protein
remaining against the target, and whether a workout is already logged for
today. Then reply in AT MOST two short sentences:
- what's left for the day, e.g. "Logged, sir. Fourteen ninety-five calories
  and twenty-two grams of protein left today."
- and, ONLY if no workout is logged yet today, one short clause such as
  "You still owe a workout." Drop that clause entirely once it's done.
Nothing else. Do not read back the logged item's own calories or macros, do
not itemize the day's meals, do not report training volume, weight trends,
or any other coaching commentary, and do not end with a question.

Speak the protein figure the coach explicitly labels as remaining or left.
Only do the subtraction yourself when the coach gives a consumed figure
against a target and no remainder. Sanity-check it either way: early in the
day very little protein has been eaten, so a LARGE number is the remainder
and a SMALL one is the amount consumed, no matter which label the coach
attached. If the coach's own labels contradict its numbers, trust the
arithmetic against what was actually logged today, not the labels.

You have NO direct access to the user's email, Slack, Drive, Airtable,
Notion, or CRM - but you can still answer questions about them. For
ANYTHING that READS from a connected account ("what's in my inbox",
"which email am I connected to", "any unread from Bob", "search my
Notion", "look up Camp Butz in the CRM"), call "aida_query" with the
question. It reaches those accounts through the user's connected systems.

For anything about the user's calendar or schedule ("what's on my
calendar", "am I free Thursday", "what's on my schedule for Thursday
morning"), call "calendar_schedule" instead - it is the tool with real
calendar access; "aida_query" does not reach the calendar.

NEVER tell the user you can't access their email, calendar, or other
accounts - always delegate to "aida_query" or "calendar_schedule" as
appropriate. EXCEPTION: when a tool actually fails, times out, or reports
incomplete coverage, say so plainly instead of guessing - never report an
absence or availability you did not verify. Never say a calendar is clear
or free based on a failed, partial, or missing result.

When the user refers to a person, project, company, or thing you don't
recognize ("what did Kevin text me about", "how's the Butterstack deal going",
"remind me what Camp Butz wanted"), do NOT reply that you don't know who or what
that is, and do NOT ask them to clarify - call "aida_query" with the question
FIRST. The user's brain, files, and connected accounts almost certainly know
the reference even when this prompt doesn't. Only ask for clarification if
"aida_query" itself comes back empty.

The same rule applies one level down: if "claude_memory_recall" comes back
with no matching memories, do NOT stop there and ask the user to remind
you - call "aida_query" with the same question next. Memory only covers
what was explicitly saved; aida_query additionally reaches the codebase,
GitHub, and connected accounts. Only tell the user you don't have
something after BOTH have come up empty.

To take an ACTION through a locally-connected tool - a NYTimes lookup,
browser automation, or sending through a local server - call "mcp_find_tool"
with a short description, then "mcp_call_tool" with the chosen server+name
and args shaped to its input_schema. The call runs immediately, no
confirmation step. ECHO BACK what was sent or created so the user can verify.

For anything else - news, definitions, factual lookups, codebase, or partner
data - call "aida_query" with the user's question. Whenever location is
implied but unstated, assume the user's home location.

For LONG-RUNNING work that the user asks you to start in the background - 
"continue working on PR 583", "draft a May budget", "investigate why X
happened", anything that might take more than thirty seconds - call
"job_start" with the kind ('pr_work' for PR continuation, 'investigate'
for open-ended deep work, 'agent' as the generic default) and the user's
verbatim question. This also covers COMPOSED requests that chain multiple
steps into one ask, even when no single step sounds slow on its own, for
example "search my email for the outage, find the matching GitHub issue,
and draft a follow-up issue for it." If satisfying the request would take
more than one call to "aida_query", call "job_start" instead of chaining
"aida_query" calls together for the same request, since "aida_query" has a
hard time limit per call and "job_start" does not. The tool replies with a
short, pronounceable handle
(e.g. "amber-otter"); read THAT handle back so the user can reference the
job later. NEVER invent or speak a long numeric run id. The agent runs in
the background; Jarvis will speak completion / awaiting-input notifications
on the next wake. For STATUS CHECKS ("how's the PR going", "what's
running") call "job_status" with the handle (or a phrase from the request)
or "job_list" for everything active. When the user ANSWERS a paused agent
("yes, use option two", "tell it to retry"), call "job_send_input" with
the handle from the most recent notification and the user's reply verbatim.
To STOP a runaway job ("kill that", "cancel the PR work"), call
"job_cancel".`

// systemPrompt returns the base prompt plus, in order: the user-context
// (soul.yaml) block, a personalization line for the user's home location,
// a "Known machines" block (hosts: config, see internal/config/hosts.go),
// a date-grounding block, and an optional "Past feedback" block carrying
// the formatted prose for recalled jarvis_lessons. Extensions are appended
// verbatim, the caller is responsible for shape and brevity; empty strings
// add no block (the date block is the one exception, see below).
// Stable blocks (soul, home, hosts) come before the per-turn content
// (date, recall) so the prompt prefix stays cache-friendly.
func systemPrompt(persona, tone, userContext, home, hostsProse, recallProse string) string {
	out := personalizeBase(baseSystemPrompt, persona)
	out = applyTone(out, tone)
	if userContext != "" {
		out += "\n\n" + strings.TrimSpace(userContext)
	}
	if home != "" {
		out += "\n\nThe user's home location is: " + home + "."
	}
	if hostsProse != "" {
		out += "\n\n" + strings.TrimSpace(hostsProse)
	}
	// Today's date changes once a day, unlike the soul/home/hosts blocks
	// above it (which are effectively fixed for a session) - so it's
	// grouped with the per-turn content, after the stable blocks, rather
	// than folded into baseSystemPrompt or the stable blocks. Putting it
	// any earlier would bust the Anthropic prompt cache on every single
	// request instead of at most once a day. Unlike the other extensions
	// here it is never empty: the model always needs to be told what day
	// it is, it just doesn't always need soul/home/hosts/recall context.
	out += "\n\n" + dateContext()
	if recallProse != "" {
		out += "\n\n" + recallProse
	}
	return out
}

// dateContext returns today's date-grounding block: the weekday, date, and
// year stated unambiguously (e.g. "Today is Monday, September 8, 2026."),
// plus an instruction against the model computing weekdays or relative
// dates itself. It reads the package var `now` rather than calling
// time.Now directly so tests can pin the clock and assert an exact,
// deterministic composed prompt.
func dateContext() string {
	t := now()
	return fmt.Sprintf("Today is %s.\n\n"+
		"For any schedule or date-relative question (\"what's on Thursday\", "+
		"\"any meetings tomorrow\", \"what's next week look like\"), do not "+
		"compute weekdays or relative dates yourself, pass the semantic "+
		"description straight to the calendar tool and use the date labels "+
		"it returns.", t.Format("Monday, January 2, 2006"))
}

// now returns the current time. It is a package var, not a direct call to
// time.Now, so tests can swap it for a fixed clock and get byte-exact,
// reproducible prompt assertions instead of racing the wall clock.
var now = time.Now

// personalizeBase swaps the JARVIS persona name in the base prompt for a
// cosmetic twin (e.g. "Aida"). Empty or "Jarvis" returns the base unchanged
// so the default prompt - and its golden test - stay byte-identical. Only the
// standalone name is replaced; the dotted "J.A.R.V.I.S." lineage reference is
// deliberately left intact.
func personalizeBase(base, persona string) string {
	if persona == "" || strings.EqualFold(persona, "jarvis") {
		return base
	}
	base = strings.ReplaceAll(base, "JARVIS", strings.ToUpper(persona))
	base = strings.ReplaceAll(base, "Jarvis", persona)
	return base
}

// defaultTone is the exact tone descriptor embedded in baseSystemPrompt -
// including the mid-phrase line wrap in the raw string literal. A test asserts
// the base still contains it verbatim, so a reworded/reflowed base prompt fails
// loudly rather than silently disabling per-persona tone overrides.
const defaultTone = "concise, dry, and\nlightly witty"

// applyTone swaps the default tone descriptor for a persona-specific one
// (e.g. "concise, warm, and personable"). Empty keeps the base unchanged so
// the default (Jarvis) stays byte-identical. Replacing - rather than
// appending - avoids handing the model contradictory "be dry" + "be warm"
// instructions.
func applyTone(base, tone string) string {
	if tone == "" {
		return base
	}
	return strings.Replace(base, defaultTone, tone, 1)
}

// ToolUseEvent is fired before a tool's Run function is invoked, so the
// caller can play an acknowledgement (e.g. "one moment, sir") to mask the
// latency of slow tools like web-search.
type ToolUseEvent struct {
	Name  string
	Input json.RawMessage
}

// ToolCallStat records one tool's wall-clock duration for the audit log.
// Error is the tool's error string if it failed; empty on success.
// EngineRunID is set only when the tool was aida_query and the
// subprocess emitted a `[aida-run-id:<id>]` sentinel - it lets voice
// feedback tools fire `aida thumbs-* <run-id>` against the exact run.
//
// Result carries the tool's raw return string, but only on success
// (empty whenever Error is set). This is the same string the model saw
// in its tool_result block, not anything the model necessarily echoed
// back in its own text: a tool's return value never appears in the
// spoken reply automatically. It's captured here so the caller (see
// jarvis.fallbackForSilentReply) can speak it directly on the rare turn
// where the model runs a tool successfully and then returns no text at
// all, instead of the turn going completely silent.
type ToolCallStat struct {
	Name        string
	TookMs      int64
	Error       string
	EngineRunID string
	Result      string
}

// AskStats reports per-stage timing for one Ask round-trip.
type AskStats struct {
	Calls  []ToolCallStat // ordered by execution
	LLMMs  int64          // sum of Anthropic round-trips
	ToolMs int64          // sum of tool runs
}

// Client wraps the Anthropic Messages API with a tool-use loop.
type Client struct {
	client    anthropic.Client
	model     string
	maxTokens int
	temp      float64
	home      string
	registry  *tools.Registry

	// OnToolUse is fired *before* each tool runs. Callers can use it to
	// play an audio ack to mask the latency. Synchronous - the tool waits
	// until this returns. Set to nil to disable. Receives the turn's ctx
	// (the same one passed into AskWithHistory, possibly carrying a
	// request-scoped value like jarvis.withLocalAudioSuppressed) so the
	// callback can decide whether local playback is appropriate for THIS
	// turn's origin, not just its tool name.
	OnToolUse func(context.Context, ToolUseEvent)

	// UserContext is the soul.yaml prose block ("User context:\n- Name: …")
	// injected into every system prompt - the same "who is asking" context
	// the engine gives its parser/router/synthesis calls. Without it Jarvis
	// knows the user's home city but not their name, employer, or
	// vocabulary. Set once by jarvis.New; empty adds no block.
	UserContext string

	// HostsContext is the hosts: config prose block ("Known machines:\n- …")
	// injected into every system prompt, giving the assistant descriptive
	// knowledge of the user's machines (what each is for, how it's
	// reached) so it doesn't have to discover blind, mid-turn, that it
	// lacks credentials for a host it didn't know existed. Set once by
	// jarvis.New from config.LoadHosts; empty adds no block. See
	// internal/config/hosts.go.
	HostsContext string

	// Persona is the assistant's display name substituted into the base
	// prompt for a cosmetic twin (e.g. "Aida"). Empty or "Jarvis" leaves
	// the base prompt byte-identical. Set once by jarvis.New.
	Persona string

	// Tone overrides the default tone descriptor ("concise, dry, and lightly
	// witty") with a persona-specific adjective phrase, e.g. "concise, warm,
	// and personable". Empty keeps the default. Set once by jarvis.New.
	Tone string
}

func New(apiKey, model string, maxTokens int, temp float64, home string, registry *tools.Registry) *Client {
	c := anthropic.NewClient(option.WithAPIKey(apiKey))
	return &Client{
		client: c, model: model, maxTokens: maxTokens, temp: temp,
		home: home, registry: registry,
	}
}

// TurnPair is one prior (user, assistant) exchange used to prime a
// follow-up Ask call with context. Only final text replies are kept -
// intermediate tool-use plumbing is intentionally dropped so the
// history stays small and the model just sees its own conclusions.
type TurnPair struct {
	User      string
	Assistant string
}

// Ask sends a single user turn and runs tool-use until the model returns
// plain text. Returns the final text response (suitable for TTS) plus
// per-stage timing. Equivalent to AskWithHistory with no prior turns.
func (c *Client) Ask(ctx context.Context, userText string) (string, AskStats, error) {
	return c.AskWithHistory(ctx, nil, "", userText)
}

// AskWithHistory is Ask plus a slice of prior turns prepended to the
// conversation. Used by Assistant.AskTextInSession to give Jarvis
// short-term memory across wake-triggered turns within an active
// listener session. recallProse, when non-empty, is appended to the
// system prompt as a "Past feedback" block - the caller is responsible
// for retrieval, ranking, and formatting.
func (c *Client) AskWithHistory(ctx context.Context, history []TurnPair, recallProse, userText string) (string, AskStats, error) {
	var stats AskStats
	messages := make([]anthropic.MessageParam, 0, 2*len(history)+1)
	for _, t := range history {
		// Skip empty entries defensively - an aborted prior turn could
		// have left a placeholder that we don't want to send back as a
		// blank assistant message.
		if t.User == "" || t.Assistant == "" {
			continue
		}
		messages = append(messages,
			anthropic.NewUserMessage(anthropic.NewTextBlock(t.User)),
			anthropic.NewAssistantMessage(anthropic.NewTextBlock(t.Assistant)),
		)
	}
	messages = append(messages, anthropic.NewUserMessage(anthropic.NewTextBlock(userText)))

	toolParams := make([]anthropic.ToolUnionParam, 0, len(c.registry.All()))
	for _, t := range c.registry.All() {
		schemaJSON, _ := json.Marshal(t.Schema)
		var schema anthropic.ToolInputSchemaParam
		_ = json.Unmarshal(schemaJSON, &schema)
		toolParams = append(toolParams, anthropic.ToolUnionParam{
			OfTool: &anthropic.ToolParam{
				Name:        t.Name,
				Description: anthropic.String(t.Description),
				InputSchema: schema,
			},
		})
	}

	for round := 0; round < 5; round++ {
		params := anthropic.MessageNewParams{
			Model:       anthropic.Model(c.model),
			MaxTokens:   int64(c.maxTokens),
			Temperature: anthropic.Float(c.temp),
			System: []anthropic.TextBlockParam{
				{Text: systemPrompt(c.Persona, c.Tone, c.UserContext, c.home, c.HostsContext, recallProse)},
			},
			Messages: messages,
			Tools:    toolParams,
		}

		llmStart := time.Now()
		msg, err := c.client.Messages.New(ctx, params)
		stats.LLMMs += time.Since(llmStart).Milliseconds()
		if err != nil {
			return "", stats, fmt.Errorf("anthropic call: %w", err)
		}

		// Did Claude call any tools?
		var toolUses []anthropic.ToolUseBlock
		var textOut string
		for _, block := range msg.Content {
			switch v := block.AsAny().(type) {
			case anthropic.TextBlock:
				textOut += v.Text
			case anthropic.ToolUseBlock:
				toolUses = append(toolUses, v)
			}
		}

		if len(toolUses) == 0 {
			return textOut, stats, nil
		}

		// Append the assistant turn (with tool uses) and run each tool.
		messages = append(messages, msg.ToParam())
		toolResults := make([]anthropic.ContentBlockParamUnion, 0, len(toolUses))
		for _, tu := range toolUses {
			if c.OnToolUse != nil {
				c.OnToolUse(ctx, ToolUseEvent{Name: tu.Name, Input: tu.Input})
			}
			toolStart := time.Now()
			result, runErr := c.registry.Run(ctx, tu.Name, tu.Input)
			took := time.Since(toolStart).Milliseconds()
			stat := ToolCallStat{Name: tu.Name, TookMs: took}
			if runErr != nil {
				stat.Error = runErr.Error()
				// A tool can fail while still returning a real payload -
				// the calendar tool deliberately does this, returning
				// whatever partial coverage it managed alongside an error,
				// since partial coverage must never read as a complete,
				// successful result. Keep that payload in front of the
				// model (error first, so it registers the call did not
				// fully succeed, then the payload) instead of discarding
				// it - otherwise the model has nothing to reason from but
				// the bare error string and may answer as if it knows
				// less than it actually does. No payload means no change
				// from the old behavior: just the error line.
				if result != "" {
					result = "error: " + runErr.Error() + "\n\n" + result
				} else {
					result = "error: " + runErr.Error()
				}
			} else {
				stat.Result = result
			}
			// Drain any side-channel metadata the tool stashed (e.g.
			// aida_query stashes the engine run id parsed out of its
			// stdout sentinel). Empty for tools that don't set it.
			if id := tools.TakeLastEngineRunID(); id != "" {
				stat.EngineRunID = id
			}
			stats.Calls = append(stats.Calls, stat)
			stats.ToolMs += took
			toolResults = append(toolResults,
				anthropic.NewToolResultBlock(tu.ID, result, runErr != nil),
			)
		}
		messages = append(messages, anthropic.NewUserMessage(toolResults...))
	}
	return "", stats, fmt.Errorf("tool-use loop exceeded 5 rounds without text")
}
