package brain

// Full-corpus re-embed tool -- generalizes scripts/backfill_embeddings.go
// (which was lessons-only and NULL-rows-only) to cover every embedded
// table in brain.db with overwrite semantics, for use whenever the
// embedding model changes (e.g. voyage-3-lite -> voyage-4-lite). Old and
// new model vectors are not comparable even at the same dimension, so a
// model swap requires re-embedding every row, not just backfilling missing
// ones -- `aida brain index` deliberately does NOT do this (it only fills
// NULL embeddings via InsertLesson/UpsertEntity/InsertMemory's "generate if
// missing" checks in compile.go, and leaves existing vectors untouched).
//
// Every row re-embedded here is corpus text (a lesson question, an entity
// summary, a wiki page body, ...), not a live user query, so every table
// uses EmbedDocuments ("document" input_type) -- matching the discipline
// documented on EmbeddingClient.EmbedDocument: mixing "query" and
// "document" input types drops cosine similarity from ~1.0 to ~0.81 even
// for identical text.
//
// Mirrors mine.go's shape: a thin method (ReembedAll) supplies the real
// embedding client, and the injectable core (reembedAll) takes an EmbedFunc
// so it's testable without a live Voyage key.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// reembedSpec describes one embedded table/column pair. textExpr is a raw
// SQL expression (not a bound parameter) so wiki_pages can concatenate
// title+body the way IndexWiki originally embedded them -- every value
// here is a fixed literal from the table below, never user input, so
// string-building the query is safe.
type reembedSpec struct {
	table    string
	idCol    string
	textExpr string
	embedCol string
}

// reembedTables lists every column in brain.db that stores a Voyage
// embedding. routing_rules is included for completeness even though it's
// unused today (0 rows) -- see db.go's migrate().
var reembedTables = []reembedSpec{
	{table: "lessons", idCol: "id", textExpr: "question", embedCol: "question_embedding"},
	{table: "routing_rules", idCol: "id", textExpr: "pattern", embedCol: "pattern_embedding"},
	{table: "entities", idCol: "slug", textExpr: "summary", embedCol: "summary_embedding"},
	{table: "memory_records", idCol: "id", textExpr: "body", embedCol: "body_embedding"},
	{table: "wiki_pages", idCol: "slug", textExpr: "title || char(10) || body", embedCol: "embedding"},
	{table: "knowledge_pages", idCol: "slug", textExpr: "title || char(10) || body", embedCol: "embedding"},
	{table: "jarvis_lessons", idCol: "id", textExpr: "query", embedCol: "query_embedding"},
	{table: "run_cache", idCol: "id", textExpr: "question", embedCol: "question_embedding"},
}

// ReembedTableNames returns the valid --table values for the CLI, in the
// fixed order reembedTables defines (lessons first, run_cache last).
func ReembedTableNames() []string {
	names := make([]string, len(reembedTables))
	for i, s := range reembedTables {
		names[i] = s.table
	}
	return names
}

// reembedRow is one pending (id, source text) pair awaiting a fresh
// embedding -- shared between reembedTable (which reads it) and
// writeReembedBatch (which writes the result back).
type reembedRow struct {
	id   string
	text string
}

// ReembedTableResult is the outcome of re-embedding one table.
type ReembedTableResult struct {
	Table   string
	Rows    int // rows with non-empty source text found
	Updated int // rows actually written (0 on dry-run)
}

// ReembedResult summarizes one full ReembedAll run.
type ReembedResult struct {
	DryRun bool
	Tables []ReembedTableResult
}

// TotalRows sums Rows across every table in the result.
func (r *ReembedResult) TotalRows() int {
	total := 0
	for _, t := range r.Tables {
		total += t.Rows
	}
	return total
}

// TotalUpdated sums Updated across every table in the result.
func (r *ReembedResult) TotalUpdated() int {
	total := 0
	for _, t := range r.Tables {
		total += t.Updated
	}
	return total
}

// ReembedAll re-embeds every row in every embedded table with the brain's
// currently configured model, OVERWRITING any existing vector -- this is
// the operation a model swap requires, not an incremental backfill.
// tableNames, if non-empty, restricts the pass to those tables (see
// ReembedTableNames) so a migration can be staged one table at a time.
// dryRun reports row counts without calling the embedding API or writing.
func (b *Brain) ReembedAll(ctx context.Context, tableNames []string, dryRun bool) (*ReembedResult, error) {
	return b.reembedAll(ctx, b.Embeddings.EmbedDocuments, tableNames, dryRun)
}

// reembedAll is the injectable core -- embed is swappable in tests.
func (b *Brain) reembedAll(ctx context.Context, embed EmbedFunc, tableNames []string, dryRun bool) (*ReembedResult, error) {
	want := make(map[string]bool, len(tableNames))
	for _, t := range tableNames {
		want[t] = true
	}

	result := &ReembedResult{DryRun: dryRun}
	for _, spec := range reembedTables {
		if len(want) > 0 && !want[spec.table] {
			continue
		}
		tr, err := b.DB.reembedTable(ctx, embed, spec, dryRun)
		if err != nil {
			return nil, fmt.Errorf("reembed %s: %w", spec.table, err)
		}
		result.Tables = append(result.Tables, tr)
	}
	return result, nil
}

// reembedTable re-embeds every row in one table whose source text is
// non-empty. Reads the full pending set, embeds it in one call (EmbedFunc
// implementations chunk internally at voyageMaxBatch -- see
// EmbedDocuments), then writes every vector back in a single transaction.
// A batch-sized corpus (brain.db's 7 tables total ~1,700 rows) doesn't need
// the free-tier throttle scripts/backfill_embeddings.go used; reembedWithRetry
// below still backs off on a real 429.
func (db *DB) reembedTable(ctx context.Context, embed EmbedFunc, spec reembedSpec, dryRun bool) (ReembedTableResult, error) {
	result := ReembedTableResult{Table: spec.table}

	query := fmt.Sprintf(
		`SELECT %s, %s FROM %s WHERE trim(coalesce(%s, '')) != ''`,
		spec.idCol, spec.textExpr, spec.table, spec.textExpr,
	)
	rows, err := db.conn.QueryContext(ctx, query)
	if err != nil {
		return result, err
	}
	var todo []reembedRow
	for rows.Next() {
		var p reembedRow
		if err := rows.Scan(&p.id, &p.text); err != nil {
			rows.Close()
			return result, err
		}
		todo = append(todo, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return result, err
	}
	rows.Close()

	result.Rows = len(todo)
	if len(todo) == 0 || dryRun {
		return result, nil
	}

	texts := make([]string, len(todo))
	for i, p := range todo {
		texts[i] = p.text
	}

	vecs, err := reembedWithRetry(ctx, embed, texts)
	if err != nil {
		return result, err
	}
	if len(vecs) != len(todo) {
		return result, fmt.Errorf("got %d vectors for %d rows", len(vecs), len(todo))
	}

	if err := writeReembedBatch(db.conn, spec, todo, vecs); err != nil {
		return result, err
	}
	result.Updated = len(todo)
	return result, nil
}

func writeReembedBatch(conn *sql.DB, spec reembedSpec, todo []reembedRow, vecs [][]float32) error {
	tx, err := conn.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(fmt.Sprintf(`UPDATE %s SET %s = ? WHERE %s = ?`, spec.table, spec.embedCol, spec.idCol))
	if err != nil {
		tx.Rollback()
		return err
	}
	for i, p := range todo {
		if _, err := stmt.Exec(EncodeVector(vecs[i]), p.id); err != nil {
			stmt.Close()
			tx.Rollback()
			return err
		}
	}
	stmt.Close()
	return tx.Commit()
}

// reembedWithRetry retries a whole embed call on a Voyage 429, with
// linearly increasing backoff -- mirrors scripts/backfill_embeddings.go's
// proven retry loop. Retrying the whole call (rather than a sub-batch) is
// correct here because EmbedDocuments itself has no partial-success
// return: if any of its internal voyageMaxBatch-sized chunks 429s, the
// whole call errors out with no indication of which chunk got through.
func reembedWithRetry(ctx context.Context, embed EmbedFunc, texts []string) ([][]float32, error) {
	var vecs [][]float32
	var err error
	for attempt := 0; attempt < 6; attempt++ {
		vecs, err = embed(ctx, texts)
		if err == nil {
			return vecs, nil
		}
		if !strings.Contains(err.Error(), "status 429") {
			return nil, err
		}
		wait := time.Duration(30*(attempt+1)) * time.Second
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	return nil, fmt.Errorf("exhausted retries: %w", err)
}
