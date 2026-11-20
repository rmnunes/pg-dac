// Package refactor implements the refactorlog: a project-side record of
// renames (tables, columns, views, functions, ...) that `publish` applies as
// ALTER ... RENAME instead of the drop + create pair a pure diff would produce —
// the DACPAC refactorlog idea. Each operation has a stable id; the target
// database remembers the ids it has applied in pgdac.refactorlog, so a rename
// runs exactly once per target and projects stay declarative.
package refactor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/rmnunes/pg-dac/internal/model"
	"github.com/rmnunes/pg-dac/internal/safety"
	"github.com/rmnunes/pg-dac/internal/sqlgen"
)

// LogName is the refactorlog file at a project's root.
const LogName = "refactorlog.json"

// Kind is the kind of object an operation renames.
type Kind string

const (
	KindTable      Kind = "table"
	KindColumn     Kind = "column"
	KindView       Kind = "view"
	KindMatView    Kind = "matview"
	KindSequence   Kind = "sequence"
	KindFunction   Kind = "function"
	KindIndex      Kind = "index"
	KindConstraint Kind = "constraint"
	KindTrigger    Kind = "trigger"
	KindPolicy     Kind = "policy"
	KindEnum       Kind = "enum"
	KindDomain     Kind = "domain"
	KindComposite  Kind = "compositeType"
	KindSchema     Kind = "schema"
)

// Op is one rename. From/To are bare names (a function's From is its
// signature "name(identity args)" and To the new bare name). Table names the
// owning relation for column/constraint/trigger/policy. ToSchema optionally
// moves the object to another schema (tables, views, sequences, functions,
// types).
type Op struct {
	ID       string `json:"id"`
	Kind     Kind   `json:"kind"`
	Schema   string `json:"schema,omitempty"`
	Table    string `json:"table,omitempty"`
	From     string `json:"from"`
	To       string `json:"to"`
	ToSchema string `json:"toSchema,omitempty"`
	Note     string `json:"note,omitempty"`
}

// Log is the parsed refactorlog.json.
type Log struct {
	Operations []Op `json:"operations"`
}

// Load reads dir/refactorlog.json; a missing file is an empty log.
func Load(dir string) (*Log, error) {
	data, err := os.ReadFile(filepath.Join(dir, LogName))
	if err != nil {
		if os.IsNotExist(err) {
			return &Log{}, nil
		}
		return nil, err
	}
	return Parse(data)
}

// Parse decodes a refactorlog.
func Parse(data []byte) (*Log, error) {
	var l Log
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("parse %s: %w", LogName, err)
	}
	for i, op := range l.Operations {
		if err := op.validate(); err != nil {
			return nil, fmt.Errorf("%s: operation %d: %w", LogName, i+1, err)
		}
	}
	return &l, nil
}

// Save writes the log to dir/refactorlog.json.
func (l *Log) Save(dir string) error {
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, LogName), append(data, '\n'), 0o644)
}

// Bytes serializes the log (for embedding in an artifact).
func (l *Log) Bytes() ([]byte, error) { return json.MarshalIndent(l, "", "  ") }

// Add appends an operation, assigning an id when none is given.
func (l *Log) Add(op Op) (Op, error) {
	if err := op.validate(); err != nil {
		return op, err
	}
	if op.ID == "" {
		op.ID = newID()
	}
	for _, existing := range l.Operations {
		if existing.ID == op.ID {
			return op, fmt.Errorf("refactorlog already has an operation with id %s", op.ID)
		}
	}
	l.Operations = append(l.Operations, op)
	return op, nil
}

func (op Op) validate() error {
	switch op.Kind {
	case KindTable, KindView, KindMatView, KindSequence, KindFunction, KindEnum, KindDomain, KindComposite:
		if op.Schema == "" {
			return fmt.Errorf("%s rename needs a schema", op.Kind)
		}
	case KindColumn, KindConstraint, KindTrigger, KindPolicy:
		if op.Schema == "" || op.Table == "" {
			return fmt.Errorf("%s rename needs schema and table", op.Kind)
		}
	case KindIndex:
		if op.Schema == "" {
			return fmt.Errorf("index rename needs a schema")
		}
	case KindSchema:
	default:
		return fmt.Errorf("unknown kind %q", op.Kind)
	}
	if op.From == "" || op.To == "" {
		return fmt.Errorf("%s rename needs from and to", op.Kind)
	}
	if op.Kind == KindFunction && !strings.Contains(op.From, "(") {
		return fmt.Errorf("a function rename's from must be a signature, e.g. %q", op.From+"(integer)")
	}
	return nil
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Describe renders the operation for humans.
func (op Op) Describe() string {
	target := op.Schema + "." + op.From
	switch op.Kind {
	case KindColumn, KindConstraint, KindTrigger, KindPolicy:
		target = op.Schema + "." + op.Table + "." + op.From
	case KindSchema:
		target = op.From
	}
	to := op.To
	if op.ToSchema != "" && op.ToSchema != op.Schema {
		to = op.ToSchema + "." + op.To
	}
	return fmt.Sprintf("rename %s %s → %s", op.Kind, target, to)
}

// Rename is a pending operation together with the SQL that performs it.
type Rename struct {
	Op  Op
	SQL []string
}

// Pending returns the operations that still have to run against the target:
// not yet recorded as applied there, whose "from" object exists and whose "to"
// name is free. An operation whose "from" is gone and "to" exists is treated
// as done; one where both exist cannot run and is reported as a warning.
func Pending(target *model.Database, log *Log, applied map[string]bool) (renames []Rename, warnings []string) {
	if log == nil {
		return nil, nil
	}
	// Operations are evaluated in order against a working copy that reflects
	// the renames accepted so far, so a later op may refer to a name an
	// earlier op introduced (rename the table, then a column of the new name).
	work := clone(target)
	for _, op := range log.Operations {
		if applied[op.ID] {
			continue
		}
		fromExists := exists(work, op, op.Schema, op.From)
		toSchema := op.ToSchema
		if toSchema == "" {
			toSchema = op.Schema
		}
		toExists := exists(work, op, toSchema, toName(op))
		switch {
		case fromExists && !toExists:
			renames = append(renames, Rename{Op: op, SQL: renameSQL(op)})
			applyOp(work, op)
		case fromExists && toExists:
			warnings = append(warnings, fmt.Sprintf("refactorlog %s (%s): both the old and the new name exist in the target; the rename cannot run", op.ID, op.Describe()))
		}
	}
	return renames, warnings
}

// toName is the bare name the object has after the rename (a function keeps
// its argument list).
func toName(op Op) string {
	if op.Kind == KindFunction {
		if p := strings.IndexByte(op.From, '('); p >= 0 {
			return op.To + op.From[p:]
		}
	}
	return op.To
}

// exists reports whether the object the op refers to, under the given schema
// and bare name, is present in the model.
func exists(db *model.Database, op Op, schema, name string) bool {
	switch op.Kind {
	case KindSchema:
		return db.Schema(name) != nil
	case KindTable:
		return db.Table(schema, name) != nil
	case KindColumn:
		t := db.Table(schema, op.Table)
		return t != nil && t.Column(name) != nil
	case KindView:
		return db.View(schema, name) != nil
	case KindMatView:
		return db.MatView(schema, name) != nil
	case KindSequence:
		s := db.Schema(schema)
		if s == nil {
			return false
		}
		for _, sq := range s.Sequences {
			if sq.Name == name {
				return true
			}
		}
	case KindFunction:
		s := db.Schema(schema)
		if s == nil {
			return false
		}
		for _, f := range s.Functions {
			if f.Signature() == name {
				return true
			}
		}
	case KindEnum:
		s := db.Schema(schema)
		if s == nil {
			return false
		}
		for _, e := range s.Enums {
			if e.Name == name {
				return true
			}
		}
	case KindDomain:
		s := db.Schema(schema)
		if s == nil {
			return false
		}
		for _, d := range s.Domains {
			if d.Name == name {
				return true
			}
		}
	case KindComposite:
		s := db.Schema(schema)
		if s == nil {
			return false
		}
		for _, c := range s.Composites {
			if c.Name == name {
				return true
			}
		}
	case KindIndex:
		s := db.Schema(schema)
		if s == nil {
			return false
		}
		for _, t := range s.Tables {
			for _, ix := range t.Indexes {
				if ix.Name == name {
					return true
				}
			}
		}
		for _, mv := range s.MatViews {
			for _, ix := range mv.Indexes {
				if ix.Name == name {
					return true
				}
			}
		}
	case KindConstraint:
		t := db.Table(schema, op.Table)
		if t == nil {
			return false
		}
		for _, c := range t.Constraints {
			if c.Name == name {
				return true
			}
		}
	case KindTrigger:
		for _, tr := range triggersOf(db, schema, op.Table) {
			if tr.Name == name {
				return true
			}
		}
	case KindPolicy:
		t := db.Table(schema, op.Table)
		if t == nil {
			return false
		}
		for _, p := range t.Policies {
			if p.Name == name {
				return true
			}
		}
	}
	return false
}

// renameSQL renders the ALTER ... RENAME (and SET SCHEMA) statements for an op.
func renameSQL(op Op) []string {
	q := model.QuoteIdent
	qn := model.QualifiedName
	var stmts []string
	var moveKeyword string // ALTER <kw> <new qualified> SET SCHEMA
	switch op.Kind {
	case KindSchema:
		return []string{"ALTER SCHEMA " + q(op.From) + " RENAME TO " + q(op.To) + ";"}
	case KindTable:
		stmts = append(stmts, "ALTER TABLE "+qn(op.Schema, op.From)+" RENAME TO "+q(op.To)+";")
		moveKeyword = "TABLE"
	case KindColumn:
		return []string{"ALTER TABLE " + qn(op.Schema, op.Table) + " RENAME COLUMN " + q(op.From) + " TO " + q(op.To) + ";"}
	case KindView:
		stmts = append(stmts, "ALTER VIEW "+qn(op.Schema, op.From)+" RENAME TO "+q(op.To)+";")
		moveKeyword = "VIEW"
	case KindMatView:
		stmts = append(stmts, "ALTER MATERIALIZED VIEW "+qn(op.Schema, op.From)+" RENAME TO "+q(op.To)+";")
		moveKeyword = "MATERIALIZED VIEW"
	case KindSequence:
		stmts = append(stmts, "ALTER SEQUENCE "+qn(op.Schema, op.From)+" RENAME TO "+q(op.To)+";")
		moveKeyword = "SEQUENCE"
	case KindFunction:
		name, args := splitSignature(op.From)
		stmts = append(stmts, "ALTER ROUTINE "+qn(op.Schema, name)+args+" RENAME TO "+q(op.To)+";")
		if op.ToSchema != "" && op.ToSchema != op.Schema {
			stmts = append(stmts, "ALTER ROUTINE "+qn(op.Schema, op.To)+args+" SET SCHEMA "+q(op.ToSchema)+";")
		}
		return stmts
	case KindEnum, KindComposite:
		stmts = append(stmts, "ALTER TYPE "+qn(op.Schema, op.From)+" RENAME TO "+q(op.To)+";")
		moveKeyword = "TYPE"
	case KindDomain:
		stmts = append(stmts, "ALTER DOMAIN "+qn(op.Schema, op.From)+" RENAME TO "+q(op.To)+";")
		moveKeyword = "DOMAIN"
	case KindIndex:
		return []string{"ALTER INDEX " + qn(op.Schema, op.From) + " RENAME TO " + q(op.To) + ";"}
	case KindConstraint:
		return []string{"ALTER TABLE " + qn(op.Schema, op.Table) + " RENAME CONSTRAINT " + q(op.From) + " TO " + q(op.To) + ";"}
	case KindTrigger:
		return []string{"ALTER TRIGGER " + q(op.From) + " ON " + qn(op.Schema, op.Table) + " RENAME TO " + q(op.To) + ";"}
	case KindPolicy:
		return []string{"ALTER POLICY " + q(op.From) + " ON " + qn(op.Schema, op.Table) + " RENAME TO " + q(op.To) + ";"}
	}
	if moveKeyword != "" && op.ToSchema != "" && op.ToSchema != op.Schema {
		stmts = append(stmts, "ALTER "+moveKeyword+" "+qn(op.Schema, op.To)+" SET SCHEMA "+q(op.ToSchema)+";")
	}
	return stmts
}

func splitSignature(sig string) (name, args string) {
	if p := strings.IndexByte(sig, '('); p >= 0 {
		return sig[:p], sig[p:]
	}
	return sig, "()"
}

// Statements renders the pending renames, followed by the statements that
// record them in the target's pgdac.refactorlog, as executable statements.
func Statements(renames []Rename) []sqlgen.Statement {
	if len(renames) == 0 {
		return nil
	}
	var out []sqlgen.Statement
	for _, r := range renames {
		for i, sql := range r.SQL {
			s := sqlgen.Statement{SQL: sql, Transactional: true,
				Hazards: []safety.Hazard{safety.New(safety.AccessExclusiveLock, "a rename takes a brief ACCESS EXCLUSIVE lock")}}
			if i == 0 {
				s.Note = "refactorlog " + r.Op.ID + ": " + r.Op.Describe()
			}
			out = append(out, s)
		}
	}
	for _, sql := range RecordSQL(renames) {
		out = append(out, sqlgen.Statement{SQL: sql, Transactional: true})
	}
	return out
}

// RecordSQL returns the statements that mark renames as applied in the target.
func RecordSQL(renames []Rename) []string {
	if len(renames) == 0 {
		return nil
	}
	out := []string{
		"CREATE SCHEMA IF NOT EXISTS pgdac;",
		"CREATE TABLE IF NOT EXISTS pgdac.refactorlog (id text PRIMARY KEY, kind text NOT NULL, object text NOT NULL, renamed_to text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now());",
	}
	for _, r := range renames {
		out = append(out, fmt.Sprintf("INSERT INTO pgdac.refactorlog (id, kind, object, renamed_to) VALUES (%s, %s, %s, %s) ON CONFLICT (id) DO NOTHING;",
			model.QuoteLiteral(r.Op.ID), model.QuoteLiteral(string(r.Op.Kind)), model.QuoteLiteral(r.Op.Describe()), model.QuoteLiteral(toName(r.Op))))
	}
	return out
}

// Querier is the subset of pgx used to read the applied log.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ReadApplied returns the ids of the operations the target has already
// applied (empty when it has no refactorlog table yet).
func ReadApplied(ctx context.Context, q Querier) (map[string]bool, error) {
	var present bool
	if err := q.QueryRow(ctx, `SELECT to_regclass('pgdac.refactorlog') IS NOT NULL`).Scan(&present); err != nil {
		return nil, fmt.Errorf("check refactorlog table: %w", err)
	}
	applied := map[string]bool{}
	if !present {
		return applied, nil
	}
	rows, err := q.Query(ctx, `SELECT id FROM pgdac.refactorlog`)
	if err != nil {
		return nil, fmt.Errorf("read refactorlog: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		applied[id] = true
	}
	return applied, rows.Err()
}

// --- lookups ---

// triggersOf returns the triggers of a table or a view.
func triggersOf(db *model.Database, schema, rel string) []*model.Trigger {
	if t := db.Table(schema, rel); t != nil {
		return t.Triggers
	}
	if v := db.View(schema, rel); v != nil {
		return v.Triggers
	}
	return nil
}
