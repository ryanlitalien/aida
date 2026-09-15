package brain

// Durable "thread memory" for the Jarvis voice layer. The in-process
// listener Session (internal/jarvis) is short-term and volatile; this is
// the durable backstop. Each substantive voice turn is auto-promoted here
// as an append-only MemoryEvent, and recall surfaces the most relevant ones
// into the system prompt on later turns - even days later, even after a
// daemon restart. This is what lets "build the next layer down" inherit the
// material/coordinate from a prior turn instead of regressing.
//
// Storage reuses the typed-memory table (memory_types.go): MemoryEvent is
// append-only (never supersedes), which is exactly right for a stream of
// conversation summaries. No new table, no migration.

import (
	"context"
	"sort"
	"strings"
)

const (
	// ThreadMemoryTag marks a MemoryEvent as a Jarvis conversation-thread
	// summary, so recall can filter to just these (vs. other events).
	ThreadMemoryTag = "jarvis-thread"
	// threadMemorySource attributes thread events to the voice layer.
	threadMemorySource = "jarvis:voice"
	// threadRecallThreshold mirrors the jarvis_lessons voice cut - short,
	// noisy voice queries need a stricter cosine threshold than the engine.
	threadRecallThreshold = 0.25
	// threadScanLimit caps how many recent events we score per recall.
	threadScanLimit = 500
)

// SimilarThreadMemory pairs a thread-memory record with its cosine
// similarity to a query embedding.
type SimilarThreadMemory struct {
	Record     MemoryRecord
	Similarity float64
}

// WriteThreadMemory persists one conversation-thread summary as an
// append-only MemoryEvent tagged ThreadMemoryTag. Append-only (not a
// superseding fact) is deliberate: a build proceeds across many turns and
// each turn's summary should accumulate, not clobber the last. An empty or
// whitespace summary is a no-op (nil, nil). Embedding and profile defaults
// are handled inside WriteMemory.
func (b *Brain) WriteThreadMemory(ctx context.Context, summary string) (*MemoryRecord, error) {
	if b == nil || strings.TrimSpace(summary) == "" {
		return nil, nil
	}
	return b.WriteMemory(ctx, MemoryRecord{
		Type:   MemoryEvent,
		Body:   summary,
		Tags:   []string{ThreadMemoryTag},
		Source: threadMemorySource,
	})
}

// FindSimilarThreadMemory returns up to k thread-memory summaries whose
// embedding is cosine-similar to queryEmbedding above the voice threshold,
// most-similar first. Mirrors FindSimilarJarvisLessons. Returns nil on any
// soft failure (no embedding, empty store) - recall is purely additive.
func (b *Brain) FindSimilarThreadMemory(ctx context.Context, queryEmbedding []float32, k int) ([]SimilarThreadMemory, error) {
	if b == nil || b.DB == nil || len(queryEmbedding) == 0 {
		return nil, nil
	}
	return b.DB.FindSimilarThreadMemory(queryEmbedding, k, b.profile, threadRecallThreshold)
}

// FindSimilarThreadMemory (DB level) scans the most recent thread events for
// the profile, scores each by cosine similarity, and returns the top k above
// threshold sorted descending. Records without an embedding are skipped.
func (db *DB) FindSimilarThreadMemory(queryEmbedding []float32, k int, profile string, threshold float64) ([]SimilarThreadMemory, error) {
	if len(queryEmbedding) == 0 || k <= 0 {
		return nil, nil
	}
	recs, err := db.ListMemory(MemoryListOpts{
		Types:   []MemoryType{MemoryEvent},
		Profile: profile,
		Limit:   threadScanLimit,
	})
	if err != nil {
		return nil, err
	}
	var out []SimilarThreadMemory
	for _, r := range recs {
		if !threadTagged(r.Tags) || len(r.Embedding) == 0 {
			continue
		}
		sim := CosineSimilarity(queryEmbedding, r.Embedding)
		if sim > threshold {
			out = append(out, SimilarThreadMemory{Record: r, Similarity: sim})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Similarity > out[j].Similarity })
	if len(out) > k {
		out = out[:k]
	}
	return out, nil
}

// FormatThreadRecallProse renders recalled thread summaries as a
// system-prompt block. Empty input returns "" (no block, caller's prompt
// unchanged). Bodies are shown nearly whole (summaries are already short and
// carry the build parameters - material, coordinate, next step - that a
// limited-context follow-up needs); they are context, not a script.
func FormatThreadRecallProse(recalled []SimilarThreadMemory) string {
	if len(recalled) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("## Earlier in this conversation\n")
	// Continuity WITHOUT precedent: "stay consistent" originally read as an
	// absolute, and a recalled failure turn (a clarification question, a
	// tool miss) anchored the model into repeating it verbatim on the next
	// similar query. Carry facts forward; never carry outcomes forward.
	sb.WriteString("Prior turns from this ongoing thread, most relevant first. Carry their FACTS forward - materials, coordinates, names, the next step. They are memory, not precedent: if a past reply asked for clarification, missed, or you now know better (check your user context and tools), answer properly this time instead of repeating it. Don't read them back verbatim.\n")
	for _, r := range recalled {
		sb.WriteString("- ")
		sb.WriteString(truncateForPrompt(r.Record.Body, 500))
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// threadTagged reports whether tags include ThreadMemoryTag.
func threadTagged(tags []string) bool {
	for _, t := range tags {
		if t == ThreadMemoryTag {
			return true
		}
	}
	return false
}
