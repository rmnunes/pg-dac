// Package sqltext normalizes SQL text for comparison. PostgreSQL stores routine
// bodies verbatim (pg_get_functiondef returns the body exactly as written), so
// the "normalize through a real database" strategy that makes every deparsed
// definition canonical does nothing for them: the same function deployed from
// a Windows checkout (CRLF) or reformatted by an editor would compare as
// changed. Normalize gives both sides a formatting-free form to compare while
// leaving the text that is deployed untouched.
package sqltext

import "strings"

// Normalize returns a whitespace-insensitive form of sql: outside string
// literals, quoted identifiers and dollar-quoted bodies, every run of
// whitespace (including line endings) collapses to a single space, and the
// result is trimmed. Literal contents are copied verbatim, so two texts
// normalize equal only when they differ by formatting alone. With dropComments,
// -- and nested /* */ comments are removed as well (SSDT's IgnoreComments);
// otherwise a comment is kept with its own whitespace collapsed, and a line
// comment keeps the line break that ends it, so code commented out on the same
// line never compares equal to code that runs.
//
// The lexer follows PostgreSQL's rules for the constructs that matter here:
// ” doubling inside standard strings, backslash escapes in E” strings, ""
// doubling inside quoted identifiers, $tag$ ... $tag$ bodies (a tag cannot
// start with a digit, so $1 is a parameter, and a $ inside an identifier such
// as foo$bar is not a quote), and nested block comments.
func Normalize(sql string, dropComments bool) string { return normalize(sql, dropComments, 0) }

// NormalizeRoutine normalizes a CREATE FUNCTION / PROCEDURE statement as
// pg_get_functiondef renders it: the header is plain SQL, and the body is a
// dollar-quoted string. For languages whose whitespace is insignificant — SQL
// and PL/pgSQL — the body is normalized one level deep as SQL too, so an
// indentation or line-ending change inside it is not a change. Dollar-quoted
// strings nested inside the body (dynamic SQL, literals) stay verbatim, and so
// does the whole body for every other language (PL/Python indentation is
// code).
func NormalizeRoutine(def, language string, dropComments bool) string {
	switch strings.ToLower(language) {
	case "sql", "plpgsql":
		return normalize(def, dropComments, 1)
	}
	return normalize(def, dropComments, 0)
}

// RoutineEqual reports whether two routine definitions are the same modulo
// formatting (see NormalizeRoutine) and, optionally, comments.
func RoutineEqual(a, b, language string, ignoreComments bool) bool {
	return a == b || NormalizeRoutine(a, language, ignoreComments) == NormalizeRoutine(b, language, ignoreComments)
}

// normalize is Normalize with a budget of dollar-quoted nesting levels whose
// contents are themselves normalized as SQL (0 = all dollar bodies verbatim).
func normalize(sql string, dropComments bool, bodyDepth int) string {
	var out strings.Builder
	out.Grow(len(sql))
	pending := false // a whitespace run was seen; emit one space before the next token
	prevIdent := false

	emit := func(s string) {
		if pending && out.Len() > 0 {
			out.WriteByte(' ')
		}
		pending = false
		out.WriteString(s)
	}

	i := 0
	n := len(sql)
	for i < n {
		c := sql[i]
		switch {
		case isSpace(c):
			pending = true
			prevIdent = false
			i++

		case c == '-' && i+1 < n && sql[i+1] == '-':
			end := i + 2
			for end < n && sql[end] != '\n' {
				end++
			}
			if !dropComments {
				emit("--" + collapse(strings.TrimRight(sql[i+2:end], " \t\r")))
				out.WriteByte('\n')
			}
			pending = dropComments
			prevIdent = false
			i = end

		case c == '/' && i+1 < n && sql[i+1] == '*':
			end := blockCommentEnd(sql, i)
			if !dropComments {
				emit("/*" + collapse(sql[i+2:max(i+2, end-2)]) + "*/")
			}
			pending = dropComments
			prevIdent = false
			i = end

		case c == '\'':
			escape := i > 0 && (sql[i-1] == 'E' || sql[i-1] == 'e') && (i < 2 || !isIdentChar(sql[i-2]))
			end := stringEnd(sql, i, escape)
			emit(sql[i:end])
			prevIdent = false
			i = end

		case c == '"':
			end := quotedIdentEnd(sql, i)
			emit(sql[i:end])
			prevIdent = true
			i = end

		case c == '$' && !prevIdent:
			if tagEnd, ok := dollarTag(sql, i); ok {
				tag := sql[i:tagEnd]
				close := strings.Index(sql[tagEnd:], tag)
				bodyEnd, end := n, n
				if close >= 0 {
					bodyEnd = tagEnd + close
					end = bodyEnd + len(tag)
				}
				if bodyDepth > 0 {
					emit(tag + normalize(sql[tagEnd:bodyEnd], dropComments, bodyDepth-1) + sql[bodyEnd:end])
				} else {
					emit(sql[i:end])
				}
				prevIdent = false
				i = end
				break
			}
			emit("$")
			prevIdent = true // $1: a parameter continues like an identifier
			i++

		default:
			// Copy a maximal run of ordinary characters in one piece.
			end := i + 1
			for end < n && !isSpace(sql[end]) && sql[end] != '\'' && sql[end] != '"' && sql[end] != '$' &&
				!(sql[end] == '-' && end+1 < n && sql[end+1] == '-') &&
				!(sql[end] == '/' && end+1 < n && sql[end+1] == '*') {
				end++
			}
			emit(sql[i:end])
			prevIdent = isIdentChar(sql[end-1])
			i = end
		}
	}
	return strings.TrimSpace(out.String())
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

func isIdentChar(c byte) bool {
	return c == '_' || c == '$' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

// collapse reduces every whitespace run inside s to one space and trims it.
func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// blockCommentEnd returns the index just past the block comment starting at i,
// honoring nesting; an unterminated comment runs to the end of the text.
func blockCommentEnd(s string, i int) int {
	depth := 0
	j := i
	for j < len(s) {
		switch {
		case j+1 < len(s) && s[j] == '/' && s[j+1] == '*':
			depth++
			j += 2
		case j+1 < len(s) && s[j] == '*' && s[j+1] == '/':
			depth--
			j += 2
			if depth == 0 {
				return j
			}
		default:
			j++
		}
	}
	return len(s)
}

// stringEnd returns the index just past the string literal opening at i.
func stringEnd(s string, i int, escape bool) int {
	j := i + 1
	for j < len(s) {
		switch {
		case escape && s[j] == '\\' && j+1 < len(s):
			j += 2
		case s[j] == '\'':
			if j+1 < len(s) && s[j+1] == '\'' {
				j += 2
				continue
			}
			return j + 1
		default:
			j++
		}
	}
	return len(s)
}

// quotedIdentEnd returns the index just past the quoted identifier opening at i.
func quotedIdentEnd(s string, i int) int {
	j := i + 1
	for j < len(s) {
		if s[j] == '"' {
			if j+1 < len(s) && s[j+1] == '"' {
				j += 2
				continue
			}
			return j + 1
		}
		j++
	}
	return len(s)
}

// dollarTag reports whether a dollar-quote opener ($$ or $tag$) starts at i and
// returns the index just past it.
func dollarTag(s string, i int) (int, bool) {
	j := i + 1
	if j < len(s) && s[j] == '$' {
		return j + 1, true
	}
	if j >= len(s) || !(s[j] == '_' || (s[j] >= 'a' && s[j] <= 'z') || (s[j] >= 'A' && s[j] <= 'Z') || s[j] >= 0x80) {
		return 0, false
	}
	for j < len(s) && s[j] != '$' {
		if !isIdentChar(s[j]) {
			return 0, false
		}
		j++
	}
	if j >= len(s) {
		return 0, false
	}
	return j + 1, true
}
