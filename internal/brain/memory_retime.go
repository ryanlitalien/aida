package brain

// In-place timestamp correction for captured memories.
//
// Early capture (and especially the one-shot backfill) stamped Created at
// the moment of capture, because Claude Code memory files carry no authoring
// date in their frontmatter. That collapsed every backfilled record onto a
// single instant, so recency recall could not tell a months-old note from a
// just-written one. Capture now derives Created from the source file's mtime
// (its real last-write time); RetimeMemory lets a re-run fix already-stored
// records to that mtime WITHOUT superseding them -- a pure data correction,
// not a new memory version, so the brain repo doesn't accumulate a shadow
// copy of every record.

import "fmt"

// UpdateMemoryCreated rewrites just the created column of one record. Used by
// RetimeMemory; does not touch supersession, embedding, or FTS (the body is
// unchanged, so none of those are affected).
func (db *DB) UpdateMemoryCreated(id, created string) error {
	if id == "" || created == "" {
		return nil
	}
	_, err := db.conn.Exec(`UPDATE memory_records SET created = ? WHERE id = ?`, created, id)
	return err
}

// RetimeMemory corrects a record's Created timestamp in place -- updating
// both the brain.db row and the JSON mirror -- without changing its ID, body,
// or active/superseded state. Returns the record's prior Created so callers
// can report what changed. No-op-safe: a record whose Created already equals
// created is left untouched (nil error, prior == created).
func (b *Brain) RetimeMemory(id, created string) (prior string, err error) {
	if b == nil || b.DB == nil {
		return "", fmt.Errorf("RetimeMemory: nil brain")
	}
	rec, err := b.DB.GetMemory(id)
	if err != nil {
		return "", err
	}
	prior = rec.Created
	if prior == created || created == "" {
		return prior, nil
	}
	rec.Created = created
	if err := writeMemoryFile(b.Path, rec); err != nil {
		return prior, fmt.Errorf("rewrite mirror: %w", err)
	}
	if err := b.DB.UpdateMemoryCreated(id, created); err != nil {
		return prior, fmt.Errorf("update db: %w", err)
	}
	return prior, nil
}
