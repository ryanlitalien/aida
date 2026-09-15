package brain

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// JarvisToolCall mirrors the audit.ToolCall shape so storage layer doesn't
// depend on the jarvis package (avoids an import cycle).
type JarvisToolCall struct {
	Name        string `json:"name"`
	TookMs      int64  `json:"took_ms,omitempty"`
	EngineRunID string `json:"engine_run_id,omitempty"`
}

// JarvisLesson is one rated Jarvis turn.
type JarvisLesson struct {
	ID                      string           `json:"id"`
	Timestamp               string           `json:"timestamp"`
	Profile                 string           `json:"profile"`
	RatedTurnStartedAt      string           `json:"rated_turn_started_at"`
	Transcript              string           `json:"transcript"`
	Query                   string           `json:"query"`
	Embedding               []float32        `json:"-"`
	Reply                   string           `json:"reply"`
	ToolCalls               []JarvisToolCall `json:"tool_calls"`
	Feedback                string           `json:"feedback"` // "up" | "down" | "note"
	FeedbackReason          string           `json:"feedback_reason,omitempty"`
	FeedbackIntendedTool    string           `json:"feedback_intended_tool,omitempty"`
	FeedbackAvoidTool       string           `json:"feedback_avoid_tool,omitempty"`
	FeedbackStyleDirectives []string         `json:"feedback_style_directives,omitempty"`
}

// SimilarJarvisLesson pairs a JarvisLesson with its cosine similarity to a
// query embedding.
type SimilarJarvisLesson struct {
	Lesson     JarvisLesson
	Similarity float64
}

// RecordJarvisLesson embeds the query and inserts a row. Returns the id.
// Failures to embed (no Voyage key, network error) are non-fatal - the row
// is still written without an embedding (it just won't appear in recall).
func (b *Brain) RecordJarvisLesson(ctx context.Context, l *JarvisLesson) (string, error) {
	if l.Timestamp == "" {
		l.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	if l.ID == "" {
		hash := shortHash(l.Query + "|" + l.Feedback)
		tsClean := strings.ReplaceAll(strings.ReplaceAll(l.Timestamp, ":", "-"), "+", "")
		l.ID = fmt.Sprintf("%s-%s", tsClean, hash)
	}
	if l.Profile == "" {
		l.Profile = b.profile
	}

	if b.Embeddings != nil && b.Embeddings.Available() && len(l.Embedding) == 0 && l.Query != "" {
		embs, err := b.Embeddings.EmbedDocuments(ctx, []string{l.Query})
		if err == nil && len(embs) > 0 {
			l.Embedding = embs[0]
		}
	}

	if err := b.DB.InsertJarvisLesson(l); err != nil {
		return "", fmt.Errorf("brain db insert jarvis_lesson: %w", err)
	}
	return l.ID, nil
}

// FindSimilarJarvisLessons returns up to k rated lessons whose query
// embedding is cosine-similar to queryEmbedding above the threshold.
// Voice queries are shorter and noisier than engine queries, so threshold
// is stricter (0.25 vs the engine's 0.15).
func (b *Brain) FindSimilarJarvisLessons(ctx context.Context, queryEmbedding []float32, k int) ([]SimilarJarvisLesson, error) {
	if len(queryEmbedding) == 0 {
		return nil, nil
	}
	return b.DB.FindSimilarJarvisLessons(queryEmbedding, k, b.profile, 0.25)
}

// InsertJarvisLesson writes one jarvis_lessons row. Replaces on id conflict.
func (db *DB) InsertJarvisLesson(l *JarvisLesson) error {
	toolCalls, _ := json.Marshal(l.ToolCalls)
	styleDirectives, _ := json.Marshal(l.FeedbackStyleDirectives)
	var embedding []byte
	if len(l.Embedding) > 0 {
		embedding = EncodeVector(l.Embedding)
	}
	_, err := db.conn.Exec(`
		INSERT OR REPLACE INTO jarvis_lessons
		(id, timestamp, profile, rated_turn_started_at, transcript, query, query_embedding,
		 reply, tool_calls, feedback, feedback_reason,
		 feedback_intended_tool, feedback_avoid_tool, feedback_style_directives)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.ID, l.Timestamp, l.Profile, l.RatedTurnStartedAt,
		l.Transcript, l.Query, embedding,
		l.Reply, string(toolCalls), l.Feedback, l.FeedbackReason,
		l.FeedbackIntendedTool, l.FeedbackAvoidTool, string(styleDirectives),
	)
	return err
}

// FindSimilarJarvisLessons runs the cosine search. Reads up to 500 most
// recent rows from the profile, scores each, returns the top k above
// threshold sorted by similarity desc.
func (db *DB) FindSimilarJarvisLessons(queryEmbedding []float32, k int, profile string, threshold float64) ([]SimilarJarvisLesson, error) {
	rows, err := db.conn.Query(`
		SELECT id, timestamp, profile, rated_turn_started_at, transcript, query, query_embedding,
		       reply, tool_calls, feedback, feedback_reason,
		       feedback_intended_tool, feedback_avoid_tool, feedback_style_directives
		FROM jarvis_lessons
		WHERE query_embedding IS NOT NULL
		  AND (? = '' OR profile = ? OR profile = '')
		ORDER BY timestamp DESC
		LIMIT 500`, profile, profile)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []SimilarJarvisLesson
	for rows.Next() {
		var l JarvisLesson
		var embBlob []byte
		var toolCallsJSON, styleDirectivesJSON string
		if err := rows.Scan(
			&l.ID, &l.Timestamp, &l.Profile, &l.RatedTurnStartedAt,
			&l.Transcript, &l.Query, &embBlob,
			&l.Reply, &toolCallsJSON, &l.Feedback, &l.FeedbackReason,
			&l.FeedbackIntendedTool, &l.FeedbackAvoidTool, &styleDirectivesJSON,
		); err != nil {
			continue
		}
		json.Unmarshal([]byte(toolCallsJSON), &l.ToolCalls)
		json.Unmarshal([]byte(styleDirectivesJSON), &l.FeedbackStyleDirectives)

		if len(embBlob) == 0 {
			continue
		}
		stored := DecodeVector(embBlob)
		sim := CosineSimilarity(queryEmbedding, stored)
		if sim > threshold {
			l.Embedding = stored
			results = append(results, SimilarJarvisLesson{Lesson: l, Similarity: sim})
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Similarity > results[j].Similarity
	})
	if len(results) > k {
		results = results[:k]
	}
	return results, nil
}

// FormatJarvisRecallProse renders a slice of recalled lessons as a
// system-prompt block grouped by feedback type. Empty input returns an
// empty string (no block, caller's prompt unchanged). Reply snippets
// are capped at 200 chars to keep the prompt bounded; queries aren't
// truncated since they're already user-sized.
func FormatJarvisRecallProse(recalled []SimilarJarvisLesson) string {
	if len(recalled) == 0 {
		return ""
	}
	var downs, ups, notes []SimilarJarvisLesson
	for _, r := range recalled {
		switch r.Lesson.Feedback {
		case "down":
			downs = append(downs, r)
		case "up":
			ups = append(ups, r)
		case "note":
			notes = append(notes, r)
		}
	}

	var sb strings.Builder
	sb.WriteString("## Past feedback on similar requests\n")
	sb.WriteString("Use these to shape your reply. Don't quote them verbatim; they're context, not a script.\n")

	if len(downs) > 0 {
		sb.WriteString("\n### To avoid (thumbs-down)\n")
		for _, r := range downs {
			sb.WriteString("- Previously asked: \"")
			sb.WriteString(r.Lesson.Query)
			sb.WriteString("\" - you replied: \"")
			sb.WriteString(truncateForPrompt(r.Lesson.Reply, 200))
			sb.WriteString("\" - user said: \"")
			sb.WriteString(r.Lesson.FeedbackReason)
			sb.WriteString("\"\n")
		}
	}
	if len(ups) > 0 {
		sb.WriteString("\n### Approved style (thumbs-up)\n")
		for _, r := range ups {
			sb.WriteString("- For \"")
			sb.WriteString(r.Lesson.Query)
			sb.WriteString("\" the user approved: \"")
			sb.WriteString(truncateForPrompt(r.Lesson.Reply, 200))
			sb.WriteString("\"")
			if r.Lesson.FeedbackReason != "" {
				sb.WriteString(" - reason: \"")
				sb.WriteString(r.Lesson.FeedbackReason)
				sb.WriteString("\"")
			}
			sb.WriteString("\n")
		}
	}
	if len(notes) > 0 {
		sb.WriteString("\n### Standing directives (notes)\n")
		for _, r := range notes {
			for _, d := range r.Lesson.FeedbackStyleDirectives {
				sb.WriteString("- ")
				sb.WriteString(d)
				sb.WriteString("\n")
			}
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// truncateForPrompt cuts s to at most n bytes, backing the cut off to a
// rune boundary - bodies carry °/ - /… and a mid-rune slice would feed
// invalid UTF-8 into the system prompt.
func truncateForPrompt(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// JarvisLessonCount returns the total number of rated Jarvis turns.
func (db *DB) JarvisLessonCount() int {
	var count int
	db.conn.QueryRow("SELECT COUNT(*) FROM jarvis_lessons").Scan(&count)
	return count
}

// JarvisLessonCountByFeedback returns counts keyed by feedback type
// ("up", "down", "note"). Zero-count types are omitted.
func (db *DB) JarvisLessonCountByFeedback() (map[string]int, error) {
	rows, err := db.conn.Query("SELECT feedback, COUNT(*) FROM jarvis_lessons GROUP BY feedback")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int)
	for rows.Next() {
		var fb string
		var n int
		if err := rows.Scan(&fb, &n); err != nil {
			continue
		}
		out[fb] = n
	}
	return out, nil
}
