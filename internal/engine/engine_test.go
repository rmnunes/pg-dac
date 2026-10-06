package engine

import (
	"strings"
	"testing"

	"github.com/rmnunes/pg-dac/internal/model"
)

// buildDesired constructs a desired database: a new schema "app" containing an
// enum, two tables joined by a foreign key, an index, and a view over a table.
func buildDesired() *model.Database {
	users := &model.Table{
		Schema: "app", Name: "users",
		Columns: []*model.Column{
			{Name: "id", Type: "bigint", NotNull: true, Identity: "always"},
			{Name: "status", Type: "app.status", NotNull: true},
		},
		Constraints: []*model.Constraint{
			{Name: "users_pkey", Type: model.ConstraintPrimaryKey, Definition: `PRIMARY KEY ("id")`, Columns: []string{"id"}, Validated: true},
		},
	}
	orders := &model.Table{
		Schema: "app", Name: "orders",
		Columns: []*model.Column{
			{Name: "id", Type: "bigint", NotNull: true, Identity: "always"},
			{Name: "user_id", Type: "bigint", NotNull: true},
		},
		Constraints: []*model.Constraint{
			{Name: "orders_pkey", Type: model.ConstraintPrimaryKey, Definition: `PRIMARY KEY ("id")`, Columns: []string{"id"}, Validated: true},
			{
				Name: "orders_user_fk", Type: model.ConstraintForeignKey,
				Definition: `FOREIGN KEY ("user_id") REFERENCES "app"."users" ("id")`,
				Columns:    []string{"user_id"}, RefSchema: "app", RefTable: "users", RefColumns: []string{"id"}, Validated: true,
			},
		},
		Indexes: []*model.Index{
			{Name: "orders_user_id_idx", Table: "orders", Definition: `CREATE INDEX "orders_user_id_idx" ON "app"."orders" USING btree ("user_id")`},
		},
	}
	view := &model.View{
		Schema: "app", Name: "active_users",
		Definition: `SELECT id FROM "app"."users"`,
		DependsOn:  []model.ObjRef{{Kind: model.KindTable, Schema: "app", Name: "users"}},
	}
	return &model.Database{
		TargetVersion: 160004,
		Schemas: []*model.Schema{
			{Name: "public"},
			{
				Name:   "app",
				Enums:  []*model.EnumType{{Schema: "app", Name: "status", Values: []string{"active", "inactive"}}},
				Tables: []*model.Table{users, orders},
				Views:  []*model.View{view},
			},
		},
	}
}

func indexOf(t *testing.T, script, needle string) int {
	t.Helper()
	i := strings.Index(script, needle)
	if i < 0 {
		t.Fatalf("expected script to contain %q\n---\n%s", needle, script)
	}
	return i
}

func TestCompare_DependencyOrdering(t *testing.T) {
	from := &model.Database{TargetVersion: 160004, Schemas: []*model.Schema{{Name: "public"}}}
	to := buildDesired()

	res, err := Compare(from, to, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	script := res.Script(true)

	createSchema := indexOf(t, script, `CREATE SCHEMA "app"`)
	createEnum := indexOf(t, script, `CREATE TYPE "app"."status"`)
	createUsers := indexOf(t, script, `CREATE TABLE "app"."users"`)
	createOrders := indexOf(t, script, `CREATE TABLE "app"."orders"`)
	addFK := indexOf(t, script, "FOREIGN KEY")
	createIndex := indexOf(t, script, `"orders_user_id_idx"`)
	createView := indexOf(t, script, `CREATE OR REPLACE VIEW "app"."active_users"`)

	// schema before its objects
	if !(createSchema < createEnum && createSchema < createUsers) {
		t.Errorf("schema must be created before its objects")
	}
	// enum before the table that uses it
	if !(createEnum < createUsers) {
		t.Errorf("enum (%d) must precede users table (%d)", createEnum, createUsers)
	}
	// foreign key after BOTH referenced and referencing tables
	if !(createUsers < addFK && createOrders < addFK) {
		t.Errorf("FK (%d) must come after users (%d) and orders (%d)", addFK, createUsers, createOrders)
	}
	// online FK staging: NOT VALID then VALIDATE
	if !strings.Contains(script, "NOT VALID") || !strings.Contains(script, "VALIDATE CONSTRAINT") {
		t.Errorf("online FK should be staged NOT VALID then validated:\n%s", script)
	}
	// index uses CONCURRENTLY in online mode and is non-transactional
	if !strings.Contains(script, "CREATE INDEX CONCURRENTLY") {
		t.Errorf("online index build should be CONCURRENTLY:\n%s", script)
	}
	_ = createIndex
	// view comes after the table it depends on
	if !(createUsers < createView) {
		t.Errorf("view (%d) must come after users (%d)", createView, createUsers)
	}
}

func TestCompare_FunctionsTriggersPolicies(t *testing.T) {
	from := &model.Database{TargetVersion: 160004, Schemas: []*model.Schema{{Name: "public"}}}
	to := &model.Database{TargetVersion: 160004, Schemas: []*model.Schema{
		{Name: "public"},
		{
			Name: "app",
			Functions: []*model.Function{{
				Schema: "app", Name: "touch", Kind: "function", IdentityArgs: "",
				Definition: "CREATE OR REPLACE FUNCTION app.touch() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$",
			}},
			Tables: []*model.Table{{
				Schema: "app", Name: "t", RLSEnabled: true,
				Columns: []*model.Column{{Name: "id", Type: "bigint", NotNull: true}, {Name: "tenant", Type: "text"}},
				Triggers: []*model.Trigger{{
					Name: "t_touch", Table: "t",
					Definition: "CREATE TRIGGER t_touch BEFORE UPDATE ON app.t FOR EACH ROW EXECUTE FUNCTION app.touch()",
				}},
				Policies: []*model.Policy{{
					Name: "p", Table: "t", Permissive: true, Command: "ALL", Using: "tenant = current_user",
				}},
			}},
		},
	}}

	res, err := Compare(from, to, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	script := res.Script(true)

	fn := indexOf(t, script, "CREATE OR REPLACE FUNCTION app.touch()")
	tbl := indexOf(t, script, `CREATE TABLE "app"."t"`)
	rls := indexOf(t, script, "ENABLE ROW LEVEL SECURITY")
	trig := indexOf(t, script, "CREATE TRIGGER t_touch")
	pol := indexOf(t, script, "CREATE POLICY")

	if !(fn < tbl) {
		t.Errorf("function (%d) should be created before the table (%d)", fn, tbl)
	}
	if !(tbl < trig && fn < trig) {
		t.Errorf("trigger (%d) must follow table (%d) and function (%d)", trig, tbl, fn)
	}
	if !(tbl < pol && tbl < rls) {
		t.Errorf("policy/RLS must follow the table")
	}
}

func TestCompare_SerialToIdentity(t *testing.T) {
	// A legacy serial column (DEFAULT nextval) converging to an identity column —
	// the conversion seen on a real project. The migration must drop the default
	// AND add the identity, not merely drop the default.
	col := func(c *model.Column) *model.Database {
		return &model.Database{TargetVersion: 160004, Schemas: []*model.Schema{{
			Name:   "sales",
			Tables: []*model.Table{{Schema: "sales", Name: "reviewers", Columns: []*model.Column{c}}},
		}}}
	}
	from := col(&model.Column{Name: "id", Type: "integer", NotNull: true, Default: "nextval('sales.reviewers_id_seq'::regclass)"})
	to := col(&model.Column{Name: "id", Type: "integer", NotNull: true, Identity: "byDefault"})

	res, err := Compare(from, to, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	script := res.Script(true)
	dropDef := indexOf(t, script, "DROP DEFAULT")
	addIdent := indexOf(t, script, "ADD GENERATED BY DEFAULT AS IDENTITY")
	if !(dropDef < addIdent) {
		t.Errorf("DROP DEFAULT (%d) must precede ADD ... IDENTITY (%d)", dropDef, addIdent)
	}
}

func TestPlanDoc_StructureAndIncludeExclude(t *testing.T) {
	from := &model.Database{TargetVersion: 160004, Schemas: []*model.Schema{{Name: "public"}}}
	to := buildDesired() // app schema: enum, users, orders (FK->users), index, view

	res, err := Compare(from, to, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	doc := res.PlanDoc()
	if doc.FormatVersion != PlanFormatVersion {
		t.Errorf("format version: got %d", doc.FormatVersion)
	}

	byID := map[string]ObjectChange{}
	for _, c := range doc.Changes {
		byID[c.ID] = c
	}

	usersID := (&model.Table{Schema: "app", Name: "users"}).Ref().Key()
	ordersID := (&model.Table{Schema: "app", Name: "orders"}).Ref().Key()
	users, ok := byID[usersID]
	if !ok {
		t.Fatalf("expected a change for app.users; got changes: %v", keysOf(doc.Changes))
	}
	// Sub-object ops (PK, unique) roll up under the table, and the table is new.
	if users.ChangeKind != "add" {
		t.Errorf("users changeKind: got %q want add", users.ChangeKind)
	}
	if users.NewDefinition == "" || !strings.Contains(users.NewDefinition, "CREATE TABLE") {
		t.Errorf("users should have a NewDefinition containing CREATE TABLE:\n%s", users.NewDefinition)
	}
	if len(users.Statements) == 0 {
		t.Errorf("users should own its CREATE/constraint statements")
	}

	// orders depends on users (foreign key) — drives the GUI's include/exclude gating.
	orders := byID[ordersID]
	if !containsStr(orders.DependsOn, usersID) {
		t.Errorf("orders should depend on users; got %v", orders.DependsOn)
	}

	// Excluding users by ID removes the users change from the plan.
	opts := DefaultOptions()
	opts.ExcludeIDs = map[string]bool{usersID: true}
	exres, err := Compare(from, to, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range exres.PlanDoc().Changes {
		if c.ID == usersID {
			t.Errorf("users should have been excluded from the plan")
		}
	}
}

func containsStr(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func keysOf(cs []ObjectChange) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.ID
	}
	return out
}

func TestCompare_CollationBeforeTable(t *testing.T) {
	from := &model.Database{TargetVersion: 160004, Schemas: []*model.Schema{{Name: "public"}}}
	to := &model.Database{TargetVersion: 160004, Schemas: []*model.Schema{{
		Name: "public",
		Collations: []*model.Collation{{
			Schema: "public", Name: "ci",
			Definition: `CREATE COLLATION "public"."ci" (provider = icu, locale = 'und-u-ks-level2', deterministic = false)`,
		}},
		Tables: []*model.Table{{
			Schema: "public", Name: "t",
			Columns: []*model.Column{{Name: "email", Type: "text", Collation: `"public"."ci"`}},
		}},
	}}}

	res, err := Compare(from, to, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	script := res.Script(true)
	cc := indexOf(t, script, "CREATE COLLATION")
	ct := indexOf(t, script, `CREATE TABLE "public"."t"`)
	if !(cc < ct) {
		t.Errorf("collation (%d) must be created before the table that uses it (%d)", cc, ct)
	}
	// idempotent
	again, _ := Compare(to, to, DefaultOptions())
	if !again.Empty() {
		t.Errorf("comparing the collation model to itself must be empty")
	}
}

func TestCompare_Idempotent(t *testing.T) {
	to := buildDesired()
	res, err := Compare(to, to, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Empty() {
		t.Fatalf("comparing a model against itself must be empty, got %d statements:\n%s",
			len(res.Statements), res.Script(true))
	}
}

func TestCompare_DropColumnDataLossGate(t *testing.T) {
	from := &model.Database{TargetVersion: 160004, Schemas: []*model.Schema{{
		Name: "public",
		Tables: []*model.Table{{
			Schema: "public", Name: "t",
			Columns: []*model.Column{
				{Name: "id", Type: "bigint", NotNull: true},
				{Name: "obsolete", Type: "text"},
			},
		}},
	}}}
	to := &model.Database{TargetVersion: 160004, Schemas: []*model.Schema{{
		Name: "public",
		Tables: []*model.Table{{
			Schema: "public", Name: "t",
			Columns: []*model.Column{{Name: "id", Type: "bigint", NotNull: true}},
		}},
	}}}

	// Default: drops not in source are NOT applied -> no statements.
	def, err := Compare(from, to, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !def.Empty() {
		t.Errorf("default options should not drop a column missing from source, got:\n%s", def.Script(true))
	}

	// With DropObjectsNotInSource: a drop is emitted and flagged as data loss.
	opts := DefaultOptions()
	opts.DropObjectsNotInSource = true
	res, err := Compare(from, to, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !res.HasDataLoss() {
		t.Errorf("dropping a column must be flagged as data loss")
	}
	// Without --allow-hazards the drop is blocked (commented out) in the script.
	if !strings.Contains(res.Script(false), "BLOCKED") {
		t.Errorf("data-loss statement should be blocked by default:\n%s", res.Script(false))
	}
	// With --allow-hazards the real DROP COLUMN appears.
	if !strings.Contains(res.Script(true), "DROP COLUMN") {
		t.Errorf("with --allow-hazards the DROP COLUMN should be present:\n%s", res.Script(true))
	}
}
