package diff

import (
	"reflect"
	"testing"

	"github.com/rmnunes/pg-dac/internal/model"
)

func opSummary(cs *ChangeSet) []string {
	out := make([]string, 0, len(cs.Ops))
	for _, op := range cs.Ops {
		out = append(out, string(op.Type)+" "+op.Owner.Key()+" "+op.Sub)
	}
	return out
}

func hasOp(cs *ChangeSet, t OpType, ownerName string) bool {
	for _, op := range cs.Ops {
		if op.Type == t && (ownerName == "" || op.Owner.Name == ownerName) {
			return true
		}
	}
	return false
}

// TestDiff_Deterministic guards the planner's tie-breaker: the differ must walk
// collections in model order, never by ranging over a Go map, or the same two
// models yield a different op order (and therefore a different script and a
// different artifact Merkle root) on every run.
func TestDiff_Deterministic(t *testing.T) {
	s := &model.Schema{Name: "app"}
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l", "m", "n"} {
		s.Tables = append(s.Tables, &model.Table{Schema: "app", Name: "t_" + n,
			Columns: []*model.Column{{Name: "id", Type: "bigint"}},
			Constraints: []*model.Constraint{
				{Name: "t_" + n + "_pkey", Type: model.ConstraintPrimaryKey, Definition: "PRIMARY KEY (id)", Validated: true},
				{Name: "t_" + n + "_chk", Type: model.ConstraintCheck, Definition: "CHECK ((id > 0))", Validated: true},
			},
			Indexes:  []*model.Index{{Name: "t_" + n + "_idx", Table: "t_" + n, Definition: "CREATE INDEX t_" + n + "_idx ON app.t_" + n + " USING btree (id)"}},
			Triggers: []*model.Trigger{{Name: "tg_" + n, Table: "t_" + n, Definition: "CREATE TRIGGER tg_" + n + " BEFORE INSERT ON app.t_" + n + " FOR EACH ROW EXECUTE FUNCTION app.f_" + n + "()"}},
			Policies: []*model.Policy{{Name: "p_" + n, Table: "t_" + n, Permissive: true, Command: "ALL", Using: "true"}},
		})
		s.Views = append(s.Views, &model.View{Schema: "app", Name: "v_" + n, Definition: "SELECT 1"})
		s.Functions = append(s.Functions, &model.Function{Schema: "app", Name: "f_" + n, Kind: "function", Definition: "CREATE OR REPLACE FUNCTION app.f_" + n + "() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RETURN NEW; END$$"})
		s.Enums = append(s.Enums, &model.EnumType{Schema: "app", Name: "e_" + n, Values: []string{"x"}})
		s.Sequences = append(s.Sequences, &model.Sequence{Schema: "app", Name: "s_" + n, Increment: 1})
		s.Collations = append(s.Collations, &model.Collation{Schema: "app", Name: "c_" + n, Definition: "CREATE COLLATION app.c_" + n + " (provider = icu, locale = 'und')"})
		s.MatViews = append(s.MatViews, &model.MaterializedView{Schema: "app", Name: "mv_" + n, Definition: "SELECT 1", WithData: true})
	}
	full := &model.Database{Schemas: []*model.Schema{{Name: "public"}, s}}
	empty := &model.Database{Schemas: []*model.Schema{{Name: "public"}}}

	for name, pair := range map[string][2]*model.Database{"creates": {empty, full}, "drops": {full, empty}} {
		first := opSummary(Diff(pair[0], pair[1]))
		if len(first) == 0 {
			t.Fatalf("%s: expected ops", name)
		}
		for i := 0; i < 50; i++ {
			if got := opSummary(Diff(pair[0], pair[1])); !reflect.DeepEqual(got, first) {
				t.Fatalf("%s: run %d produced a different op order:\n%v\nvs\n%v", name, i, got, first)
			}
		}
	}
}

func serialFixture(withDefault bool, identity string) *model.Database {
	col := &model.Column{Name: "id", Type: "integer", NotNull: true, Identity: identity}
	if withDefault {
		col.Default = "nextval('public.t_id_seq'::regclass)"
	}
	return &model.Database{Schemas: []*model.Schema{{
		Name:   "public",
		Tables: []*model.Table{{Schema: "public", Name: "t", Columns: []*model.Column{col}}},
		Sequences: []*model.Sequence{{
			Schema: "public", Name: "t_id_seq", DataType: "integer", Start: 1, Increment: 1, MinValue: 1, MaxValue: 2147483647, Cache: 1,
			OwnedBy: &model.ColumnRef{Schema: "public", Table: "t", Column: "id"},
		}},
	}}}
}

// TestDiff_SerialSequenceIsExplicit: a serial column's sequence is just an
// owned sequence plus a nextval default — it must be created (before the
// table) and attached (after it), or CREATE TABLE references a missing relation.
// An identity column's sequence is implicit and must never be emitted.
func TestDiff_SerialSequenceIsExplicit(t *testing.T) {
	empty := &model.Database{Schemas: []*model.Schema{{Name: "public"}}}

	cs := Diff(empty, serialFixture(true, ""))
	for _, want := range []OpType{OpCreateSequence, OpSetSequenceOwner, OpCreateTable} {
		if !hasOp(cs, want, "") {
			t.Errorf("serial table from empty: missing %s in %v", want, opSummary(cs))
		}
	}

	cs = Diff(empty, serialFixture(false, "byDefault"))
	if hasOp(cs, OpCreateSequence, "") || hasOp(cs, OpSetSequenceOwner, "") {
		t.Errorf("identity-backed sequence must not be emitted: %v", opSummary(cs))
	}

	// Dropping the table drops its owned sequence with it: no explicit DROP SEQUENCE.
	cs = Diff(serialFixture(true, ""), empty)
	if !hasOp(cs, OpDropTable, "t") || hasOp(cs, OpDropSequence, "") {
		t.Errorf("table drop should cascade to the owned sequence, got %v", opSummary(cs))
	}

	// serial -> identity on a surviving column: the identity op runs and the old
	// serial sequence is an explicit drop (ordered after the column ops by the planner).
	cs = Diff(serialFixture(true, ""), serialFixture(false, "byDefault"))
	if !hasOp(cs, OpAlterColumnIdentity, "t") || !hasOp(cs, OpDropSequence, "t_id_seq") {
		t.Errorf("serial->identity should alter identity and drop the old sequence, got %v", opSummary(cs))
	}

	// Self-compare is empty.
	if cs := Diff(serialFixture(true, ""), serialFixture(true, "")); !cs.Empty() {
		t.Errorf("self-compare must be empty, got %v", opSummary(cs))
	}
}

func fkFixture(validated bool) *model.Database {
	return &model.Database{Schemas: []*model.Schema{{Name: "public", Tables: []*model.Table{{
		Schema: "public", Name: "c", Columns: []*model.Column{{Name: "pid", Type: "integer"}},
		Constraints: []*model.Constraint{{
			Name: "c_fk", Type: model.ConstraintForeignKey, Definition: "FOREIGN KEY (pid) REFERENCES public.p(id)",
			Validated: validated, RefSchema: "public", RefTable: "p", Columns: []string{"pid"}, RefColumns: []string{"id"},
		}},
	}}}}}
}

// TestDiff_ValidatesNotValidConstraintInPlace: same body, target still NOT
// VALID -> VALIDATE CONSTRAINT; nothing to do in the other direction.
func TestDiff_ValidatesNotValidConstraintInPlace(t *testing.T) {
	cs := Diff(fkFixture(false), fkFixture(true))
	if len(cs.Ops) != 1 || cs.Ops[0].Type != OpValidateConstraint {
		t.Errorf("expected exactly one validateConstraint op, got %v", opSummary(cs))
	}
	if cs := Diff(fkFixture(true), fkFixture(false)); !cs.Empty() {
		t.Errorf("cannot un-validate; expected no ops, got %v", opSummary(cs))
	}
	if cs := Diff(fkFixture(false), fkFixture(false)); !cs.Empty() {
		t.Errorf("identical NOT VALID constraints must compare equal, got %v", opSummary(cs))
	}
}

func TestViewReplaceCompatible(t *testing.T) {
	cols := func(cs ...string) []model.ViewColumn {
		var out []model.ViewColumn
		for i := 0; i+1 < len(cs); i += 2 {
			out = append(out, model.ViewColumn{Name: cs[i], Type: cs[i+1]})
		}
		return out
	}
	v := func(cs ...string) *model.View { return &model.View{Columns: cols(cs...)} }
	cases := []struct {
		name      string
		prev, cur *model.View
		want      bool
	}{
		{"append column", v("a", "integer"), v("a", "integer", "b", "text"), true},
		{"identical", v("a", "integer"), v("a", "integer"), true},
		{"drop column", v("a", "integer", "b", "text"), v("a", "integer"), false},
		{"rename column", v("a", "integer"), v("b", "integer"), false},
		{"retype column", v("a", "integer"), v("a", "bigint"), false},
		{"reorder", v("a", "integer", "b", "text"), v("b", "text", "a", "integer"), false},
		{"unknown columns (older artifact)", &model.View{}, v("a", "integer"), true},
	}
	for _, c := range cases {
		if got := viewReplaceCompatible(c.prev, c.cur); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestFunctionRecreateReason(t *testing.T) {
	f := func(kind, ret, args string) *model.Function {
		return &model.Function{Kind: kind, ReturnType: ret, Arguments: args}
	}
	if r := functionRecreateReason(f("function", "integer", "a integer"), f("function", "integer", "a integer, b text DEFAULT 'x'::text")); r != "" {
		t.Errorf("adding an input parameter with a default is a signature change handled elsewhere, not a recreate: %q", r)
	}
	if r := functionRecreateReason(f("function", "integer", ""), f("function", "bigint", "")); r == "" {
		t.Error("return type change must force a recreate")
	}
	if r := functionRecreateReason(f("function", "void", ""), f("procedure", "", "")); r == "" {
		t.Error("function -> procedure must force a recreate")
	}
	if r := functionRecreateReason(f("function", "record", "OUT a integer, OUT b text"), f("function", "record", "OUT a bigint, OUT b text")); r == "" {
		t.Error("OUT parameter type change must force a recreate")
	}
	if r := functionRecreateReason(f("function", "", ""), f("function", "bigint", "")); r != "" {
		t.Errorf("unknown return type (older artifact) must not force a recreate: %q", r)
	}
}

// TestDiff_IndexDefinitionChangeIsRebuild: PostgreSQL cannot alter an index
// definition, so a changed index is a drop + create pair; both ops must say so
// (Replaces) so the rendered statements and hazards read as a rebuild, not as
// an index removed for good. A plain removal stays a plain drop.
func TestDiff_IndexDefinitionChangeIsRebuild(t *testing.T) {
	withIndex := func(def string) *model.Database {
		db := serialFixture(true, "")
		tbl := db.Schemas[0].Tables[0]
		if def != "" {
			tbl.Indexes = append(tbl.Indexes, &model.Index{Name: "t_a", Table: tbl.Name, Definition: def})
		}
		return db
	}
	cs := Diff(withIndex("CREATE INDEX t_a ON public.t USING btree (id)"), withIndex("CREATE INDEX t_a ON public.t USING btree (id) WHERE (id > 0)"))
	var drops, creates int
	for _, op := range cs.Ops {
		switch op.Type {
		case OpDropIndex, OpCreateIndex:
			if op.Sub != "t_a" || !op.Replaces {
				t.Errorf("%s %s: want Replaces on the rebuild pair, got %+v", op.Type, op.Sub, op.Replaces)
			}
			if op.Type == OpDropIndex {
				drops++
			} else {
				creates++
			}
		}
	}
	if drops != 1 || creates != 1 {
		t.Errorf("changed index: want one drop and one create, got %v", opSummary(cs))
	}

	cs = Diff(withIndex("CREATE INDEX t_a ON public.t USING btree (id)"), withIndex(""))
	for _, op := range cs.Ops {
		if op.Type == OpDropIndex && op.Replaces {
			t.Errorf("a removed index must not be marked as a rebuild: %v", opSummary(cs))
		}
	}
	if !hasOp(cs, OpDropIndex, "t") {
		t.Errorf("removed index: want a drop, got %v", opSummary(cs))
	}
}
