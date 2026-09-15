package dailybriefing

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ErrGWSTimeout marks a per-call deadline expiration (distinct from parent-ctx
// cancellation). runGWSWithRetry checks errors.Is(err, ErrGWSTimeout) to decide
// whether one retry with a fresh deadline is warranted.
var ErrGWSTimeout = errors.New("gws per-call deadline exceeded")

// gws deadlines. Each call to the gws CLI is bounded independently of the
// overall briefing watchdog so a single hung subprocess can't dominate the run.
// Values picked from observed successful-run latencies (typically 1–10s) plus
// headroom for cold-cache / network jitter.
const (
	gwsLabelsListDeadline   = 30 * time.Second
	gwsMessagesListDeadline = 30 * time.Second
	gwsMessageGetDeadline   = 15 * time.Second
	gwsCalendarDeadline     = 30 * time.Second
	gwsSendDeadline         = 60 * time.Second
	gwsModifyDeadline       = 30 * time.Second
)

// runGWS executes `gws <args>` with a per-call deadline and returns stdout.
// callName is a short label used only for the elapsed-time log line.
//
// Error mapping:
//   - per-call deadline fires while parent ctx is healthy → wraps ErrGWSTimeout
//   - parent ctx cancellation                             → returns parent.Err()-derived
//   - non-zero gws exit                                   → includes stderr
func runGWS(parent context.Context, deadline time.Duration, callName string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, deadline)
	defer cancel()
	start := time.Now()
	out, err := exec.CommandContext(ctx, "gws", args...).Output()
	elapsed := time.Since(start)
	fmt.Fprintf(os.Stderr, "[gws] %s elapsed=%s\n", callName, elapsed.Truncate(time.Millisecond))
	if err != nil && ctx.Err() == context.DeadlineExceeded && parent.Err() == nil {
		return out, fmt.Errorf("gws %s: per-call timeout after %s: %w", callName, deadline, ErrGWSTimeout)
	}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return out, fmt.Errorf("gws %s failed: %w (stderr: %s)", callName, err, string(exitErr.Stderr))
		}
		return out, fmt.Errorf("gws %s: %w", callName, err)
	}
	return out, nil
}

// runGWSCombined is like runGWS but captures stdout+stderr merged. Use for
// write operations where gws reports failures on stderr that we want surfaced.
func runGWSCombined(parent context.Context, deadline time.Duration, callName string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, deadline)
	defer cancel()
	start := time.Now()
	out, err := exec.CommandContext(ctx, "gws", args...).CombinedOutput()
	elapsed := time.Since(start)
	fmt.Fprintf(os.Stderr, "[gws] %s elapsed=%s\n", callName, elapsed.Truncate(time.Millisecond))
	if err != nil && ctx.Err() == context.DeadlineExceeded && parent.Err() == nil {
		return out, fmt.Errorf("gws %s: per-call timeout after %s: %w", callName, deadline, ErrGWSTimeout)
	}
	if err != nil {
		return out, fmt.Errorf("gws %s failed: %w (output: %s)", callName, err, string(out))
	}
	return out, nil
}

// runGWSWithRetry retries runGWS once on per-call timeout. Other errors pass
// through unchanged - retrying a 4xx won't help.
func runGWSWithRetry(parent context.Context, deadline time.Duration, callName string, args ...string) ([]byte, error) {
	out, err := runGWS(parent, deadline, callName, args...)
	if err == nil || !errors.Is(err, ErrGWSTimeout) {
		return out, err
	}
	fmt.Fprintf(os.Stderr, "[gws] %s timed out after %s; retrying once\n", callName, deadline)
	return runGWS(parent, deadline, callName+"(retry)", args...)
}

// runGWSCombinedWithRetry retries runGWSCombined once on per-call timeout.
func runGWSCombinedWithRetry(parent context.Context, deadline time.Duration, callName string, args ...string) ([]byte, error) {
	out, err := runGWSCombined(parent, deadline, callName, args...)
	if err == nil || !errors.Is(err, ErrGWSTimeout) {
		return out, err
	}
	fmt.Fprintf(os.Stderr, "[gws] %s timed out after %s; retrying once\n", callName, deadline)
	return runGWSCombined(parent, deadline, callName+"(retry)", args...)
}

// CalendarToday returns the user's primary-calendar events for the current day in displayLoc.
// Filters out transparency=transparent events (Home, OOO, working location, free-time blocks)
// and cancelled events. Times are normalized to displayLoc.
func CalendarToday(ctx context.Context, displayLoc *time.Location) ([]Event, error) {
	now := time.Now().In(displayLoc)
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, displayLoc)
	dayEnd := dayStart.Add(24 * time.Hour).Add(-time.Second)

	params, _ := json.Marshal(map[string]any{
		"calendarId":   "primary",
		"timeMin":      dayStart.Format(time.RFC3339),
		"timeMax":      dayEnd.Format(time.RFC3339),
		"singleEvents": true,
		"orderBy":      "startTime",
		"maxResults":   50,
	})

	out, err := runGWS(ctx, gwsCalendarDeadline, "calendar.events.list",
		"calendar", "events", "list", "--params", string(params))
	if err != nil {
		return nil, err
	}
	return parseEventsListJSON(out, displayLoc)
}

// GmailSend sends a plain-text UTF-8 email and returns the resulting message ID.
// Uses `gws gmail users messages send --json` directly (the `+send` helper
// panics on bodies that contain "=" characters, e.g. URLs). One retry on
// per-call timeout - delivery is critical-path.
func GmailSend(ctx context.Context, to, subject, body string) (string, error) {
	raw := buildRFC5322(to, subject, body)
	encoded := base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte(raw))
	payload, _ := json.Marshal(map[string]any{"raw": encoded})
	out, err := runGWSCombinedWithRetry(ctx, gwsSendDeadline, "gmail.messages.send",
		"gmail", "users", "messages", "send",
		"--params", `{"userId":"me"}`,
		"--json", string(payload),
	)
	if err != nil {
		return "", err
	}
	id, perr := extractJSONField(out, "id")
	if perr != nil {
		return "", fmt.Errorf("parsing send response: %w (output: %s)", perr, string(out))
	}
	return id, nil
}

// buildRFC5322 constructs a minimal text/plain message. The Gmail API ignores
// any From header and stamps the authenticated user, so we omit it.
func buildRFC5322(to, subject, body string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(body, "\n", "\r\n"))
	return b.String()
}

// GmailAddLabel applies the named label to the given message ID. Looks up the
// label by name once via users.labels list, then calls users.messages modify.
// Returns an error if the label doesn't exist (create it in Gmail manually first).
func GmailAddLabel(ctx context.Context, messageID, labelName string) error {
	labelID, err := gmailFindLabelID(ctx, labelName)
	if err != nil {
		return err
	}
	params, _ := json.Marshal(map[string]any{
		"userId": "me",
		"id":     messageID,
	})
	bodyJSON, _ := json.Marshal(map[string]any{
		"addLabelIds": []string{labelID},
	})
	if _, err := runGWSCombined(ctx, gwsModifyDeadline, "gmail.messages.modify",
		"gmail", "users", "messages", "modify",
		"--params", string(params),
		"--json", string(bodyJSON),
	); err != nil {
		return err
	}
	return nil
}

// gmailFindLabelID returns the Gmail label ID for the given user-visible label name.
// Returns an error if the label does not exist.
func gmailFindLabelID(ctx context.Context, name string) (string, error) {
	params, _ := json.Marshal(map[string]any{"userId": "me"})
	out, err := runGWSWithRetry(ctx, gwsLabelsListDeadline, "gmail.labels.list",
		"gmail", "users", "labels", "list", "--params", string(params))
	if err != nil {
		return "", err
	}
	var resp struct {
		Labels []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"labels"`
	}
	body, perr := firstJSONObject(out)
	if perr != nil {
		return "", fmt.Errorf("parsing labels.list response: %w", perr)
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("decoding labels.list JSON: %w", err)
	}
	for _, l := range resp.Labels {
		if l.Name == name {
			return l.ID, nil
		}
	}
	return "", fmt.Errorf("label %q not found in Gmail", name)
}

// extractJSONField extracts a single string field from the first {...} object
// in a noisy output stream (gws prints "Tip: ..." lines before the JSON body).
func extractJSONField(out []byte, field string) (string, error) {
	body, err := firstJSONObject(out)
	if err != nil {
		return "", err
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return "", err
	}
	v, ok := m[field].(string)
	if !ok {
		return "", fmt.Errorf("field %q missing or not a string", field)
	}
	return v, nil
}

// firstJSONObject returns the first top-level {...} block found in out.
func firstJSONObject(out []byte) ([]byte, error) {
	start := -1
	depth := 0
	for i, b := range out {
		switch b {
		case '{':
			if start == -1 {
				start = i
			}
			depth++
		case '}':
			depth--
			if depth == 0 && start != -1 {
				return out[start : i+1], nil
			}
		}
	}
	return nil, fmt.Errorf("no JSON object found in output")
}
