package brain

// Typed memory - Action #1a of the harness roadmap.
//
// Stores three record types with supersession-by-key semantics:
//
//   fact - long-lived assertions (e.g. "user prefers US units").
//                  Supersedes prior records with the same (type, key, profile).
//   event - timestamped occurrences. Append-only, never supersedes.
//   instruction - durable rules ("always run make install after Go changes").
//                  Supersedes prior records the same way facts do.
//
// Two-layer persistence following the brain pattern:
//   1. brain/memory/<type>/<id>.json - source of truth, syncs via git
//   2. memory_records row in brain.db - derived index for fast lookup
//
// Tasks have their own dedicated table; this module deliberately does not
// duplicate them. See db.go's TaskRecord and tasks.go for the task surface.

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ryanlitalien/aida/internal/ui"
)

// MemoryType is the type tag for a memory record.
type MemoryType string

const (
	MemoryFact        MemoryType = "fact"
	MemoryEvent       MemoryType = "event"
	MemoryInstruction MemoryType = "instruction"
)

const memoryDirName = "memory"

// IsValidMemoryType reports whether t is one of the canonical types.
func IsValidMemoryType(t MemoryType) bool {
	switch t {
	case MemoryFact, MemoryEvent, MemoryInstruction:
		return true
	}
	return false
}

// SupersedesByKey reports whether records of this type supersede prior
// records with the same key. Events accumulate; facts and instructions
// supersede.
func (t MemoryType) SupersedesByKey() bool {
	return t == MemoryFact || t == MemoryInstruction
}

// MemoryRecord is one durable memory entry. It is the same shape on disk
// (JSON) and in brain.db (one column per top-level field, with embedding
// stored as an encoded BLOB rather than the float slice).
type MemoryRecord struct {
	ID           string     `json:"id"`
	Type         MemoryType `json:"type"`
	Key          string     `json:"key,omitempty"`
	Body         string     `json:"body"`
	Embedding    []float32  `json:"-"`
	Tags         []string   `json:"tags,omitempty"`
	Profile      string     `json:"profile,omitempty"`
	Source       string     `json:"source,omitempty"`
	Created      string     `json:"created"`
	SupersededBy string     `json:"superseded_by,omitempty"`
	// Provenance lists the IDs of records (typically events) this record
	// was consolidated from. Empty for directly-written records.
	Provenance []string `json:"provenance,omitempty"`
	// Confidence is the writer's confidence in the record, 0..1. Zero
	// means "unset" (legacy records); consolidation writes 1.0, or a
	// penalized value for records born from a harsh contradiction.
	Confidence float64 `json:"confidence,omitempty"`
	// LastUsedAt and UseCount are recall-practice signals maintained in
	// brain.db only - deliberately not mirrored to the JSON files, so
	// recall bumps don't churn the brain git repo. Lost on a rebuild
	// from disk, which is acceptable: they tune ranking, not truth.
	LastUsedAt string `json:"-"`
	UseCount   int    `json:"-"`
}

// MemoryDir returns the absolute path to brain/memory for a given brain root.
func MemoryDir(brainPath string) string {
	return filepath.Join(brainPath, memoryDirName)
}

// memoryFilePath is the on-disk JSON path for a record.
func memoryFilePath(brainPath string, t MemoryType, id string) string {
	return filepath.Join(MemoryDir(brainPath), string(t), id+".json")
}

// newMemoryID generates a sortable ID: "<UTC ts>-<short hash>". The
// timestamp prefix gives lexical newest-last ordering matching every other
// brain artifact (eval-runs, exec-plans, lessons).
func newMemoryID(t MemoryType, key, body string) string {
	ts := time.Now().UTC().Format("20060102-150405.000")
	ts = strings.ReplaceAll(ts, ".", "")
	h := sha1.Sum([]byte(string(t) + "|" + key + "|" + body))
	return ts + "-" + hex.EncodeToString(h[:3])
}

// WriteMemory persists a typed memory record. It:
//
//   - validates type/body
//   - assigns ID + Created if not already set
//   - generates an embedding when one is available
//   - for fact/instruction: marks any active record with the same
//     (type, key, profile) as superseded
//   - writes the JSON file mirror at brain/memory/<type>/<id>.json
//   - inserts the row into brain.db
//
// Returns the persisted record (with ID/Created populated). Empty key on a
// fact/instruction is allowed but disables supersession - callers who want
// supersession must supply a key.
func (b *Brain) WriteMemory(ctx context.Context, m MemoryRecord) (*MemoryRecord, error) {
	if !IsValidMemoryType(m.Type) {
		return nil, fmt.Errorf("WriteMemory: invalid type %q", m.Type)
	}
	if strings.TrimSpace(m.Body) == "" {
		return nil, fmt.Errorf("WriteMemory: empty body")
	}
	if m.Created == "" {
		m.Created = time.Now().UTC().Format(time.RFC3339)
	}
	if m.ID == "" {
		m.ID = newMemoryID(m.Type, m.Key, m.Body)
	}
	if m.Profile == "" {
		m.Profile = b.profile
	}

	// Embed body when client is available. Failure is non-fatal - record
	// still writes; semantic search just won't find it until backfill.
	if m.Embedding == nil && b.Embeddings != nil && b.Embeddings.Available() {
		embs, err := b.Embeddings.EmbedDocuments(ctx, []string{m.Body})
		if err != nil {
			ui.PrintVerbose("Memory embed", "failed: "+err.Error())
		} else if len(embs) > 0 {
			m.Embedding = embs[0]
		}
	}

	// Supersession: the new record always wins. Any prior active record
	// with the same (type, key, profile) gets superseded_by pointing here.
	// Done before insert so a single transaction-equivalent ordering holds:
	// from any reader's perspective, after WriteMemory returns the new
	// record is the lone active row.
	if m.Type.SupersedesByKey() && m.Key != "" {
		if err := b.DB.SupersedePriorMemory(m.Type, m.Key, m.Profile, m.ID); err != nil {
			return nil, fmt.Errorf("supersede prior: %w", err)
		}
	}

	// 1. Disk file (source of truth).
	if err := writeMemoryFile(b.Path, &m); err != nil {
		ui.PrintVerbose("Memory file", "write error: "+err.Error())
	}

	// 2. brain.db row (derived index).
	if err := b.DB.InsertMemory(&m); err != nil {
		return nil, fmt.Errorf("insert memory: %w", err)
	}

	return &m, nil
}

// writeMemoryFile writes the JSON mirror at brain/memory/<type>/<id>.json.
// Creates the per-type directory on first write.
func writeMemoryFile(brainPath string, m *MemoryRecord) error {
	dir := filepath.Join(MemoryDir(brainPath), string(m.Type))
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal memory: %w", err)
	}
	return os.WriteFile(memoryFilePath(brainPath, m.Type, m.ID), data, 0644)
}

// ReadMemoryFile reads a single record from disk by type+id. Returns
// os.ErrNotExist (wrapped) for missing records.
func ReadMemoryFile(brainPath string, t MemoryType, id string) (*MemoryRecord, error) {
	data, err := os.ReadFile(memoryFilePath(brainPath, t, id))
	if err != nil {
		return nil, err
	}
	var m MemoryRecord
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("unmarshal memory: %w", err)
	}
	return &m, nil
}

// InsertMemory writes a memory record to brain.db. INSERT OR REPLACE so a
// rebuild from disk over an existing row is idempotent.
func (db *DB) InsertMemory(m *MemoryRecord) error {
	tags, _ := json.Marshal(m.Tags)
	provenance, _ := json.Marshal(m.Provenance)
	var emb []byte
	if m.Embedding != nil {
		emb = EncodeVector(m.Embedding)
	}
	_, err := db.conn.Exec(`
		INSERT OR REPLACE INTO memory_records
		(id, type, key, body, body_embedding, tags, profile, source, created, superseded_by,
		 provenance, confidence, last_used_at, use_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, string(m.Type), m.Key, m.Body, emb, string(tags),
		m.Profile, m.Source, m.Created, m.SupersededBy,
		string(provenance), m.Confidence, m.LastUsedAt, m.UseCount,
	)
	if err == nil {
		// Sync FTS5 corpus. Superseded memories drop out so they
		// don't compete with active ones in keyword retrieval.
		if m.SupersededBy != "" {
			db.deleteCorpusFTS("memory:" + m.ID)
		} else {
			db.upsertCorpusFTS("memory:"+m.ID, "memory:"+string(m.Type), m.Body)
		}
	}
	return err
}

// SupersedePriorMemory marks every currently-active record of the given
// (type, key, profile) as superseded by newID. Empty profile in storage
// matches an empty profile argument; otherwise profile is matched exactly.
// Superseded records also drop out of the FTS5 corpus so they don't
// compete with the active record in keyword retrieval.
func (db *DB) SupersedePriorMemory(t MemoryType, key, profile, newID string) error {
	if key == "" {
		return nil
	}
	rows, err := db.conn.Query(`
		SELECT id FROM memory_records
		 WHERE type = ? AND key = ? AND profile = ?
		   AND superseded_by = '' AND id != ?`,
		string(t), key, profile, newID,
	)
	var priorIDs []string
	if err == nil {
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				priorIDs = append(priorIDs, id)
			}
		}
		rows.Close()
	}
	if _, err := db.conn.Exec(`
		UPDATE memory_records
		   SET superseded_by = ?
		 WHERE type = ?
		   AND key = ?
		   AND profile = ?
		   AND superseded_by = ''
		   AND id != ?`,
		newID, string(t), key, profile, newID,
	); err != nil {
		return err
	}
	for _, id := range priorIDs {
		db.deleteCorpusFTS("memory:" + id)
	}
	return nil
}

// SupersedeByID marks the single record identified by oldID as
// superseded by newID and drops it from the FTS5 corpus. Mirrors
// SupersedePriorMemory's effect but targets one arbitrary record
// rather than every active row sharing a (type, key, profile) -
// enables contradiction-driven supersession discovered by
// consolidation, where the retired record need not share the new
// record's key (e.g. a renamed fact, or an instruction superseded by
// a fact). No-op if oldID/newID is empty, oldID doesn't exist, or
// oldID is already superseded.
func (db *DB) SupersedeByID(oldID, newID string) error {
	if oldID == "" || newID == "" {
		return nil
	}
	res, err := db.conn.Exec(`
		UPDATE memory_records
		   SET superseded_by = ?
		 WHERE id = ? AND superseded_by = ''`,
		newID, oldID,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		db.deleteCorpusFTS("memory:" + oldID)
	}
	return nil
}

// memorySelectCols is the canonical column list scanMemoryRow expects.
const memorySelectCols = `id, type, key, body, body_embedding, tags, profile, source, created, superseded_by,
	provenance, confidence, COALESCE(last_used_at, ''), use_count`

// GetMemory loads a record by id. Returns sql.ErrNoRows when not found.
func (db *DB) GetMemory(id string) (*MemoryRecord, error) {
	row := db.conn.QueryRow(`
		SELECT `+memorySelectCols+`
		  FROM memory_records WHERE id = ?`, id)
	return scanMemoryRow(row)
}

// ActiveMemoryByKey returns the active (non-superseded) record for
// (type, key, profile), or sql.ErrNoRows if none exists. Only meaningful
// for types that supersede by key.
func (db *DB) ActiveMemoryByKey(t MemoryType, key, profile string) (*MemoryRecord, error) {
	row := db.conn.QueryRow(`
		SELECT `+memorySelectCols+`
		  FROM memory_records
		 WHERE type = ? AND key = ? AND profile = ? AND superseded_by = ''
		 ORDER BY created DESC LIMIT 1`,
		string(t), key, profile,
	)
	return scanMemoryRow(row)
}

// MemoryListOpts filter the result of ListMemory.
type MemoryListOpts struct {
	// Types limits to records of these types. Empty means all.
	Types []MemoryType
	// Profile, when non-empty, filters by exact profile match (no
	// "fall back to empty profile" - callers pass "" to list everything).
	Profile string
	// IncludeSuperseded includes superseded rows. Default returns active only.
	IncludeSuperseded bool
	// Limit caps the result size. 0 = no limit.
	Limit int
}

// ListMemory returns records matching opts, ordered newest first by created.
func (db *DB) ListMemory(opts MemoryListOpts) ([]MemoryRecord, error) {
	var conditions []string
	var args []any
	if len(opts.Types) > 0 {
		placeholders := make([]string, len(opts.Types))
		for i, t := range opts.Types {
			placeholders[i] = "?"
			args = append(args, string(t))
		}
		conditions = append(conditions, "type IN ("+strings.Join(placeholders, ",")+")")
	}
	if opts.Profile != "" {
		conditions = append(conditions, "profile = ?")
		args = append(args, opts.Profile)
	}
	if !opts.IncludeSuperseded {
		conditions = append(conditions, "superseded_by = ''")
	}

	q := "SELECT " + memorySelectCols + " FROM memory_records"
	if len(conditions) > 0 {
		q += " WHERE " + strings.Join(conditions, " AND ")
	}
	q += " ORDER BY created DESC"
	if opts.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", opts.Limit)
	}

	rows, err := db.conn.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []MemoryRecord
	for rows.Next() {
		m, err := scanMemoryRow(rows)
		if err != nil {
			continue
		}
		out = append(out, *m)
	}
	return out, nil
}

// MemoryCount returns active and superseded counts grouped by type.
type MemoryCount struct {
	Type       MemoryType
	Active     int
	Superseded int
}

// MemoryCounts returns per-type counts for stats display.
func (db *DB) MemoryCounts() ([]MemoryCount, error) {
	rows, err := db.conn.Query(`
		SELECT type,
		       SUM(CASE WHEN superseded_by = '' THEN 1 ELSE 0 END),
		       SUM(CASE WHEN superseded_by != '' THEN 1 ELSE 0 END)
		  FROM memory_records GROUP BY type`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemoryCount
	for rows.Next() {
		var c MemoryCount
		var typ string
		if err := rows.Scan(&typ, &c.Active, &c.Superseded); err != nil {
			continue
		}
		c.Type = MemoryType(typ)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return string(out[i].Type) < string(out[j].Type) })
	return out, nil
}

// scanner is the subset of *sql.Row / *sql.Rows used by scanMemoryRow,
// so we can share logic between QueryRow and Query loops.
type scanner interface {
	Scan(dest ...any) error
}

func scanMemoryRow(s scanner) (*MemoryRecord, error) {
	var m MemoryRecord
	var typ string
	var emb []byte
	var tagsJSON, provenanceJSON string
	if err := s.Scan(
		&m.ID, &typ, &m.Key, &m.Body, &emb, &tagsJSON,
		&m.Profile, &m.Source, &m.Created, &m.SupersededBy,
		&provenanceJSON, &m.Confidence, &m.LastUsedAt, &m.UseCount,
	); err != nil {
		return nil, err
	}
	m.Type = MemoryType(typ)
	if len(emb) > 0 {
		m.Embedding = DecodeVector(emb)
	}
	if tagsJSON != "" {
		json.Unmarshal([]byte(tagsJSON), &m.Tags)
	}
	if provenanceJSON != "" {
		json.Unmarshal([]byte(provenanceJSON), &m.Provenance)
	}
	return &m, nil
}

// Compile-time check that sql.ErrNoRows is what GetMemory propagates,
// so callers can rely on errors.Is(err, sql.ErrNoRows).
var _ = sql.ErrNoRows
