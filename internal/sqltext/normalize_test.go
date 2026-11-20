package sqltext

import "testing"

func TestNormalizeEqualModuloFormatting(t *testing.T) {
	cases := []struct {
		name string
		a, b string
	}{
		{"crlf vs lf", "SELECT 1;\r\nSELECT 2;", "SELECT 1;\nSELECT 2;"},
		{"indentation", "BEGIN\n    RETURN NEW;\nEND", "BEGIN\n\tRETURN NEW;\n\n\nEND"},
		{"single vs multi line", "SELECT * INTO r FROM t WHERE id = p_id;", "SELECT * INTO r\n    FROM t\n    WHERE id = p_id;"},
		{"trailing whitespace and END; vs END", "END;   \n$function$", "END;\n$function$"},
		{"block comment whitespace", "a /* x   y */ b", "a /*  x y  */ b"},
		{"line comment whitespace", "a -- x   y\nb", "a -- x y   \nb"},
	}
	for _, c := range cases {
		if Normalize(c.a, false) != Normalize(c.b, false) {
			t.Errorf("%s: should normalize equal:\n%q\n%q", c.name, Normalize(c.a, false), Normalize(c.b, false))
		}
	}
}

func TestNormalizeKeepsSemanticDifferences(t *testing.T) {
	cases := []struct {
		name string
		a, b string
	}{
		{"whitespace inside string literal", "SELECT 'a  b'", "SELECT 'a b'"},
		{"newline inside string literal", "SELECT 'a\nb'", "SELECT 'a b'"},
		{"whitespace inside quoted identifier", `SELECT "a  b"`, `SELECT "a b"`},
		{"dollar body verbatim", "$$SELECT  1$$", "$$SELECT 1$$"},
		{"tagged dollar body verbatim", "$fn$ a  b $fn$", "$fn$ a b $fn$"},
		{"escape string quote", `E'it\'s'  x`, `E'it\'s' y`},
		{"commented out code stays commented", "-- c\nSELECT 1", "-- c SELECT 1"},
		{"different code", "SELECT 1", "SELECT 2"},
		{"identifier case", "select a", "SELECT a"},
	}
	for _, c := range cases {
		if Normalize(c.a, false) == Normalize(c.b, false) {
			t.Errorf("%s: must NOT normalize equal: %q", c.name, Normalize(c.a, false))
		}
	}
}

func TestNormalizeDropComments(t *testing.T) {
	a := "SELECT 1; -- trailing\n/* block\n   comment */ SELECT 2;"
	b := "SELECT 1;\nSELECT 2;"
	if Normalize(a, true) != Normalize(b, true) {
		t.Errorf("comments should be ignored: %q vs %q", Normalize(a, true), Normalize(b, true))
	}
	if Normalize(a, false) == Normalize(b, false) {
		t.Error("comments must count when not ignored")
	}
	nested := "a /* outer /* inner */ still */ b"
	if got := Normalize(nested, true); got != "a b" {
		t.Errorf("nested block comment: got %q", got)
	}
	if got := Normalize("x -- only a comment", true); got != "x" {
		t.Errorf("trailing line comment: got %q", got)
	}
}

func TestNormalizeLexerEdges(t *testing.T) {
	// $1 is a parameter, not a dollar quote; foo$bar is an identifier.
	if got := Normalize("SELECT $1,   $2", false); got != "SELECT $1, $2" {
		t.Errorf("positional params: got %q", got)
	}
	if got := Normalize("SELECT foo$bar$  x", false); got != "SELECT foo$bar$ x" {
		t.Errorf("dollar inside identifier: got %q", got)
	}
	// Doubled quotes stay inside their literal/identifier.
	if got := Normalize(`SELECT 'it''s',   "a""b"`, false); got != `SELECT 'it''s', "a""b"` {
		t.Errorf("doubled quotes: got %q", got)
	}
	// Unterminated constructs do not panic and run to the end.
	for _, s := range []string{"SELECT 'open", `SELECT "open`, "$$open", "/* open", "SELECT $tag$ open"} {
		_ = Normalize(s, false)
		_ = Normalize(s, true)
	}
	// A standard string's backslash is not an escape (standard_conforming_strings).
	if got := Normalize(`SELECT 'a\'   ,   1`, false); got != `SELECT 'a\' , 1` {
		t.Errorf("backslash in standard string: got %q", got)
	}
}

// pg_get_functiondef wraps the body in $function$ quotes: for SQL and PL/pgSQL
// the body is normalized as SQL one level deep; nested dollar strings inside it
// and bodies in other languages stay verbatim.
func TestNormalizeRoutine(t *testing.T) {
	head := "CREATE OR REPLACE FUNCTION app.f()\n RETURNS integer\n LANGUAGE plpgsql\nAS $function$"
	a := head + "\nBEGIN\n    RETURN 1;\nEND\n$function$"
	b := head + "\r\nBEGIN\r\n\tRETURN 1;\r\n\r\nEND\r\n$function$"
	if !RoutineEqual(a, b, "plpgsql", false) {
		t.Errorf("plpgsql body formatting must not matter:\n%q\n%q", NormalizeRoutine(a, "plpgsql", false), NormalizeRoutine(b, "plpgsql", false))
	}
	if !RoutineEqual(a, b, "sql", false) {
		t.Error("sql body formatting must not matter")
	}
	if RoutineEqual(a, b, "plpython3u", false) {
		t.Error("a PL/Python body is code: whitespace must count")
	}
	// A literal inside the body keeps its whitespace.
	c := head + "\nBEGIN\n  RAISE NOTICE 'a  b';\nEND\n$function$"
	d := head + "\nBEGIN\n  RAISE NOTICE 'a b';\nEND\n$function$"
	if RoutineEqual(c, d, "plpgsql", false) {
		t.Error("string literal inside the body must stay verbatim")
	}
	// A nested dollar-quoted string inside the body stays verbatim.
	e := head + "\nBEGIN\n  EXECUTE $q$SELECT  1$q$;\nEND\n$function$"
	f := head + "\nBEGIN\n  EXECUTE $q$SELECT 1$q$;\nEND\n$function$"
	if RoutineEqual(e, f, "plpgsql", false) {
		t.Error("nested dollar-quoted string must stay verbatim")
	}
	// Comments inside the body are ignored only when asked.
	g := head + "\nBEGIN\n  -- note\n  RETURN 1;\nEND\n$function$"
	if RoutineEqual(a, g, "plpgsql", false) {
		t.Error("a comment inside the body counts by default")
	}
	if !RoutineEqual(a, g, "plpgsql", true) {
		t.Error("IgnoreComments must ignore a comment inside the body")
	}
}
