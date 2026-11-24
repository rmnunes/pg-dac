package sqltext

import "strings"

// Head is what the leading clause of a statement says about the object it
// defines or annotates: enough to attribute every statement of a project file
// to an owning object and to tell declarative definitions from imperative
// migration steps.
type Head struct {
	// Verb is the first keyword: CREATE, ALTER, COMMENT, GRANT, REVOKE, DROP,
	// DO, INSERT, ... (upper case; "" for an empty statement).
	Verb string
	// Kind is the object kind the statement names: TABLE, VIEW, MATERIALIZED
	// VIEW, FUNCTION, PROCEDURE, SEQUENCE, TYPE, DOMAIN, SCHEMA, EXTENSION,
	// COLLATION, INDEX, TRIGGER, POLICY, CONSTRAINT, COLUMN, DEFAULT
	// PRIVILEGES; "" when unknown. For DEFAULT PRIVILEGES, Name is the schema
	// ("" for database-wide), the role and the object type (tables, sequences,
	// functions, types, schemas).
	Kind string
	// Name is the object's qualified name parts, unquoted. For a sub-object
	// (INDEX, TRIGGER, POLICY, CONSTRAINT, COLUMN) it is the sub-object's own
	// name and On names the relation it belongs to.
	Name []string
	On   []string
	// IfNotExists / OrReplace record those clauses.
	IfNotExists bool
	OrReplace   bool
	// Declarative is true when the statement belongs to the vocabulary pg-dac
	// itself renders for an object from scratch — a CREATE, an added constraint,
	// an index, a trigger, a policy, a column storage attribute, row-level
	// security state, a sequence's OWNED BY, a comment, a grant — and therefore
	// can be replaced by a fresh rendering. Imperative migration steps (ADD
	// COLUMN, DROP, RENAME, DO blocks, DML, CREATE ROLE, GRANT ON ALL TABLES
	// IN SCHEMA, ...) are not.
	Declarative bool
	// CurrentRole marks ALTER DEFAULT PRIVILEGES without FOR ROLE that sets
	// one entry: the entry of whichever role runs it (Name[1] is ""). It is
	// declarative where that role is known — a project's owner.
	CurrentRole bool
}

// ParseHead reads the leading clause of one statement.
func ParseHead(stmt string) Head {
	toks := lex(stmt, 48)
	var h Head
	if len(toks) == 0 {
		return h
	}
	h.Verb = upper(toks[0])
	i := 1
	switch h.Verb {
	case "CREATE":
		parseCreate(&h, toks, i)
	case "ALTER":
		parseAlter(&h, toks, i)
	case "COMMENT":
		parseComment(&h, toks, i)
	case "GRANT", "REVOKE":
		parseGrant(&h, toks, i)
	}
	return h
}

// token is a lexical item: a word (keyword or unquoted identifier), a quoted
// identifier, punctuation or a literal. Quoted identifiers keep their exact
// text; words are compared case-insensitively.
type token struct {
	text   string
	quoted bool
	punct  bool
}

func upper(t token) string {
	if t.quoted || t.punct {
		return t.text
	}
	return strings.ToUpper(t.text)
}

// lex returns up to max tokens of stmt, skipping whitespace and comments.
func lex(s string, max int) []token {
	var out []token
	i, n := 0, len(s)
	for i < n && len(out) < max {
		c := s[i]
		switch {
		case isSpace(c):
			i++
		case c == '-' && i+1 < n && s[i+1] == '-':
			for i < n && s[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && s[i+1] == '*':
			i = blockCommentEnd(s, i)
		case c == '"':
			end := quotedIdentEnd(s, i)
			body := s[i+1 : end-1]
			if end-1 < i+1 {
				body = ""
			}
			out = append(out, token{text: strings.ReplaceAll(body, `""`, `"`), quoted: true})
			i = end
		case c == '\'':
			end := stringEnd(s, i, false)
			out = append(out, token{text: s[i:end], punct: true})
			i = end
		case c == '$':
			if tagEnd, ok := dollarTag(s, i); ok && (i == 0 || !isIdentChar(s[i-1])) {
				tag := s[i:tagEnd]
				end := n
				if close := indexFrom(s, tag, tagEnd); close >= 0 {
					end = close + len(tag)
				}
				out = append(out, token{text: s[i:end], punct: true})
				i = end
				break
			}
			i++
		case isIdentChar(c):
			j := i
			for j < n && (isIdentChar(s[j]) || s[j] == '.') && s[j] != '.' {
				j++
			}
			out = append(out, token{text: s[i:j]})
			i = j
		default:
			out = append(out, token{text: string(c), punct: true})
			i++
		}
	}
	return out
}

// qualifiedName reads name parts separated by dots starting at i and returns
// the parts and the index after them.
func qualifiedName(toks []token, i int) ([]string, int) {
	var parts []string
	for i < len(toks) {
		t := toks[i]
		if t.punct && !t.quoted {
			break
		}
		parts = append(parts, t.text)
		i++
		if i < len(toks) && toks[i].punct && toks[i].text == "." {
			i++
			continue
		}
		break
	}
	return parts, i
}

func is(toks []token, i int, words ...string) bool {
	for k, w := range words {
		if i+k >= len(toks) || toks[i+k].quoted || upper(toks[i+k]) != w {
			return false
		}
	}
	return true
}

func parseCreate(h *Head, toks []token, i int) {
	if is(toks, i, "OR", "REPLACE") {
		h.OrReplace = true
		i += 2
	}
	// Persistence / uniqueness / constraint modifiers.
	for is(toks, i, "TEMP") || is(toks, i, "TEMPORARY") || is(toks, i, "UNLOGGED") || is(toks, i, "UNIQUE") || is(toks, i, "CONSTRAINT") || is(toks, i, "RECURSIVE") {
		i++
	}
	if is(toks, i, "MATERIALIZED", "VIEW") {
		h.Kind = "MATERIALIZED VIEW"
		i += 2
	} else if i < len(toks) {
		h.Kind = upper(toks[i])
		i++
	}
	if is(toks, i, "CONCURRENTLY") {
		i++
	}
	if is(toks, i, "IF", "NOT", "EXISTS") {
		h.IfNotExists = true
		i += 3
	}
	switch h.Kind {
	case "TABLE", "VIEW", "MATERIALIZED VIEW", "FUNCTION", "PROCEDURE", "SEQUENCE", "TYPE", "DOMAIN", "EXTENSION", "COLLATION":
		h.Name, _ = qualifiedName(toks, i)
		h.Declarative = len(h.Name) > 0
	case "SCHEMA":
		if is(toks, i, "AUTHORIZATION") {
			i++
		}
		h.Name, _ = qualifiedName(toks, i)
		h.Declarative = len(h.Name) > 0
	case "INDEX":
		h.Name, i = qualifiedName(toks, i)
		if is(toks, i, "ON") {
			i++
			if is(toks, i, "ONLY") {
				i++
			}
			h.On, _ = qualifiedName(toks, i)
		}
		h.Declarative = len(h.On) > 0
	case "TRIGGER", "POLICY":
		h.Name, i = qualifiedName(toks, i)
		for i < len(toks) && !is(toks, i, "ON") {
			i++
		}
		if is(toks, i, "ON") {
			i++
			h.On, _ = qualifiedName(toks, i)
		}
		h.Declarative = len(h.On) > 0
	}
}

func parseAlter(h *Head, toks []token, i int) {
	switch {
	case is(toks, i, "DEFAULT", "PRIVILEGES"):
		parseDefaultPrivileges(h, toks, i+2)
	case is(toks, i, "TABLE"):
		h.Kind = "TABLE"
		i++
		if is(toks, i, "IF", "EXISTS") {
			i += 2
		}
		if is(toks, i, "ONLY") {
			i++
		}
		h.Name, i = qualifiedName(toks, i)
		switch {
		case is(toks, i, "ADD", "CONSTRAINT"),
			is(toks, i, "ENABLE", "ROW", "LEVEL", "SECURITY"), is(toks, i, "DISABLE", "ROW", "LEVEL", "SECURITY"),
			is(toks, i, "FORCE", "ROW", "LEVEL", "SECURITY"), is(toks, i, "NO", "FORCE", "ROW", "LEVEL", "SECURITY"),
			is(toks, i, "ENABLE", "TRIGGER"), is(toks, i, "DISABLE", "TRIGGER"),
			is(toks, i, "ENABLE", "ALWAYS", "TRIGGER"), is(toks, i, "ENABLE", "REPLICA", "TRIGGER"),
			is(toks, i, "SET", "("), is(toks, i, "RESET", "("):
			h.Declarative = true
		case is(toks, i, "ALTER", "COLUMN") || is(toks, i, "ALTER"):
			j := i + 1
			if is(toks, j, "COLUMN") {
				j++
			}
			_, j = qualifiedName(toks, j)
			h.Declarative = is(toks, j, "SET", "STORAGE") || is(toks, j, "SET", "STATISTICS") || is(toks, j, "SET", "COMPRESSION")
		}
	case is(toks, i, "SEQUENCE"):
		h.Kind = "SEQUENCE"
		i++
		if is(toks, i, "IF", "EXISTS") {
			i += 2
		}
		h.Name, i = qualifiedName(toks, i)
		h.Declarative = is(toks, i, "OWNED", "BY")
	case is(toks, i, "MATERIALIZED", "VIEW"):
		h.Kind = "MATERIALIZED VIEW"
		h.Name, _ = qualifiedName(toks, i+2)
	case is(toks, i, "VIEW"), is(toks, i, "FUNCTION"), is(toks, i, "PROCEDURE"), is(toks, i, "TYPE"), is(toks, i, "DOMAIN"), is(toks, i, "SCHEMA"), is(toks, i, "INDEX"), is(toks, i, "EXTENSION"), is(toks, i, "COLLATION"):
		h.Kind = upper(toks[i])
		i++
		if is(toks, i, "IF", "EXISTS") {
			i += 2
		}
		h.Name, _ = qualifiedName(toks, i)
	}
}

func parseComment(h *Head, toks []token, i int) {
	if !is(toks, i, "ON") {
		return
	}
	i++
	switch {
	case is(toks, i, "MATERIALIZED", "VIEW"):
		h.Kind = "MATERIALIZED VIEW"
		i += 2
	default:
		if i < len(toks) {
			h.Kind = upper(toks[i])
			i++
		}
	}
	switch h.Kind {
	case "COLUMN":
		// COMMENT ON COLUMN [schema.]table.column
		parts, _ := qualifiedName(toks, i)
		if len(parts) >= 2 {
			h.On, h.Name = parts[:len(parts)-1], parts[len(parts)-1:]
		}
	case "CONSTRAINT", "TRIGGER", "POLICY", "RULE":
		h.Name, i = qualifiedName(toks, i)
		if is(toks, i, "ON") {
			if is(toks, i+1, "DOMAIN") {
				i++
				h.Kind = "DOMAIN CONSTRAINT"
			}
			h.On, _ = qualifiedName(toks, i+1)
		}
	case "ROUTINE":
		h.Kind = "FUNCTION"
		h.Name, _ = qualifiedName(toks, i)
	default:
		h.Name, _ = qualifiedName(toks, i)
	}
	h.Declarative = len(h.Name) > 0 || len(h.On) > 0
}

func parseGrant(h *Head, toks []token, i int) {
	if is(toks, i, "GRANT", "OPTION", "FOR") {
		i += 3
	}
	// Skip the privilege list up to ON.
	for i < len(toks) && !is(toks, i, "ON") {
		i++
	}
	if !is(toks, i, "ON") {
		return
	}
	i++
	switch {
	case is(toks, i, "ALL"):
		h.Kind = "ALL"
		return // GRANT ... ON ALL TABLES IN SCHEMA: not one object
	case is(toks, i, "MATERIALIZED", "VIEW"):
		h.Kind = "MATERIALIZED VIEW"
		i += 2
	case is(toks, i, "TABLE"), is(toks, i, "SEQUENCE"), is(toks, i, "FUNCTION"), is(toks, i, "PROCEDURE"), is(toks, i, "ROUTINE"),
		is(toks, i, "SCHEMA"), is(toks, i, "TYPE"), is(toks, i, "DOMAIN"), is(toks, i, "DATABASE"), is(toks, i, "LANGUAGE"),
		is(toks, i, "TABLESPACE"), is(toks, i, "FOREIGN"), is(toks, i, "LARGE"), is(toks, i, "PARAMETER"):
		h.Kind = upper(toks[i])
		if h.Kind == "ROUTINE" {
			h.Kind = "FUNCTION"
		}
		i++
	default:
		h.Kind = "TABLE"
	}
	h.Name, i = qualifiedName(toks, i)
	// Several objects in one GRANT cannot be attributed to a single owner.
	if is(toks, i, ",") {
		h.Declarative = false
		h.Name = nil
		return
	}
	switch h.Kind {
	case "TABLE", "SEQUENCE", "FUNCTION", "PROCEDURE", "SCHEMA", "TYPE", "DOMAIN", "MATERIALIZED VIEW":
		h.Declarative = len(h.Name) > 0
	}
}

// defaultPrivilegeTypes maps the object-type word of ALTER DEFAULT PRIVILEGES
// to the name pg-dac models it under (ROUTINES is FUNCTIONS).
var defaultPrivilegeTypes = map[string]string{
	"TABLES": "tables", "SEQUENCES": "sequences", "FUNCTIONS": "functions", "ROUTINES": "functions",
	"TYPES": "types", "SCHEMAS": "schemas",
}

// parseDefaultPrivileges reads ALTER DEFAULT PRIVILEGES [FOR ROLE r] [IN
// SCHEMA s] GRANT|REVOKE ... ON <type> .... It is declarative — one entry pg-dac
// renders itself — when it names exactly one role and at most one schema;
// without FOR ROLE the entry belongs to whoever runs it, and a list of roles
// or schemas sets several entries at once.
func parseDefaultPrivileges(h *Head, toks []token, i int) {
	h.Kind = "DEFAULT PRIVILEGES"
	single := true
	role, schema := "", ""
	if is(toks, i, "FOR", "ROLE") || is(toks, i, "FOR", "USER") {
		i += 2
		if i < len(toks) {
			role = toks[i].text
			i++
		}
		if i < len(toks) && toks[i].punct && toks[i].text == "," {
			single = false
		}
	}
	for i < len(toks) && !is(toks, i, "IN", "SCHEMA") && !is(toks, i, "GRANT") && !is(toks, i, "REVOKE") {
		i++
	}
	if is(toks, i, "IN", "SCHEMA") {
		i += 2
		if i < len(toks) {
			schema = toks[i].text
			i++
		}
		if i < len(toks) && toks[i].punct && toks[i].text == "," {
			single = false
		}
	}
	objtype := ""
	for ; i < len(toks); i++ {
		if is(toks, i, "ON") && i+1 < len(toks) {
			objtype = defaultPrivilegeTypes[upper(toks[i+1])]
			break
		}
	}
	h.Name = []string{schema, role, objtype}
	h.Declarative = single && role != "" && objtype != ""
	h.CurrentRole = single && role == "" && objtype != ""
}
