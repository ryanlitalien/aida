package dailybriefing

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/config"
)

// EmailRow is one message worth of triage data, hydrated via users.messages.get
// (format=metadata). Snippet and headers are normalized strings; LabelIDs holds
// the raw Gmail label IDs so callers can categorize via the label map.
type EmailRow struct {
	ID       string
	ThreadID string
	From     string
	Subject  string
	Snippet  string
	Date     time.Time
	LabelIDs []string
	// PartnerTag, when set, is the label name with the partner prefix stripped
	// (e.g. "Partners/Google" -> "Google"). Populated for partner rows only.
	PartnerTag string
}

// TriageResult is what RunTriage returns: three buckets that mirror the
// legacy three-query layout (flagged labels, stale unread primary, partner unread).
type TriageResult struct {
	// Flagged is keyed by user-visible label name (e.g. "TODO", "FollowUp").
	Flagged map[string][]EmailRow
	// Stale is unread primary-tab messages older than 1d, newer than 7d.
	// Google Docs comment-noise rows are collapsed in-place to one summary row per subject.
	Stale []EmailRow
	// Partners are unread messages within 7d that carry any Partners/<Name> label.
	Partners []EmailRow
}

// RunTriage executes the three gmail queries and returns categorized rows.
// It looks up label name<->ID maps once, then queries+hydrates in sequence.
// On any subcommand failure, returns a partial result if possible plus the error.
func RunTriage(ctx context.Context, dc *config.DailyConfig) (*TriageResult, error) {
	nameByID, idByName, err := gmailLabelMaps(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading gmail labels: %w", err)
	}

	res := &TriageResult{Flagged: map[string][]EmailRow{}}

	// Query 1: flagged labels (TODO/FollowUp). One message per thread (most recent).
	if len(dc.GmailLabels) > 0 {
		q := buildLabelOrQuery(dc.GmailLabels)
		rows, err := triageQuery(ctx, q, 30)
		if err != nil {
			return res, fmt.Errorf("flagged-label query: %w", err)
		}
		rows = dedupeByThread(rows)
		for _, r := range rows {
			label := primaryLabelName(r.LabelIDs, dc.GmailLabels, idByName)
			if label == "" {
				continue
			}
			res.Flagged[label] = append(res.Flagged[label], r)
		}
	}

	// Query 2: stale unread primary tab (1-7 days old). Includes Google Docs collapsing.
	stale, err := triageQuery(ctx,
		"is:unread in:inbox category:primary older_than:1d newer_than:7d "+dc.GmailExclude,
		30,
	)
	if err != nil {
		return res, fmt.Errorf("stale-unread query: %w", err)
	}
	res.Stale = collapseGoogleDocs(stale)

	// Query 3: unread partner-labeled in last 7 days.
	if len(dc.GmailPartners) > 0 && dc.GmailPartnerLabelPrefix != "" {
		q := buildPartnerQuery(dc.GmailPartnerLabelPrefix, dc.GmailPartners)
		rows, err := triageQuery(ctx, q, 20)
		if err != nil {
			return res, fmt.Errorf("partner query: %w", err)
		}
		for i, r := range rows {
			rows[i].PartnerTag = partnerTagFromLabels(r.LabelIDs, dc.GmailPartnerLabelPrefix, nameByID)
		}
		res.Partners = rows
	}

	return res, nil
}

// triageQuery runs `users.messages.list` for the given Gmail search query, then
// hydrates each result via `users.messages.get(format=metadata)`. Returns rows
// sorted newest-first by Date.
func triageQuery(ctx context.Context, query string, maxResults int) ([]EmailRow, error) {
	listParams, _ := json.Marshal(map[string]any{
		"userId":     "me",
		"q":          query,
		"maxResults": maxResults,
	})
	out, err := runGWS(ctx, gwsMessagesListDeadline, "gmail.messages.list",
		"gmail", "users", "messages", "list", "--params", string(listParams))
	if err != nil {
		return nil, err
	}
	body, perr := firstJSONObject(out)
	if perr != nil {
		return nil, perr
	}
	var lst struct {
		Messages []struct {
			ID       string `json:"id"`
			ThreadID string `json:"threadId"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &lst); err != nil {
		return nil, fmt.Errorf("decode messages.list: %w", err)
	}

	rows := make([]EmailRow, 0, len(lst.Messages))
	for _, m := range lst.Messages {
		row, err := hydrateMessage(ctx, m.ID)
		if err != nil {
			// Skip messages that 404 or fail to hydrate - one missing row shouldn't kill the briefing.
			continue
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Date.After(rows[j].Date) })
	return rows, nil
}

// hydrateMessage fetches headers + snippet + labelIds for one message.
func hydrateMessage(ctx context.Context, id string) (EmailRow, error) {
	params, _ := json.Marshal(map[string]any{
		"userId":          "me",
		"id":              id,
		"format":          "metadata",
		"metadataHeaders": []string{"From", "Subject", "Date"},
	})
	out, err := runGWS(ctx, gwsMessageGetDeadline, "gmail.messages.get",
		"gmail", "users", "messages", "get", "--params", string(params))
	if err != nil {
		return EmailRow{}, err
	}
	body, perr := firstJSONObject(out)
	if perr != nil {
		return EmailRow{}, perr
	}
	var msg struct {
		ID           string   `json:"id"`
		ThreadID     string   `json:"threadId"`
		LabelIDs     []string `json:"labelIds"`
		InternalDate string   `json:"internalDate"`
		Snippet      string   `json:"snippet"`
		Payload      struct {
			Headers []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"headers"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		return EmailRow{}, err
	}
	row := EmailRow{
		ID:       msg.ID,
		ThreadID: msg.ThreadID,
		Snippet:  decodeSnippet(msg.Snippet),
		LabelIDs: msg.LabelIDs,
	}
	for _, h := range msg.Payload.Headers {
		switch h.Name {
		case "From":
			row.From = h.Value
		case "Subject":
			row.Subject = h.Value
		}
	}
	if ms, err := strconv.ParseInt(msg.InternalDate, 10, 64); err == nil {
		row.Date = time.Unix(ms/1000, 0)
	}
	return row, nil
}

// decodeSnippet replaces the HTML entities Gmail returns in snippet bodies with
// their plain-text equivalents so the briefing reads naturally.
func decodeSnippet(s string) string {
	r := strings.NewReplacer(
		"&#39;", "'",
		"&quot;", `"`,
		"&amp;", "&",
		"&lt;", "<",
		"&gt;", ">",
		"&nbsp;", " ",
	)
	return r.Replace(s)
}

// dedupeByThread keeps the newest message per threadId. Input must already be
// sorted newest-first (triageQuery guarantees this).
func dedupeByThread(rows []EmailRow) []EmailRow {
	seen := map[string]bool{}
	out := rows[:0]
	for _, r := range rows {
		if r.ThreadID == "" || !seen[r.ThreadID] {
			seen[r.ThreadID] = true
			out = append(out, r)
		}
	}
	return out
}

// primaryLabelName returns the first label-name in `wanted` whose ID appears on
// the message. Returns "" if none match - caller drops the row.
func primaryLabelName(labelIDs []string, wanted []string, idByName map[string]string) string {
	idSet := map[string]bool{}
	for _, id := range labelIDs {
		idSet[id] = true
	}
	for _, name := range wanted {
		if id, ok := idByName[name]; ok && idSet[id] {
			return name
		}
	}
	return ""
}

// collapseGoogleDocs groups messages from *@docs.google.com that share a
// subject into one EmailRow per subject with Snippet = "(N new comments/replies)".
// Non-docs rows pass through unchanged.
func collapseGoogleDocs(rows []EmailRow) []EmailRow {
	type bucket struct {
		base    EmailRow
		count   int
		senders map[string]bool
	}
	var ordered []*bucket
	byKey := map[string]*bucket{}

	out := make([]EmailRow, 0, len(rows))
	for _, r := range rows {
		if !strings.Contains(r.From, "@docs.google.com") {
			out = append(out, r)
			continue
		}
		key := r.Subject
		if b, ok := byKey[key]; ok {
			b.count++
			b.senders[displaySender(r.From)] = true
			continue
		}
		b := &bucket{base: r, count: 1, senders: map[string]bool{displaySender(r.From): true}}
		byKey[key] = b
		ordered = append(ordered, b)
	}
	for _, b := range ordered {
		row := b.base
		names := make([]string, 0, len(b.senders))
		for n := range b.senders {
			names = append(names, n)
		}
		sort.Strings(names)
		row.From = "Google Docs"
		row.Snippet = fmt.Sprintf("%d new comments/replies from %s", b.count, strings.Join(names, ", "))
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date.After(out[j].Date) })
	return out
}

// partnerTagFromLabels returns the partner name (label name minus the prefix)
// for the first matching Partners/<Name> label on the message. Returns "" if none.
func partnerTagFromLabels(labelIDs []string, prefix string, nameByID map[string]string) string {
	for _, id := range labelIDs {
		name := nameByID[id]
		if strings.HasPrefix(name, prefix) {
			return strings.TrimPrefix(name, prefix)
		}
	}
	return ""
}

// gmailLabelMaps returns id->name and name->id maps via one users.labels.list call.
// Retries once on per-call timeout - this is the first gmail call in Step 4 and
// a hang here forfeits all downstream triage.
func gmailLabelMaps(ctx context.Context) (nameByID, idByName map[string]string, err error) {
	params, _ := json.Marshal(map[string]any{"userId": "me"})
	out, err := runGWSWithRetry(ctx, gwsLabelsListDeadline, "gmail.labels.list",
		"gmail", "users", "labels", "list", "--params", string(params))
	if err != nil {
		return nil, nil, err
	}
	body, perr := firstJSONObject(out)
	if perr != nil {
		return nil, nil, perr
	}
	var resp struct {
		Labels []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, nil, err
	}
	nameByID = make(map[string]string, len(resp.Labels))
	idByName = make(map[string]string, len(resp.Labels))
	for _, l := range resp.Labels {
		nameByID[l.ID] = l.Name
		idByName[l.Name] = l.ID
	}
	return nameByID, idByName, nil
}

// buildLabelOrQuery joins label names with " OR " for the messages.list q param.
func buildLabelOrQuery(labels []string) string {
	parts := make([]string, len(labels))
	for i, l := range labels {
		parts[i] = "label:" + l
	}
	return strings.Join(parts, " OR ")
}

// buildPartnerQuery builds: is:unread newer_than:7d (label:Partners/Google OR label:Partners/Umbrella ...)
func buildPartnerQuery(prefix string, partners []string) string {
	parts := make([]string, len(partners))
	for i, p := range partners {
		parts[i] = "label:" + prefix + p
	}
	return fmt.Sprintf("is:unread newer_than:7d (%s)", strings.Join(parts, " OR "))
}

// displaySender extracts a friendly name from a Gmail "Name <email@x>" header.
// Falls back to the email local-part if no name is present.
func displaySender(from string) string {
	from = strings.TrimSpace(from)
	if i := strings.Index(from, "<"); i > 0 {
		name := strings.Trim(strings.TrimSpace(from[:i]), `"`)
		if name != "" {
			// Drop "(Google Docs)" suffix on first names.
			if j := strings.Index(name, " ("); j > 0 {
				name = name[:j]
			}
			// First name only for noise-collapse readability.
			if sp := strings.Index(name, " "); sp > 0 {
				return name[:sp]
			}
			return name
		}
	}
	if i := strings.Index(from, "@"); i > 0 {
		return from[:i]
	}
	return from
}
