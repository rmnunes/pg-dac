package sqltext

// Span is the byte range [Start, End) of one statement inside a script,
// including the comments and whitespace that lead into it and the semicolon
// that ends it (when there is one). Code is where the statement's own text
// begins, after those leading comments, so a rewrite can keep them.
type Span struct {
	Start, Code, End int
}

// Spans locates the statements of a script: it ends a statement at every
// semicolon that is not inside a string literal, a quoted identifier, a
// dollar-quoted body or a comment, so DO blocks and function bodies stay whole.
// Comments and blank lines before a statement belong to it (they describe what
// follows); trailing comment-only text after the last statement is dropped.
// Positions let callers rewrite one statement of a file in place.
func Spans(sql string) []Span {
	var spans []Span
	n := len(sql)
	start, code := 0, -1
	i := 0
	for i < n {
		c := sql[i]
		if code < 0 && !isSpace(c) && !(c == '-' && i+1 < n && sql[i+1] == '-') && !(c == '/' && i+1 < n && sql[i+1] == '*') {
			code = i
		}
		switch {
		case c == '-' && i+1 < n && sql[i+1] == '-':
			for i < n && sql[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && sql[i+1] == '*':
			i = blockCommentEnd(sql, i)
		case c == '\'':
			escape := i > 0 && (sql[i-1] == 'E' || sql[i-1] == 'e') && (i < 2 || !isIdentChar(sql[i-2]))
			i = stringEnd(sql, i, escape)
		case c == '"':
			i = quotedIdentEnd(sql, i)
		case c == '$' && (i == 0 || !isIdentChar(sql[i-1])):
			if tagEnd, ok := dollarTag(sql, i); ok {
				tag := sql[i:tagEnd]
				if close := indexFrom(sql, tag, tagEnd); close >= 0 {
					i = close + len(tag)
				} else {
					i = n
				}
			} else {
				i++
			}
		case c == ';':
			i++
			if code < 0 {
				code = i - 1
			}
			spans = append(spans, Span{Start: start, Code: code, End: i})
			start, code = i, -1
		default:
			i++
		}
	}
	if hasCode(sql[start:]) {
		if code < 0 {
			code = start
		}
		spans = append(spans, Span{Start: start, Code: code, End: n})
	}
	return spans
}

// Split returns the statements of a script as text (see Spans), each trimmed.
func Split(sql string) []string {
	spans := Spans(sql)
	out := make([]string, 0, len(spans))
	for _, s := range spans {
		out = append(out, trimSpace(sql[s.Start:s.End]))
	}
	return out
}

// HasCode reports whether s contains anything besides whitespace and comments.
func HasCode(s string) bool {
	return Normalize(s, true) != ""
}

func hasCode(s string) bool { return HasCode(s) }

func indexFrom(s, sub string, from int) int {
	for i := from; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func trimSpace(s string) string {
	i, j := 0, len(s)
	for i < j && isSpace(s[i]) {
		i++
	}
	for j > i && isSpace(s[j-1]) {
		j--
	}
	return s[i:j]
}
