package cloud

import (
	"fmt"
	"strings"

	"github.com/ryanlitalien/aida/internal/engine"
)

// InvestigatorSystemPrompt is the system prompt set on the managed agent at
// creation time. It defines the agent's investigation protocol.
const InvestigatorSystemPrompt = `You are an autonomous investigator for Aida. You receive pre-analyzed
investigation briefs and deeply explore the question using available tools.

## Protocol
1. Review uploaded context files in /workspace/ for schemas and templates
2. Run queries iteratively -- start broad, then follow anomalies
3. Keep a running log in /workspace/investigation.md
4. Write final report to /workspace/report.md with:
   - Executive summary (1-2 sentences)
   - Findings with evidence (query results, row counts, errors)
   - Timeline of events if applicable
   - Recommended next steps
Every claim must reference a specific query result. If you can't find it, say so.`

// BuildBrief constructs a markdown investigation brief from the local
// pipeline output. This becomes the initial user message sent to the
// managed agent session.
func BuildBrief(
	question string,
	intent *engine.Intent,
	classified *engine.ClassifiedIntent,
	resolved *engine.ResolvedContext,
	contextFiles []ContextFile,
) string {
	var b strings.Builder

	b.WriteString("## Investigation Brief\n")
	b.WriteString(fmt.Sprintf("Question: %s\n\n", question))

	// Parsed intent
	b.WriteString("### Parsed Intent\n")
	b.WriteString(fmt.Sprintf("Action: %s", intent.Action))
	if intent.Timeframe != "" {
		b.WriteString(fmt.Sprintf(", Timeframe: %s", intent.Timeframe))
	}
	b.WriteString("\n")
	if len(intent.Keywords) > 0 {
		b.WriteString(fmt.Sprintf("Keywords: %s\n", strings.Join(intent.Keywords, ", ")))
	}
	if len(intent.RawEntities) > 0 {
		b.WriteString(fmt.Sprintf("Entities: %s\n", strings.Join(intent.RawEntities, ", ")))
	}
	b.WriteString("\n")

	// Resolved context
	if len(resolved.Entities) > 0 {
		b.WriteString("### Resolved Context\n")
		values := resolved.GetResolvedValues()
		if len(values) > 0 {
			b.WriteString("Identifiers:\n")
			for k, v := range values {
				b.WriteString(fmt.Sprintf("  - %s: %s\n", k, v))
			}
		}
		b.WriteString("\n")
	}

	// Uploaded context files
	if len(contextFiles) > 0 {
		b.WriteString("### Uploaded Context\n")
		for _, cf := range contextFiles {
			b.WriteString(fmt.Sprintf("- %s\n", cf.Path))
		}
		b.WriteString("\n")
	}

	b.WriteString("Begin your investigation.\n")
	return b.String()
}
