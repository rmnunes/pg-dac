package diff

import (
	"testing"

	"github.com/rmnunes/pg-dac/internal/model"
)

func fnModel(def string) *model.Database {
	f := &model.Function{Schema: "app", Name: "f", Kind: "function", Language: "sql", Definition: def}
	return &model.Database{TargetVersion: 160000, Schemas: []*model.Schema{{Name: "app", Functions: []*model.Function{f}}}}
}

// Routine bodies are stored verbatim by PostgreSQL, so two bodies that differ
// only by line endings or indentation are the same routine unless exact text
// comparison is asked for; comments count unless ignored.
func TestRoutineWhitespaceAndComments(t *testing.T) {
	lf := fnModel("CREATE OR REPLACE FUNCTION app.f()\n RETURNS integer\n LANGUAGE sql\nAS $function$\n  SELECT 1;\n$function$")
	crlf := fnModel("CREATE OR REPLACE FUNCTION app.f()\n RETURNS integer\n LANGUAGE sql\nAS $function$\r\n    SELECT 1;\r\n$function$")
	commented := fnModel("CREATE OR REPLACE FUNCTION app.f()\n RETURNS integer\n LANGUAGE sql\nAS $function$\n  -- one\n  SELECT 1;\n$function$")

	if cs := Diff(crlf, lf); !cs.Empty() {
		t.Errorf("whitespace-only difference must be ignored by default, got %v", opSummary(cs))
	}
	if cs := DiffWith(crlf, lf, Options{CompareWhitespace: true}); cs.Empty() {
		t.Error("CompareWhitespace must report a whitespace-only difference")
	}
	if cs := Diff(lf, commented); cs.Empty() {
		t.Error("a comment-only difference counts by default")
	}
	if cs := DiffWith(lf, commented, Options{IgnoreComments: true}); !cs.Empty() {
		t.Errorf("IgnoreComments must ignore a comment-only difference, got %v", opSummary(cs))
	}
	if cs := DiffWith(lf, commented, Options{CompareWhitespace: true, IgnoreComments: true}); !cs.Empty() {
		t.Errorf("IgnoreComments implies whitespace-insensitive comparison, got %v", opSummary(cs))
	}
	// A real body change is still a change under every option.
	changed := fnModel("CREATE OR REPLACE FUNCTION app.f()\n RETURNS integer\n LANGUAGE sql\nAS $function$\n  SELECT 2;\n$function$")
	for _, o := range []Options{{}, {IgnoreComments: true}, {CompareWhitespace: true}} {
		if cs := DiffWith(lf, changed, o); cs.Empty() {
			t.Errorf("real change must be reported under %+v", o)
		}
	}
}

// A column that gains a generator the target never had — a new identity, or a
// default drawing from a sequence this migration creates — is flagged so sqlgen
// seeds the sequence past the existing rows. A default switched to a sequence
// the target already has keeps that sequence's position and is not seeded.
func TestSeedSequenceFlags(t *testing.T) {
	table := func(col *model.Column) *model.Table {
		return &model.Table{Schema: "app", Name: "t", Columns: []*model.Column{col}}
	}
	serialSeq := &model.Sequence{Schema: "app", Name: "t_id_seq", OwnedBy: &model.ColumnRef{Schema: "app", Table: "t", Column: "id"}}
	db := func(t *model.Table, seqs ...*model.Sequence) *model.Database {
		return &model.Database{TargetVersion: 160000, Schemas: []*model.Schema{{Name: "app", Tables: []*model.Table{t}, Sequences: seqs}}}
	}
	find := func(cs *ChangeSet, typ OpType) *Op {
		for i := range cs.Ops {
			if cs.Ops[i].Type == typ {
				return &cs.Ops[i]
			}
		}
		return nil
	}

	serial := db(table(&model.Column{Name: "id", Type: "integer", NotNull: true, Default: "nextval('app.t_id_seq'::regclass)"}), serialSeq)
	identity := db(table(&model.Column{Name: "id", Type: "integer", NotNull: true, Identity: "byDefault"}),
		&model.Sequence{Schema: "app", Name: "t_id_seq", OwnedBy: &model.ColumnRef{Schema: "app", Table: "t", Column: "id"}})

	// serial -> identity: the identity sequence is new.
	cs := Diff(serial, identity)
	if op := find(cs, OpAlterColumnIdentity); op == nil || !op.SeedSequence {
		t.Fatalf("serial->identity must seed the identity sequence: %v", opSummary(cs))
	}
	if op := find(cs, OpSetColumnDefault); op != nil {
		t.Fatalf("serial->identity must not emit a separate default op: %v", opSummary(cs))
	}

	// identity -> serial: DROP IDENTITY, then the default on the explicit
	// sequence (created by this migration, replacing the identity's own).
	cs = Diff(identity, serial)
	id := find(cs, OpAlterColumnIdentity)
	def := find(cs, OpSetColumnDefault)
	seq := find(cs, OpCreateSequence)
	if id == nil || id.SeedSequence || def == nil || !def.SeedSequence || seq == nil || seq.PrevSeq == nil {
		t.Fatalf("identity->serial: want identity op (no seed), seeded default op, create-sequence with PrevSeq; got %v", opSummary(cs))
	}

	// default switched to an already existing standalone sequence: no seed.
	existing := &model.Sequence{Schema: "app", Name: "shared_seq"}
	before := db(table(&model.Column{Name: "id", Type: "integer"}), existing)
	after := db(table(&model.Column{Name: "id", Type: "integer", Default: "nextval('app.shared_seq'::regclass)"}), existing)
	cs = Diff(before, after)
	if op := find(cs, OpSetColumnDefault); op == nil || op.SeedSequence {
		t.Fatalf("default on an existing sequence must not be seeded: %v", opSummary(cs))
	}

	// default on a brand-new sequence: seeded.
	fresh := db(table(&model.Column{Name: "id", Type: "integer", Default: "nextval('app.new_seq'::regclass)"}), &model.Sequence{Schema: "app", Name: "new_seq"})
	cs = Diff(db(table(&model.Column{Name: "id", Type: "integer"})), fresh)
	if op := find(cs, OpSetColumnDefault); op == nil || !op.SeedSequence {
		t.Fatalf("default on a new sequence must be seeded: %v", opSummary(cs))
	}

	// a non-integer column is never seeded.
	textBefore := db(table(&model.Column{Name: "code", Type: "text"}))
	textAfter := db(table(&model.Column{Name: "code", Type: "text", Default: "nextval('app.new_seq'::regclass)"}), &model.Sequence{Schema: "app", Name: "new_seq"})
	if op := find(Diff(textBefore, textAfter), OpSetColumnDefault); op == nil || op.SeedSequence {
		t.Fatal("text column must not be seeded")
	}
}

func TestSequenceOfDefault(t *testing.T) {
	cases := []struct {
		def          string
		schema, name string
		ok           bool
	}{
		{"nextval('sales.reviewers_id_seq'::regclass)", "sales", "reviewers_id_seq", true},
		{"nextval('\"Sales\".\"Seq.1\"'::regclass)", "Sales", "Seq.1", true},
		{"nextval('s'::regclass)", "public", "s", true},
		{"now()", "", "", false},
		{"nextval(('x'::text)::regclass)", "", "", false},
	}
	for _, c := range cases {
		s, n, ok := sequenceOfDefault(c.def)
		if ok != c.ok || s != c.schema || n != c.name {
			t.Errorf("%s: got (%q, %q, %v) want (%q, %q, %v)", c.def, s, n, ok, c.schema, c.name, c.ok)
		}
	}
}
