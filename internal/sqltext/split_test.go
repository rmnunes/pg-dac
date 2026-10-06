package sqltext

import (
	"strings"
	"testing"
)

func TestSplitStatements(t *testing.T) {
	script := `-- leading comment
CREATE TABLE t(a text DEFAULT 'x;y', b text DEFAULT "q;uoted");
/* block; comment */ CREATE FUNCTION f() RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  RAISE NOTICE 'a;b';
  EXECUTE $q$SELECT 1; SELECT 2$q$;
END $$;
DO $body$ BEGIN PERFORM 1; END $body$
;
SELECT 1 -- trailing without semicolon
-- only a comment at the end
`
	got := Split(script)
	if len(got) != 4 {
		t.Fatalf("want 4 statements, got %d:\n%s", len(got), strings.Join(got, "\n---\n"))
	}
	if !strings.HasPrefix(got[0], "-- leading comment\nCREATE TABLE") || !strings.HasSuffix(got[0], `"q;uoted");`) {
		t.Errorf("statement 1 wrong: %q", got[0])
	}
	if !strings.HasPrefix(got[1], "/* block; comment */ CREATE FUNCTION") || !strings.Contains(got[1], "$q$SELECT 1; SELECT 2$q$") || !strings.HasSuffix(got[1], "END $$;") {
		t.Errorf("statement 2 wrong: %q", got[1])
	}
	if !strings.HasPrefix(got[2], "DO $body$") || !strings.HasSuffix(got[2], ";") {
		t.Errorf("statement 3 wrong: %q", got[2])
	}
	if got[3] != "SELECT 1 -- trailing without semicolon\n-- only a comment at the end" {
		t.Errorf("statement 4 wrong: %q", got[3])
	}
	// Spans cover the text exactly and in order.
	spans := Spans(script)
	for i := 1; i < len(spans); i++ {
		if spans[i].Start != spans[i-1].End {
			t.Errorf("spans must be contiguous: %v", spans)
		}
	}
	if Split("  -- nothing here\n/* nor here */") != nil && len(Split("  -- nothing\n")) != 0 {
		t.Error("comment-only text has no statements")
	}
	if got := Split("SELECT $1; SELECT a$b$c;"); len(got) != 2 {
		t.Errorf("positional parameters and $ inside identifiers are not dollar quotes: %v", got)
	}
}
