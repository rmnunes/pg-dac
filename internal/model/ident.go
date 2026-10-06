package model

import "strings"

// QuoteIdent quotes a PostgreSQL identifier with double quotes, doubling any
// embedded double quotes. pg-dac always emits quoted identifiers in generated
// SQL: it is unconditionally safe (a quoted identifier is case-sensitive and
// matches the exact stored name) and avoids every reserved-keyword and
// folding pitfall. Introspected names are stored verbatim (the catalog's true
// name), so quoting them round-trips exactly.
func QuoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// QuoteLiteral quotes a string as a PostgreSQL single-quoted literal, doubling
// embedded single quotes. Used for COMMENT bodies and similar.
func QuoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
