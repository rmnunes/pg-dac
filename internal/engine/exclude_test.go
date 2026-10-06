package engine

import (
	"strings"
	"testing"

	"github.com/rmnunes/pg-dac/internal/model"
)

// Excluding an object type removes every operation of that kind — top-level
// objects by their kind, sub-objects (indexes, constraints, triggers, policies,
// privileges, comments) by theirs — and nothing else.
func TestExcludeObjectTypes(t *testing.T) {
	from := privFixture(nil, nil)
	to := privFixture([]model.Grant{sel("reader", false)}, nil)
	to.Schemas[0].Tables[0].Indexes = []*model.Index{{Name: "t_a", Table: "t", Definition: `CREATE INDEX t_a ON app.t USING btree (a)`}}
	to.Schemas[0].Tables[0].Comment = "a table"
	to.Schemas[0].Tables[0].Columns = append(to.Schemas[0].Tables[0].Columns, &model.Column{Name: "c", Type: "text"})
	to.Schemas[0].Views[0].Comment = "a view"

	all, err := Compare(from, to, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	script := all.Script(true)
	for _, want := range []string{"GRANT SELECT", "CREATE INDEX", `COMMENT ON TABLE`, `COMMENT ON VIEW`, `ADD COLUMN "c"`} {
		if !strings.Contains(script, want) {
			t.Fatalf("baseline script lacks %q:\n%s", want, script)
		}
	}

	opts := DefaultOptions()
	opts.ExcludeObjectTypes = []string{"index", "privilege", "comment"}
	res, err := Compare(from, to, opts)
	if err != nil {
		t.Fatal(err)
	}
	script = res.Script(true)
	for _, gone := range []string{"GRANT", "CREATE INDEX", "COMMENT ON"} {
		if strings.Contains(script, gone) {
			t.Errorf("excluded type still present (%s):\n%s", gone, script)
		}
	}
	if !strings.Contains(script, `ADD COLUMN "c"`) {
		t.Errorf("the table change itself must survive:\n%s", script)
	}

	opts.ExcludeObjectTypes = []string{"view"}
	res, err = Compare(from, to, opts)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Script(true), `COMMENT ON VIEW`) || !strings.Contains(res.Script(true), `COMMENT ON TABLE`) {
		t.Errorf("excluding views must drop the view's ops only:\n%s", res.Script(true))
	}
}

// --single-transaction wraps transactional runs in BEGIN/COMMIT and leaves
// CONCURRENTLY statements between them.
func TestScriptSingleTransaction(t *testing.T) {
	from := privFixture(nil, nil)
	to := privFixture(nil, nil)
	to.Schemas[0].Tables[0].Indexes = []*model.Index{{Name: "t_a", Table: "t", Definition: `CREATE INDEX t_a ON app.t USING btree (a)`}}
	to.Schemas[0].Tables[0].Columns = append(to.Schemas[0].Tables[0].Columns, &model.Column{Name: "c", Type: "text"})
	res, err := Compare(from, to, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	s := res.ScriptWith(ScriptOptions{AllowHazards: true, SingleTransaction: true})
	begin, commit, idx := strings.Index(s, "BEGIN;"), strings.Index(s, "COMMIT;"), strings.Index(s, "CREATE INDEX CONCURRENTLY")
	if begin < 0 || commit < 0 || idx < 0 || !(begin < commit && commit < idx) {
		t.Fatalf("expected BEGIN ... COMMIT before the CONCURRENTLY statement:\n%s", s)
	}
	if strings.Contains(res.Script(true), "BEGIN;") {
		t.Fatal("the plain script must not open a transaction")
	}
}
