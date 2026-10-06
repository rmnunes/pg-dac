package refactor_test

import (
	"strings"
	"testing"

	"github.com/rodrigonunes/pg-dac/internal/model"
	"github.com/rodrigonunes/pg-dac/internal/refactor"
)

// fixture: app.t(id serial pk, b int) with index/trigger, app.u with an FK to
// t, view app.v over t, the serial's sequence, and the trigger function.
func fixture() *model.Database {
	one := 1
	_ = one
	t := &model.Table{Schema: "app", Name: "t",
		Columns: []*model.Column{
			{Name: "id", Type: "integer", NotNull: true, Default: "nextval('app.t_id_seq'::regclass)"},
			{Name: "b", Type: "integer"},
		},
		Constraints: []*model.Constraint{{Name: "t_pkey", Type: model.ConstraintPrimaryKey, Definition: "PRIMARY KEY (id)", Columns: []string{"id"}, Validated: true}},
		Indexes:     []*model.Index{{Name: "t_b_idx", Table: "t", Definition: "CREATE INDEX t_b_idx ON app.t USING btree (b)", Columns: []string{"b"}}},
		Triggers: []*model.Trigger{{Name: "tg", Table: "t", Definition: "CREATE TRIGGER tg BEFORE INSERT ON app.t FOR EACH ROW EXECUTE FUNCTION app.tf()",
			DependsOn: []model.ObjRef{{Kind: model.KindFunction, Schema: "app", Name: "tf()"}}}},
	}
	u := &model.Table{Schema: "app", Name: "u",
		Columns: []*model.Column{{Name: "tid", Type: "integer"}},
		Constraints: []*model.Constraint{{Name: "u_tid_fkey", Type: model.ConstraintForeignKey, Definition: "FOREIGN KEY (tid) REFERENCES app.t(id)",
			Columns: []string{"tid"}, RefSchema: "app", RefTable: "t", RefColumns: []string{"id"}, Validated: true}},
	}
	v := &model.View{Schema: "app", Name: "v", Definition: "SELECT id, b FROM app.t",
		DependsOn:  []model.ObjRef{t.Ref()},
		ColumnDeps: []model.ColumnRef{{Schema: "app", Table: "t", Column: "id"}, {Schema: "app", Table: "t", Column: "b"}},
		Columns:    []model.ViewColumn{{Name: "id", Type: "integer"}, {Name: "b", Type: "integer"}}}
	seq := &model.Sequence{Schema: "app", Name: "t_id_seq", DataType: "integer", Increment: 1, MinValue: 1, MaxValue: 2147483647, Start: 1, Cache: 1,
		OwnedBy: &model.ColumnRef{Schema: "app", Table: "t", Column: "id"}}
	f := &model.Function{Schema: "app", Name: "tf", Kind: "function", ReturnType: "trigger",
		Definition: "CREATE OR REPLACE FUNCTION app.tf()\n RETURNS trigger\n LANGUAGE plpgsql\nAS $function$BEGIN RETURN NEW; END$function$"}
	return &model.Database{TargetVersion: 160000, Schemas: []*model.Schema{{Name: "public"}, {
		Name: "app", Tables: []*model.Table{t, u}, Views: []*model.View{v}, Sequences: []*model.Sequence{seq}, Functions: []*model.Function{f},
	}}}
}

func schemaOf(db *model.Database, name string) *model.Schema {
	for _, s := range db.Schemas {
		if s.Name == name {
			return s
		}
	}
	return nil
}

func logOf(ops ...refactor.Op) *refactor.Log {
	l := &refactor.Log{}
	for i, op := range ops {
		if op.ID == "" {
			op.ID = string(rune('a' + i))
		}
		l.Operations = append(l.Operations, op)
	}
	return l
}

func TestPending(t *testing.T) {
	db := fixture()
	log := logOf(
		refactor.Op{ID: "1", Kind: refactor.KindTable, Schema: "app", From: "t", To: "t2"},
		refactor.Op{ID: "2", Kind: refactor.KindColumn, Schema: "app", Table: "t2", From: "b", To: "bee"}, // sees the renamed table
		refactor.Op{ID: "3", Kind: refactor.KindTable, Schema: "app", From: "gone", To: "x"},              // nothing to rename
		refactor.Op{ID: "4", Kind: refactor.KindTable, Schema: "app", From: "u", To: "t2"},                // both exist afterwards
		refactor.Op{ID: "5", Kind: refactor.KindView, Schema: "app", From: "v", To: "v2"},
	)
	renames, warns := refactor.Pending(db, log, map[string]bool{"5": true})
	var ids []string
	for _, r := range renames {
		ids = append(ids, r.Op.ID)
	}
	if strings.Join(ids, ",") != "1,2" {
		t.Errorf("pending ids: %v", ids)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "both the old and the new name") {
		t.Errorf("warnings: %v", warns)
	}
	if renames[0].SQL[0] != `ALTER TABLE "app"."t" RENAME TO "t2";` || renames[1].SQL[0] != `ALTER TABLE "app"."t2" RENAME COLUMN "b" TO "bee";` {
		t.Errorf("sql: %v %v", renames[0].SQL, renames[1].SQL)
	}
	// Idempotent: once the target shows the new names, nothing is pending.
	renamed := refactor.ApplyToModel(db, renames)
	if again, _ := refactor.Pending(renamed, log, map[string]bool{"5": true}); len(again) != 0 {
		t.Errorf("renamed model should have nothing pending, got %d", len(again))
	}
}

func TestApplyToModel_TableAndColumn(t *testing.T) {
	db := fixture()
	log := logOf(
		refactor.Op{Kind: refactor.KindTable, Schema: "app", From: "t", To: "t2"},
		refactor.Op{Kind: refactor.KindColumn, Schema: "app", Table: "t2", From: "b", To: "bee"},
		refactor.Op{Kind: refactor.KindSequence, Schema: "app", From: "t_id_seq", To: "t2_id_seq"},
		refactor.Op{Kind: refactor.KindIndex, Schema: "app", From: "t_b_idx", To: "t2_bee_idx"},
		refactor.Op{Kind: refactor.KindConstraint, Schema: "app", Table: "t2", From: "t_pkey", To: "t2_pkey"},
	)
	renames, warns := refactor.Pending(db, log, nil)
	if len(renames) != 5 || len(warns) != 0 {
		t.Fatalf("expected 5 renames, got %d (warns %v)", len(renames), warns)
	}
	out := refactor.ApplyToModel(db, renames)
	if db.Table("app", "t") == nil || db.Table("app", "t2") != nil {
		t.Fatal("the original model must not be modified")
	}
	t2 := out.Table("app", "t2")
	if t2 == nil || out.Table("app", "t") != nil {
		t.Fatal("table not renamed in the copy")
	}
	if t2.Column("bee") == nil || t2.Column("b") != nil {
		t.Error("column not renamed")
	}
	if got := t2.Column("id").Default; got != "nextval('app.t2_id_seq'::regclass)" {
		t.Errorf("default should follow the sequence rename, got %q", got)
	}
	if ix := t2.Indexes[0]; ix.Name != "t2_bee_idx" || ix.Definition != "CREATE INDEX t2_bee_idx ON app.t2 USING btree (bee)" {
		t.Errorf("index: %+v", ix)
	}
	if con := t2.Constraints[0]; con.Name != "t2_pkey" {
		t.Errorf("constraint: %+v", con)
	}
	if tr := t2.Triggers[0]; !strings.Contains(tr.Definition, "ON app.t2 FOR") {
		t.Errorf("trigger: %q", tr.Definition)
	}
	u := out.Table("app", "u")
	if fk := u.Constraints[0]; fk.RefTable != "t2" || fk.Definition != "FOREIGN KEY (tid) REFERENCES app.t2(id)" {
		t.Errorf("fk: %+v", fk)
	}
	var v *model.View
	var seq *model.Sequence
	for _, s := range out.Schemas {
		for _, vv := range s.Views {
			if vv.Name == "v" {
				v = vv
			}
		}
		for _, sq := range s.Sequences {
			seq = sq
		}
	}
	if v.DependsOn[0].Name != "t2" || v.ColumnDeps[1].Table != "t2" || v.ColumnDeps[1].Column != "bee" {
		t.Errorf("view deps: %+v %+v", v.DependsOn, v.ColumnDeps)
	}
	if seq.Name != "t2_id_seq" || seq.OwnedBy.Table != "t2" {
		t.Errorf("sequence: %+v owned by %+v", seq, seq.OwnedBy)
	}
}

func TestApplyToModel_FunctionEnumSchema(t *testing.T) {
	db := fixture()
	db.Schemas[1].Enums = []*model.EnumType{{Schema: "app", Name: "status", Values: []string{"a"}}}
	db.Schemas[1].Tables[0].Columns[1].Type = "app.status"
	db.Schemas[1].Functions[0].IdentityArgs = ""
	renames, _ := refactor.Pending(db, logOf(
		refactor.Op{Kind: refactor.KindFunction, Schema: "app", From: "tf()", To: "touch"},
		refactor.Op{Kind: refactor.KindEnum, Schema: "app", From: "status", To: "state"},
	), nil)
	if len(renames) != 2 {
		t.Fatalf("expected 2 renames, got %d", len(renames))
	}
	if renames[0].SQL[0] != `ALTER ROUTINE "app"."tf"() RENAME TO "touch";` || renames[1].SQL[0] != `ALTER TYPE "app"."status" RENAME TO "state";` {
		t.Errorf("sql: %v %v", renames[0].SQL, renames[1].SQL)
	}
	out := refactor.ApplyToModel(db, renames)
	f := schemaOf(out, "app").Functions[0]
	if f.Name != "touch" || !strings.Contains(f.Definition, "FUNCTION app.touch()") {
		t.Errorf("function: %s %q", f.Name, f.Definition)
	}
	tr := out.Table("app", "t").Triggers[0]
	if tr.DependsOn[0].Name != "touch()" || !strings.Contains(tr.Definition, "EXECUTE FUNCTION app.touch()") {
		t.Errorf("trigger: %+v %q", tr.DependsOn, tr.Definition)
	}
	if got := out.Table("app", "t").Column("b").Type; got != "app.state" {
		t.Errorf("column type should follow the enum rename, got %q", got)
	}

	renames, _ = refactor.Pending(fixture(), logOf(refactor.Op{Kind: refactor.KindSchema, From: "app", To: "core"}), nil)
	out = refactor.ApplyToModel(fixture(), renames)
	if out.Table("core", "t") == nil || out.Table("app", "t") != nil {
		t.Fatal("schema rename")
	}
	if got := out.Table("core", "t").Column("id").Default; got != "nextval('core.t_id_seq'::regclass)" {
		t.Errorf("default after schema rename: %q", got)
	}
	if fk := out.Table("core", "u").Constraints[0]; fk.RefSchema != "core" || fk.Definition != "FOREIGN KEY (tid) REFERENCES core.t(id)" {
		t.Errorf("fk after schema rename: %+v", fk)
	}
}

func TestStatementsAndRecord(t *testing.T) {
	renames, _ := refactor.Pending(fixture(), logOf(refactor.Op{ID: "abc", Kind: refactor.KindTable, Schema: "app", From: "t", To: "t2", Note: "n"}), nil)
	stmts := refactor.Statements(renames)
	if len(stmts) != 4 { // rename + CREATE SCHEMA + CREATE TABLE + INSERT
		t.Fatalf("expected 4 statements, got %d", len(stmts))
	}
	if !strings.HasPrefix(stmts[0].Note, "refactorlog abc:") || stmts[0].SQL != `ALTER TABLE "app"."t" RENAME TO "t2";` {
		t.Errorf("first: %+v", stmts[0])
	}
	if !strings.Contains(stmts[3].SQL, "INSERT INTO pgdac.refactorlog") || !strings.Contains(stmts[3].SQL, "'abc'") {
		t.Errorf("record: %s", stmts[3].SQL)
	}
}
