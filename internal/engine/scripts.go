package engine

import (
	"fmt"
	"strings"

	"github.com/rmnunes/pg-dac/internal/diff"
	"github.com/rmnunes/pg-dac/internal/model"
	"github.com/rmnunes/pg-dac/internal/sqlgen"
)

// ExtractSection is the folder an extracted project's scripts are written to.
const ExtractSection = "schema"

// ObjectScript is one object's canonical CREATE SQL, the project folder it
// belongs in, and a filename stem (unique within the folder). Used by
// `extract --to-project` to lay a database out as a declarative project.
type ObjectScript struct {
	Section string
	Base    string
	SQL     string
}

// FileName returns a filesystem-safe "<base>.sql".
func (o ObjectScript) FileName() string { return sanitizeFilename(o.Base) + ".sql" }

// ObjectScripts renders a database as a numbered sequence of per-object scripts
// in global dependency order — the very order a from-empty publish applies — so
// the resulting project builds cleanly even when a function's signature needs a
// view's row type, a view calls such a function, or a trigger executes it.
// Consecutive plan operations on the same object share one file; a table's
// foreign keys, triggers and policies therefore land in later files of their
// own once everything they reference exists (the file name says what they hold).
func ObjectScripts(db *model.Database) []ObjectScript {
	baseline := &model.Database{TargetVersion: db.TargetVersion, Schemas: []*model.Schema{{Name: "public"}}}
	res, err := Compare(baseline, db, Options{Online: false})
	if err != nil {
		return nil
	}

	var out []ObjectScript
	n := 0
	for i := 0; i < len(res.Groups); {
		owner := res.Groups[i].Op.Owner
		j := i
		var sqls []string
		for j < len(res.Groups) && res.Groups[j].Op.Owner.Key() == owner.Key() {
			for _, s := range res.Groups[j].Statements {
				sqls = append(sqls, s.SQL)
			}
			j++
		}
		sql := strings.TrimSpace(strings.Join(sqls, "\n"))
		if sql != "" {
			n++
			out = append(out, ObjectScript{Section: ExtractSection, Base: scriptBase(n, owner, groupSuffix(res.Groups[i:j])), SQL: sql})
		}
		i = j
	}
	return out
}

// scriptBase builds the file stem "NNNN_<kind>_<schema.name><suffix>". A
// function's signature is reduced to its name (the sequence number already
// keeps overloads apart), and the stem is bounded so deep paths stay well under
// filesystem limits.
func scriptBase(n int, owner model.ObjRef, suffix string) string {
	name := owner.Name
	if owner.Kind == model.KindFunction {
		if p := strings.IndexByte(name, '('); p > 0 {
			name = name[:p]
		}
	}
	if owner.Schema != "" {
		name = owner.Schema + "." + name
	}
	const maxName = 80
	if len(name) > maxName {
		name = name[:maxName]
	}
	return fmt.Sprintf("%04d_%s_%s%s", n, owner.Kind, name, suffix)
}

// groupSuffix names a file that holds only sub-object operations of an object
// created in an earlier file (its foreign keys, triggers, ...); it is empty when
// the file creates the object itself.
func groupSuffix(groups []sqlgen.OpStatements) string {
	for _, g := range groups {
		switch g.Op.Type {
		case diff.OpCreateTable, diff.OpCreateView, diff.OpCreateMatView, diff.OpCreateFunction,
			diff.OpCreateSequence, diff.OpCreateSchema, diff.OpCreateExtension, diff.OpCreateEnum,
			diff.OpCreateDomain, diff.OpCreateCompositeType, diff.OpCreateCollation:
			return ""
		}
	}
	switch groups[0].Op.Type {
	case diff.OpAddConstraint:
		return "_constraints"
	case diff.OpCreateIndex:
		return "_indexes"
	case diff.OpCreateTrigger, diff.OpSetTriggerEnabled:
		return "_triggers"
	case diff.OpSetColumnStorage, diff.OpSetColumnCompression, diff.OpSetColumnStats:
		return "_columns"
	case diff.OpSetComment:
		return "_comments"
	case diff.OpCreatePolicy:
		return "_policies"
	case diff.OpEnableRLS:
		return "_rls"
	case diff.OpSetSequenceOwner:
		return "_owner"
	case diff.OpValidateConstraint:
		return "_validate"
	}
	return "_" + string(groups[0].Op.Type)
}

func sanitizeFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return strings.Trim(b.String(), "_")
}
