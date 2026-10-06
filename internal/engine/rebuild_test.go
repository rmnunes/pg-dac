package engine

import (
	"strings"
	"testing"

	"github.com/rmnunes/pg-dac/internal/model"
	"github.com/rmnunes/pg-dac/internal/safety"
)

// rebuildFixture: app.t(id identity, <order...>) with a serial-backed column
// "n", a view reading t.id, a child table with an FK to t, and a grant.
func rebuildFixture(order ...string) *model.Database {
	columns := []*model.Column{{Name: "id", Type: "integer", NotNull: true, Identity: "byDefault"}}
	for _, c := range order {
		col := &model.Column{Name: c, Type: "text"}
		if c == "n" {
			col = &model.Column{Name: "n", Type: "integer", Default: "nextval('app.t_n_seq'::regclass)"}
		}
		columns = append(columns, col)
	}
	t := &model.Table{Schema: "app", Name: "t", Columns: columns,
		Constraints: []*model.Constraint{{Name: "t_pkey", Type: model.ConstraintPrimaryKey, Definition: "PRIMARY KEY (id)", Columns: []string{"id"}, Validated: true}},
		Grants:      []model.Grant{{Grantee: "reader", Privileges: []model.Privilege{{Type: "SELECT"}}}}}
	c := &model.Table{Schema: "app", Name: "c", Columns: []*model.Column{{Name: "t_id", Type: "integer"}},
		Constraints: []*model.Constraint{{Name: "c_t_fk", Type: model.ConstraintForeignKey, Definition: "FOREIGN KEY (t_id) REFERENCES app.t(id)",
			Columns: []string{"t_id"}, RefSchema: "app", RefTable: "t", RefColumns: []string{"id"}, Validated: true}}}
	v := &model.View{Schema: "app", Name: "v", Definition: "SELECT id FROM app.t",
		DependsOn: []model.ObjRef{t.Ref()}, ColumnDeps: []model.ColumnRef{{Schema: "app", Table: "t", Column: "id"}},
		Columns: []model.ViewColumn{{Name: "id", Type: "integer"}}}
	seqs := []*model.Sequence{
		{Schema: "app", Name: "t_id_seq", OwnedBy: &model.ColumnRef{Schema: "app", Table: "t", Column: "id"}},
		{Schema: "app", Name: "t_n_seq", OwnedBy: &model.ColumnRef{Schema: "app", Table: "t", Column: "n"}},
	}
	return &model.Database{TargetVersion: 160000, Schemas: []*model.Schema{{Name: "app",
		Tables: []*model.Table{c, t}, Views: []*model.View{v}, Sequences: seqs}}}
}

// The rebuild runs as one atomic DO block (new table in the declared order,
// rows copied, serial sequences detached and re-owned, swap, identity seeded),
// bracketed by the dependents: the FK and the view are dropped before and
// recreated after, the table's own constraints and grants come back.
func TestRebuildTableScript(t *testing.T) {
	from := rebuildFixture("b", "n", "a")
	to := rebuildFixture("a", "b", "n")
	opts := DefaultOptions()
	opts.ColumnOrder = "rebuild"
	res, err := Compare(from, to, opts)
	if err != nil {
		t.Fatal(err)
	}
	script := res.Script(true)
	ordered(t, script,
		`DROP VIEW "app"."v"`,
		`DROP CONSTRAINT "c_t_fk"`,
		`CREATE TABLE "app"."t__pgdac_rebuild"`,
		`INSERT INTO "app"."t__pgdac_rebuild" ("id", "a", "b", "n") OVERRIDING SYSTEM VALUE SELECT "id", "a", "b", "n" FROM "app"."t"`,
		`ALTER SEQUENCE "app"."t_n_seq" OWNED BY NONE`,
		`DROP TABLE "app"."t"`,
		`ALTER TABLE "app"."t__pgdac_rebuild" RENAME TO "t"`,
		`ALTER SEQUENCE "app"."t_n_seq" OWNED BY "app"."t"."n"`,
		`ADD CONSTRAINT "t_pkey" PRIMARY KEY (id)`,
		`ADD CONSTRAINT "c_t_fk"`,
		`CREATE OR REPLACE VIEW "app"."v"`,
	)
	indexOfOrFail(t, script, `GRANT SELECT ON TABLE "app"."t" TO "reader";`)
	indexOfOrFail(t, script, "setval(")
	if !strings.Contains(script, "DO $pgdac$") {
		t.Fatalf("the rebuild must be one atomic DO block:\n%s", script)
	}
	var rewrite bool
	for _, s := range res.Statements {
		for _, h := range s.Hazards {
			if h.Type == safety.TableRewrite && strings.Contains(s.SQL, "t__pgdac_rebuild") {
				rewrite = true
			}
		}
	}
	if !rewrite {
		t.Fatal("the rebuild must carry a TABLE_REWRITE hazard")
	}
	if res.HasDataLoss() {
		t.Fatalf("no column is dropped: no data-loss hazard expected:\n%s", script)
	}
	if self, _ := Compare(to, to, opts); !self.Empty() {
		t.Errorf("self-compare must stay empty:\n%s", self.Script(true))
	}
}

// A column the target has and the source lacks is dropped by the rebuild only
// when the drop policy allows: otherwise it is carried over (appended), so the
// rebuild never loses data the policy protects.
func TestRebuildTableRespectsDropPolicy(t *testing.T) {
	from := rebuildFixture("b", "a", "extra")
	to := rebuildFixture("a", "b")
	opts := DefaultOptions()
	opts.ColumnOrder = "rebuild"

	res, err := Compare(from, to, opts)
	if err != nil {
		t.Fatal(err)
	}
	script := res.Script(true)
	indexOfOrFail(t, script, `"extra" text`)
	indexOfOrFail(t, script, `("id", "a", "b", "extra") OVERRIDING SYSTEM VALUE SELECT "id", "a", "b", "extra"`)
	if res.HasDataLoss() {
		t.Fatalf("a kept column is not data loss:\n%s", script)
	}

	opts.DropObjectsNotInSource = true
	res, err = Compare(from, to, opts)
	if err != nil {
		t.Fatal(err)
	}
	script = res.Script(true)
	if strings.Contains(script, `"extra"`) {
		t.Fatalf("under drop policy all the column must go:\n%s", script)
	}
	if !res.HasDataLoss() {
		t.Fatal("dropping a column through the rebuild is data loss")
	}
}
