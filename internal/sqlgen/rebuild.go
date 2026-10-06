package sqlgen

import (
	"strings"

	"github.com/rmnunes/pg-dac/internal/diff"
	"github.com/rmnunes/pg-dac/internal/model"
	"github.com/rmnunes/pg-dac/internal/safety"
)

// rebuildSuffix names the temporary table a rebuild creates next to the
// original before swapping them.
const rebuildSuffix = "__pgdac_rebuild"

// renderRebuildTable renders an SSDT-style table rebuild as ONE atomic DO
// block: create the table under a temporary name with the declared column
// order, copy every row (common columns by name; identity values preserved
// with OVERRIDING SYSTEM VALUE; generated columns recomputed), detach the
// serial sequences the old table's columns own (they would be dropped with
// it), drop the old table, rename the new one, re-own the sequences, give
// identity sequences their conventional names and seed them past the copied
// values. Constraints, indexes, triggers, policies, comments and privileges
// are separate operations that run after this one, and the planner brackets
// the block with the drops and recreates of everything that depends on the
// table (foreign keys from other tables, views).
//
// Columns of the old table that the new one does not declare are not copied:
// their data is lost, which the statement reports as DELETES_DATA. The engine
// appends such columns to op.Table beforehand when the drop policy keeps them.
func renderRebuildTable(op diff.Op) []Statement {
	t, prev := op.Table, op.PrevTable
	tmpName := t.Name + rebuildSuffix
	if len(tmpName) > 63 {
		tmpName = tmpName[:63]
	}
	tmp := model.QualifiedName(t.Schema, tmpName)
	final := t.QualifiedName()

	keyword := "TABLE"
	if t.Persistence == "unlogged" {
		keyword = "UNLOGGED TABLE"
	}
	lines := make([]string, 0, len(t.Columns))
	for _, c := range t.Columns {
		lines = append(lines, "        "+columnDef(c))
	}
	create := "    CREATE " + keyword + " " + tmp + " (\n" + strings.Join(lines, ",\n") + "\n    )"
	if len(t.RelOptions) > 0 {
		create += " WITH (" + renderRelOptions(t.RelOptions) + ")"
	}

	// Columns to copy: declared columns the old table also has, minus generated
	// ones (recomputed on insert).
	prevCols := map[string]bool{}
	for _, c := range prev.Columns {
		prevCols[c.Name] = true
	}
	var copied []string
	hasIdentity := false
	for _, c := range t.Columns {
		if !prevCols[c.Name] || c.Generated != "" {
			continue
		}
		copied = append(copied, model.QuoteIdent(c.Name))
		if c.Identity != "" {
			hasIdentity = true
		}
	}
	var lost []string
	declared := map[string]bool{}
	for _, c := range t.Columns {
		declared[c.Name] = true
	}
	for _, c := range prev.Columns {
		if !declared[c.Name] {
			lost = append(lost, c.Name)
		}
	}

	var b strings.Builder
	b.WriteString("DO $pgdac$ BEGIN\n")
	b.WriteString(create + ";\n")
	if len(copied) > 0 {
		override := ""
		if hasIdentity {
			override = "OVERRIDING SYSTEM VALUE "
		}
		cols := strings.Join(copied, ", ")
		b.WriteString("    INSERT INTO " + tmp + " (" + cols + ") " + override + "SELECT " + cols + " FROM " + final + ";\n")
	}
	for _, s := range op.OwnedSequences {
		b.WriteString("    ALTER SEQUENCE " + s.QualifiedName() + " OWNED BY NONE;\n")
	}
	b.WriteString("    DROP TABLE " + final + ";\n")
	b.WriteString("    ALTER TABLE " + tmp + " RENAME TO " + model.QuoteIdent(t.Name) + ";\n")
	for _, s := range op.OwnedSequences {
		if s.OwnedBy == nil {
			continue
		}
		b.WriteString("    ALTER SEQUENCE " + s.QualifiedName() + " OWNED BY " + final + "." + model.QuoteIdent(s.OwnedBy.Column) + ";\n")
	}
	for _, c := range t.Columns {
		if c.Identity == "" {
			continue
		}
		seq := identitySequenceExpr(t, c.Name)
		conventional := t.Name + "_" + c.Name + "_seq"
		if len(conventional) > 63 {
			conventional = conventional[:63]
		}
		// The identity sequence was named after the temporary table; give it the
		// name PostgreSQL would have chosen, unless that name is taken.
		b.WriteString("    BEGIN\n        EXECUTE format('ALTER SEQUENCE %s RENAME TO %I', " + seq + "::text, " + model.QuoteLiteral(conventional) + ");\n" +
			"    EXCEPTION WHEN duplicate_table THEN NULL; END;\n")
		if prevCols[c.Name] {
			b.WriteString("    PERFORM pg_catalog.setval(" + seq + ", COALESCE((SELECT max(" + model.QuoteIdent(c.Name) + ") FROM " + final + "), 0) + 1, false);\n")
		}
	}
	b.WriteString("END $pgdac$;")

	stmt := Statement{
		SQL:           b.String(),
		Transactional: true,
		Note: "column order: the table is rebuilt — a new table in the declared order, every row copied, then swapped in (one atomic block); " +
			"its constraints, indexes, triggers, policies, privileges and dependents are recreated by the statements that follow",
		Hazards: []safety.Hazard{
			safety.New(safety.TableRewrite, "copies every row into a new table and drops the old one"),
			safety.New(safety.AccessExclusiveLock, "takes ACCESS EXCLUSIVE on the table for the whole copy"),
		},
	}
	if len(lost) > 0 {
		stmt.Hazards = append(stmt.Hazards, safety.New(safety.DeletesData,
			"column(s) "+strings.Join(lost, ", ")+" exist in the target only and are not carried over: their data is lost"))
	}
	stmts := []Statement{stmt}
	stmts = append(stmts, commentOn("TABLE", final, t.Comment)...)
	for _, c := range t.Columns {
		stmts = append(stmts, commentOn("COLUMN", final+"."+model.QuoteIdent(c.Name), c.Comment)...)
	}
	return stmts
}
