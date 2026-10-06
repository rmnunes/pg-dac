package sqlgen

import (
	"fmt"
	"strings"

	"github.com/rodrigonunes/pg-dac/internal/diff"
	"github.com/rodrigonunes/pg-dac/internal/model"
	"github.com/rodrigonunes/pg-dac/internal/plan"
	"github.com/rodrigonunes/pg-dac/internal/safety"
)

// Options controls how SQL is generated.
type Options struct {
	// TargetMajor is the PostgreSQL major version the migration targets. It gates
	// version-specific safe strategies (0 = unknown -> most conservative path).
	TargetMajor int
	// Online enables lock-safe rewrites: CREATE INDEX CONCURRENTLY, constraints
	// added NOT VALID then validated, etc. When false, the simplest direct form
	// is emitted (suitable for an empty/throwaway database).
	Online bool
}

// OpStatements links a plan operation to the SQL statements it produced. It
// preserves the op<->statement relationship that a GUI needs for per-object SQL
// and include/exclude (one object change can expand to several statements).
type OpStatements struct {
	Op         diff.Op
	Statements []Statement
}

// GenerateGrouped renders a plan, keeping each op's statements grouped with the op.
func GenerateGrouped(p *plan.Plan, opts Options) ([]OpStatements, error) {
	out := make([]OpStatements, 0, len(p.Ops))
	for _, op := range p.Ops {
		stmts, err := renderOp(op, opts)
		if err != nil {
			return nil, err
		}
		out = append(out, OpStatements{Op: op, Statements: stmts})
	}
	return out, nil
}

// Generate renders a plan into a flat, ordered statement list (the form the
// script writer and executor consume).
func Generate(p *plan.Plan, opts Options) ([]Statement, error) {
	groups, err := GenerateGrouped(p, opts)
	if err != nil {
		return nil, err
	}
	var out []Statement
	for _, g := range groups {
		out = append(out, g.Statements...)
	}
	return out, nil
}

// renderOp renders one op; an op-level Reason (why the op exists beyond the
// plain diff) becomes the note of its first statement.
func renderOp(op diff.Op, opts Options) ([]Statement, error) {
	stmts, err := renderOpInner(op, opts)
	if err != nil {
		return nil, err
	}
	if op.Reason != "" && len(stmts) > 0 {
		if stmts[0].Note == "" {
			stmts[0].Note = op.Reason
		} else {
			stmts[0].Note = op.Reason + "; " + stmts[0].Note
		}
	}
	return stmts, nil
}

func renderOpInner(op diff.Op, opts Options) ([]Statement, error) {
	switch op.Type {
	case diff.OpCreateSchema:
		return one("CREATE SCHEMA "+model.QuoteIdent(op.Schema.Name)+";", op.Schema.Comment, commentOn("SCHEMA", model.QuoteIdent(op.Schema.Name), op.Schema.Comment)), nil
	case diff.OpDropSchema:
		return haz("DROP SCHEMA "+model.QuoteIdent(op.Schema.Name)+";", "drops a schema and everything still in it",
			safety.New(safety.DeletesData, "dropping schema "+op.Schema.Name+" removes all objects it contains")), nil

	case diff.OpCreateExtension:
		sql := "CREATE EXTENSION IF NOT EXISTS " + model.QuoteIdent(op.Extension.Name)
		if op.Extension.Schema != "" {
			sql += " SCHEMA " + model.QuoteIdent(op.Extension.Schema)
		}
		return one(sql+";", "", nil), nil
	case diff.OpDropExtension:
		return one("DROP EXTENSION "+model.QuoteIdent(op.Extension.Name)+";", "", nil), nil
	case diff.OpAlterExtension:
		s := Statement{SQL: "ALTER EXTENSION " + model.QuoteIdent(op.Extension.Name) + " UPDATE TO " + model.QuoteLiteral(op.Extension.Version) + ";", Transactional: true}
		s.Hazards = append(s.Hazards, safety.New(safety.UpgradingExtension, "runs the extension's own upgrade scripts"))
		return []Statement{s}, nil

	case diff.OpCreateEnum:
		vals := make([]string, len(op.Enum.Values))
		for i, v := range op.Enum.Values {
			vals[i] = model.QuoteLiteral(v)
		}
		sql := "CREATE TYPE " + op.Enum.QualifiedName() + " AS ENUM (" + strings.Join(vals, ", ") + ");"
		return one(sql, op.Enum.Comment, commentOn("TYPE", op.Enum.QualifiedName(), op.Enum.Comment)), nil
	case diff.OpDropEnum:
		return one("DROP TYPE "+op.Enum.QualifiedName()+";", "", nil), nil
	case diff.OpAddEnumValue:
		var stmts []Statement
		note := ""
		if !safety.AddEnumValueInTransaction(opts.TargetMajor) {
			note = "ALTER TYPE ADD VALUE cannot run in a transaction before PostgreSQL 12"
		}
		for i, v := range op.EnumValues {
			sql := "ALTER TYPE " + op.Enum.QualifiedName() + " ADD VALUE IF NOT EXISTS " + model.QuoteLiteral(v)
			if i < len(op.EnumPositions) {
				switch p := op.EnumPositions[i]; {
				case p.Before != "":
					sql += " BEFORE " + model.QuoteLiteral(p.Before)
				case p.After != "":
					sql += " AFTER " + model.QuoteLiteral(p.After)
				}
			}
			stmts = append(stmts, Statement{
				SQL:           sql + ";",
				Transactional: safety.AddEnumValueInTransaction(opts.TargetMajor),
				Note:          note,
			})
		}
		return stmts, nil

	case diff.OpCreateDomain:
		return one(terminate(op.Domain.Definition), op.Domain.Comment,
			commentOn("DOMAIN", op.Domain.QualifiedName(), op.Domain.Comment)), nil
	case diff.OpDropDomain:
		return one("DROP DOMAIN "+op.Domain.QualifiedName()+";", "", nil), nil
	case diff.OpCreateCompositeType:
		return one(terminate(op.Composite.Definition), op.Composite.Comment,
			commentOn("TYPE", op.Composite.QualifiedName(), op.Composite.Comment)), nil
	case diff.OpDropCompositeType:
		return one("DROP TYPE "+op.Composite.QualifiedName()+";", "", nil), nil

	case diff.OpCreateCollation:
		return one(terminate(op.Collation.Definition), op.Collation.Comment,
			commentOn("COLLATION", op.Collation.QualifiedName(), op.Collation.Comment)), nil
	case diff.OpDropCollation:
		return one("DROP COLLATION "+op.Collation.QualifiedName()+";", "", nil), nil

	case diff.OpCreateSequence:
		return one(renderCreateSequence(op.Sequence), op.Sequence.Comment, commentOn("SEQUENCE", op.Sequence.QualifiedName(), op.Sequence.Comment)), nil
	case diff.OpDropSequence:
		return one("DROP SEQUENCE "+op.Sequence.QualifiedName()+";", "", nil), nil
	case diff.OpAlterSequence:
		return one(renderAlterSequence(op.Sequence), "", nil), nil
	case diff.OpSetSequenceOwner:
		return one(renderSequenceOwner(op.Sequence), "", nil), nil

	case diff.OpCreateTable:
		return renderCreateTable(op.Table), nil
	case diff.OpSetTableLogged:
		return renderSetLogged(op), nil
	case diff.OpSetRelOptions:
		return renderRelOptionsChange(op), nil
	case diff.OpSetColumnStorage, diff.OpSetColumnCompression, diff.OpSetColumnStats:
		return renderColumnAttr(op), nil
	case diff.OpAlterColumnGenerated:
		return renderAlterGenerated(op, opts), nil
	case diff.OpSetComment:
		return renderComment(op), nil
	case diff.OpUnsupported:
		return renderUnsupported(op), nil
	case diff.OpDropTable:
		return haz("DROP TABLE "+op.Table.QualifiedName()+";", "drops a table and all its data",
			safety.New(safety.DeletesData, "dropping table "+op.Table.Name+" destroys its data"),
			safety.New(safety.AccessExclusiveLock, "takes ACCESS EXCLUSIVE on the table")), nil

	case diff.OpAddColumn:
		return renderAddColumn(op, opts), nil
	case diff.OpDropColumn:
		return haz("ALTER TABLE "+op.Table.QualifiedName()+" DROP COLUMN "+model.QuoteIdent(op.Column.Name)+";",
			"drops a column and its data",
			safety.New(safety.DeletesData, "dropping column "+op.Column.Name+" destroys its data"),
			safety.New(safety.AccessExclusiveLock, "takes ACCESS EXCLUSIVE on the table")), nil
	case diff.OpAlterColumnType:
		return renderAlterColumnType(op), nil
	case diff.OpSetColumnDefault:
		return one("ALTER TABLE "+op.Table.QualifiedName()+" ALTER COLUMN "+model.QuoteIdent(op.Column.Name)+" SET DEFAULT "+op.Column.Default+";", "", nil), nil
	case diff.OpDropColumnDefault:
		return one("ALTER TABLE "+op.Table.QualifiedName()+" ALTER COLUMN "+model.QuoteIdent(op.Column.Name)+" DROP DEFAULT;", "", nil), nil
	case diff.OpSetColumnNotNull:
		return renderSetNotNull(op, opts), nil
	case diff.OpDropColumnNotNull:
		return one("ALTER TABLE "+op.Table.QualifiedName()+" ALTER COLUMN "+model.QuoteIdent(op.Column.Name)+" DROP NOT NULL;", "", nil), nil
	case diff.OpAlterColumnIdentity:
		return renderAlterColumnIdentity(op), nil

	case diff.OpAddConstraint:
		return renderAddConstraint(op, opts), nil
	case diff.OpDropConstraint:
		return haz("ALTER TABLE "+op.Table.QualifiedName()+" DROP CONSTRAINT "+model.QuoteIdent(op.Constraint.Name)+";",
			"", safety.New(safety.AccessExclusiveLock, "takes ACCESS EXCLUSIVE on the table")), nil
	case diff.OpValidateConstraint:
		return []Statement{{
			SQL:           "ALTER TABLE " + op.Table.QualifiedName() + " VALIDATE CONSTRAINT " + model.QuoteIdent(op.Constraint.Name) + ";",
			Transactional: true,
			Note:          "validate the existing NOT VALID constraint (SHARE UPDATE EXCLUSIVE — does not block writes)",
		}}, nil

	case diff.OpCreateIndex:
		return renderCreateIndex(op, opts), nil
	case diff.OpDropIndex:
		return renderDropIndex(op, opts), nil

	case diff.OpCreateView:
		v := op.View
		sql := "CREATE OR REPLACE VIEW " + v.QualifiedName()
		var vopts []string
		if v.SecurityBarrier {
			vopts = append(vopts, "security_barrier=true")
		}
		if v.SecurityInvoker {
			vopts = append(vopts, "security_invoker=true")
		}
		if len(vopts) > 0 {
			sql += " WITH (" + strings.Join(vopts, ", ") + ")"
		}
		sql += " AS " + v.Definition
		if v.CheckOption != "" {
			sql += " WITH " + strings.ToUpper(v.CheckOption) + " CHECK OPTION"
		}
		return one(sql+";", v.Comment, commentOn("VIEW", v.QualifiedName(), v.Comment)), nil
	case diff.OpDropView:
		return one("DROP VIEW "+op.View.QualifiedName()+";", "", nil), nil

	case diff.OpCreateMatView:
		data := " WITH DATA"
		if !op.MatView.WithData {
			data = " WITH NO DATA"
		}
		sql := "CREATE MATERIALIZED VIEW " + op.MatView.QualifiedName() + " AS " + op.MatView.Definition + data + ";"
		stmts := one(sql, op.MatView.Comment, commentOn("MATERIALIZED VIEW", op.MatView.QualifiedName(), op.MatView.Comment))
		if op.Reason != "" && op.MatView.WithData {
			stmts[0].Hazards = append(stmts[0].Hazards, safety.New(safety.ImpactsPerformance,
				"the materialized view is rebuilt (its data is recomputed) as part of being recreated"))
		}
		return stmts, nil
	case diff.OpDropMatView:
		return one("DROP MATERIALIZED VIEW "+op.MatView.QualifiedName()+";", "", nil), nil

	case diff.OpCreateFunction:
		f := op.Function
		objType := "FUNCTION"
		if f.Kind == "procedure" {
			objType = "PROCEDURE"
		}
		return one(terminate(f.Definition), f.Comment, commentOn(objType, f.QualifiedSignature(), f.Comment)), nil
	case diff.OpDropFunction:
		f := op.Function
		objType := "FUNCTION"
		if f.Kind == "procedure" {
			objType = "PROCEDURE"
		}
		return one("DROP "+objType+" "+f.QualifiedSignature()+";", "", nil), nil

	case diff.OpCreateTrigger:
		tr := op.Trigger
		return one(terminate(tr.Definition), tr.Comment,
			commentOn("TRIGGER", model.QuoteIdent(tr.Name)+" ON "+relName(op), tr.Comment)), nil
	case diff.OpDropTrigger:
		return one("DROP TRIGGER "+model.QuoteIdent(op.Trigger.Name)+" ON "+relName(op)+";", "", nil), nil
	case diff.OpSetTriggerEnabled:
		return renderTriggerEnabled(op), nil

	case diff.OpCreatePolicy:
		return renderCreatePolicy(op), nil
	case diff.OpDropPolicy:
		return one("DROP POLICY "+model.QuoteIdent(op.Policy.Name)+" ON "+op.Table.QualifiedName()+";", "", nil), nil

	case diff.OpEnableRLS:
		return renderEnableRLS(op), nil
	case diff.OpDisableRLS:
		return one("ALTER TABLE "+op.Table.QualifiedName()+" DISABLE ROW LEVEL SECURITY;", "", nil), nil
	}
	return nil, fmt.Errorf("sqlgen: unhandled op type %q", op.Type)
}

// --- helpers ---

// relName is the qualified name of the relation an op's Owner refers to (a
// table or a view — trigger ops exist for both).
func relName(op diff.Op) string { return model.QualifiedName(op.Owner.Schema, op.Owner.Name) }

func one(sql, _ string, extra []Statement) []Statement {
	stmts := []Statement{{SQL: sql, Transactional: true}}
	return append(stmts, extra...)
}

func haz(sql, note string, hazards ...safety.Hazard) []Statement {
	return []Statement{{SQL: sql, Transactional: true, Note: note, Hazards: hazards}}
}

// commentOn returns a COMMENT ON statement (as a 1-element slice) when body is
// non-empty, else nil — so it can be appended after a create.
func commentOn(objType, qualifiedName, body string) []Statement {
	if body == "" {
		return nil
	}
	return []Statement{{
		SQL:           "COMMENT ON " + objType + " " + qualifiedName + " IS " + model.QuoteLiteral(body) + ";",
		Transactional: true,
	}}
}
