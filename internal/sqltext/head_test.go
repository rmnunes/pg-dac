package sqltext

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseHead(t *testing.T) {
	cases := []struct {
		stmt string
		want Head
	}{
		{`CREATE TABLE sales.reviewers (id int)`, Head{Verb: "CREATE", Kind: "TABLE", Name: []string{"sales", "reviewers"}, Declarative: true}},
		{`create table if not exists "Sales"."Odd Name" (id int)`, Head{Verb: "CREATE", Kind: "TABLE", Name: []string{"Sales", "Odd Name"}, IfNotExists: true, Declarative: true}},
		{`CREATE UNLOGGED TABLE t (a int)`, Head{Verb: "CREATE", Kind: "TABLE", Name: []string{"t"}, Declarative: true}},
		{`CREATE OR REPLACE VIEW app.v AS SELECT 1`, Head{Verb: "CREATE", Kind: "VIEW", Name: []string{"app", "v"}, OrReplace: true, Declarative: true}},
		{`CREATE MATERIALIZED VIEW app.mv AS SELECT 1 WITH NO DATA`, Head{Verb: "CREATE", Kind: "MATERIALIZED VIEW", Name: []string{"app", "mv"}, Declarative: true}},
		{`CREATE OR REPLACE FUNCTION app.f(p_id text) RETURNS void LANGUAGE sql AS $$ SELECT 1 $$`, Head{Verb: "CREATE", Kind: "FUNCTION", Name: []string{"app", "f"}, OrReplace: true, Declarative: true}},
		{`CREATE PROCEDURE app.p() LANGUAGE sql AS $$ SELECT 1 $$`, Head{Verb: "CREATE", Kind: "PROCEDURE", Name: []string{"app", "p"}, Declarative: true}},
		{`CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS ix ON ONLY app.t USING btree (a)`, Head{Verb: "CREATE", Kind: "INDEX", Name: []string{"ix"}, On: []string{"app", "t"}, IfNotExists: true, Declarative: true}},
		{`CREATE TRIGGER tg BEFORE INSERT OR UPDATE ON app.t FOR EACH ROW EXECUTE FUNCTION app.tf()`, Head{Verb: "CREATE", Kind: "TRIGGER", Name: []string{"tg"}, On: []string{"app", "t"}, Declarative: true}},
		{`CREATE CONSTRAINT TRIGGER ct AFTER INSERT ON t DEFERRABLE FOR EACH ROW EXECUTE FUNCTION tf()`, Head{Verb: "CREATE", Kind: "TRIGGER", Name: []string{"ct"}, On: []string{"t"}, Declarative: true}},
		{`CREATE POLICY p ON app.t FOR SELECT USING (true)`, Head{Verb: "CREATE", Kind: "POLICY", Name: []string{"p"}, On: []string{"app", "t"}, Declarative: true}},
		{`CREATE SCHEMA IF NOT EXISTS audit`, Head{Verb: "CREATE", Kind: "SCHEMA", Name: []string{"audit"}, IfNotExists: true, Declarative: true}},
		{`CREATE TYPE app.e AS ENUM ('a')`, Head{Verb: "CREATE", Kind: "TYPE", Name: []string{"app", "e"}, Declarative: true}},
		{`CREATE EXTENSION IF NOT EXISTS citext`, Head{Verb: "CREATE", Kind: "EXTENSION", Name: []string{"citext"}, IfNotExists: true, Declarative: true}},
		{`CREATE ROLE r NOLOGIN`, Head{Verb: "CREATE", Kind: "ROLE"}},
		{`ALTER TABLE ONLY app.t ADD CONSTRAINT t_pkey PRIMARY KEY (id)`, Head{Verb: "ALTER", Kind: "TABLE", Name: []string{"app", "t"}, Declarative: true}},
		{`ALTER TABLE app.t ADD COLUMN IF NOT EXISTS c text`, Head{Verb: "ALTER", Kind: "TABLE", Name: []string{"app", "t"}}},
		{`ALTER TABLE app.t ALTER COLUMN a SET STORAGE EXTERNAL`, Head{Verb: "ALTER", Kind: "TABLE", Name: []string{"app", "t"}, Declarative: true}},
		{`ALTER TABLE app.t ALTER COLUMN a SET DEFAULT 1`, Head{Verb: "ALTER", Kind: "TABLE", Name: []string{"app", "t"}}},
		{`ALTER TABLE app.t ENABLE ROW LEVEL SECURITY`, Head{Verb: "ALTER", Kind: "TABLE", Name: []string{"app", "t"}, Declarative: true}},
		{`ALTER TABLE app.t DISABLE TRIGGER tg`, Head{Verb: "ALTER", Kind: "TABLE", Name: []string{"app", "t"}, Declarative: true}},
		{`ALTER TABLE app.t DROP COLUMN c`, Head{Verb: "ALTER", Kind: "TABLE", Name: []string{"app", "t"}}},
		{`ALTER SEQUENCE app.s OWNED BY app.t.id`, Head{Verb: "ALTER", Kind: "SEQUENCE", Name: []string{"app", "s"}, Declarative: true}},
		{`ALTER SEQUENCE app.s INCREMENT BY 2`, Head{Verb: "ALTER", Kind: "SEQUENCE", Name: []string{"app", "s"}}},
		{`COMMENT ON TABLE app.t IS 'x'`, Head{Verb: "COMMENT", Kind: "TABLE", Name: []string{"app", "t"}, Declarative: true}},
		{`COMMENT ON COLUMN app.t.c IS 'x'`, Head{Verb: "COMMENT", Kind: "COLUMN", Name: []string{"c"}, On: []string{"app", "t"}, Declarative: true}},
		{`COMMENT ON CONSTRAINT t_pkey ON app.t IS 'x'`, Head{Verb: "COMMENT", Kind: "CONSTRAINT", Name: []string{"t_pkey"}, On: []string{"app", "t"}, Declarative: true}},
		{`COMMENT ON ROUTINE app.f(integer) IS 'x'`, Head{Verb: "COMMENT", Kind: "FUNCTION", Name: []string{"app", "f"}, Declarative: true}},
		{`COMMENT ON INDEX app.ix IS 'x'`, Head{Verb: "COMMENT", Kind: "INDEX", Name: []string{"app", "ix"}, Declarative: true}},
		{`GRANT SELECT, INSERT ON TABLE app.t TO "acme.app.sql.readers"`, Head{Verb: "GRANT", Kind: "TABLE", Name: []string{"app", "t"}, Declarative: true}},
		{`GRANT SELECT ("c") ON app.t TO r`, Head{Verb: "GRANT", Kind: "TABLE", Name: []string{"app", "t"}, Declarative: true}},
		{`REVOKE GRANT OPTION FOR EXECUTE ON FUNCTION app.f(integer) FROM r`, Head{Verb: "REVOKE", Kind: "FUNCTION", Name: []string{"app", "f"}, Declarative: true}},
		{`GRANT USAGE ON SCHEMA app TO r`, Head{Verb: "GRANT", Kind: "SCHEMA", Name: []string{"app"}, Declarative: true}},
		{`GRANT SELECT ON ALL TABLES IN SCHEMA app TO r`, Head{Verb: "GRANT", Kind: "ALL"}},
		{`GRANT SELECT ON TABLE a, b TO r`, Head{Verb: "GRANT", Kind: "TABLE"}},
		{`DO $$ BEGIN NULL; END $$`, Head{Verb: "DO"}},
		{`-- comment first
INSERT INTO t VALUES (1)`, Head{Verb: "INSERT"}},
		{``, Head{}},
	}
	for _, c := range cases {
		got := ParseHead(c.stmt)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s\n got  %+v\n want %+v", c.stmt, got, c.want)
		}
	}
}

// ALTER DEFAULT PRIVILEGES names one entry when it has exactly one role and at
// most one schema; anything else is imperative.
func TestParseHeadDefaultPrivileges(t *testing.T) {
	cases := []struct {
		stmt        string
		name        []string
		declarative bool
		current     bool
	}{
		{`ALTER DEFAULT PRIVILEGES FOR ROLE app_owner IN SCHEMA app GRANT SELECT ON TABLES TO readers`, []string{"app", "app_owner", "tables"}, true, false},
		{`ALTER DEFAULT PRIVILEGES FOR ROLE "App Owner" REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC`, []string{"", "App Owner", "functions"}, true, false},
		{`alter default privileges for user o in schema "S" grant usage on sequences to r`, []string{"S", "o", "sequences"}, true, false},
		{`ALTER DEFAULT PRIVILEGES FOR ROLE o GRANT EXECUTE ON ROUTINES TO r`, []string{"", "o", "functions"}, true, false},
		{`ALTER DEFAULT PRIVILEGES IN SCHEMA app GRANT SELECT ON TABLES TO readers`, []string{"app", "", "tables"}, false, true},
		{`ALTER DEFAULT PRIVILEGES REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC`, []string{"", "", "functions"}, false, true},
		{`ALTER DEFAULT PRIVILEGES IN SCHEMA s1, s2 GRANT SELECT ON TABLES TO r`, nil, false, false},
		{`ALTER DEFAULT PRIVILEGES FOR ROLE a, b GRANT SELECT ON TABLES TO r`, nil, false, false},
		{`ALTER DEFAULT PRIVILEGES FOR ROLE a IN SCHEMA s1, s2 GRANT SELECT ON TABLES TO r`, nil, false, false},
	}
	for _, c := range cases {
		h := ParseHead(c.stmt)
		if h.Verb != "ALTER" || h.Kind != "DEFAULT PRIVILEGES" || h.Declarative != c.declarative || h.CurrentRole != c.current {
			t.Errorf("%s: verb=%q kind=%q declarative=%v current=%v", c.stmt, h.Verb, h.Kind, h.Declarative, h.CurrentRole)
		}
		if c.name != nil && strings.Join(h.Name, "|") != strings.Join(c.name, "|") {
			t.Errorf("%s: name=%q, want %q", c.stmt, h.Name, c.name)
		}
	}
}
