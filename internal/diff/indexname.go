package diff

import (
	"fmt"
	"hash/fnv"
	"unicode/utf8"

	"github.com/rmnunes/pg-dac/internal/model"
)

// tempIndexSuffix marks the name an index is rebuilt under (see
// OpReplaceIndex); a leftover with this suffix is a rebuild that was
// interrupted, which the next rebuild clears.
const tempIndexSuffix = "_pgdac_new"

// maxIdentLen is PostgreSQL's identifier limit (NAMEDATALEN - 1) in bytes.
const maxIdentLen = 63

// tempIndexName derives the name an index is rebuilt under before it takes the
// original name back. It is deterministic (the same plan renders the same
// script every time), fits PostgreSQL's identifier limit, and is distinct from
// every name in taken. A name too long for the suffix is truncated and stamped
// with a hash of the full name, so two long names sharing a prefix cannot
// collide after truncation.
func tempIndexName(name string, taken map[string]bool) string {
	base := name
	if len(base)+len(tempIndexSuffix) > maxIdentLen {
		h := fnv.New32a()
		h.Write([]byte(name))
		tag := fmt.Sprintf("_%08x", h.Sum32())
		base = truncateUTF8(name, maxIdentLen-len(tempIndexSuffix)-len(tag)) + tag
	}
	cand := base + tempIndexSuffix
	for n := 2; taken[cand]; n++ {
		extra := fmt.Sprintf("_%d", n)
		cand = truncateUTF8(base, maxIdentLen-len(tempIndexSuffix)-len(extra)) + extra + tempIndexSuffix
	}
	return cand
}

// truncateUTF8 cuts s to at most n bytes without splitting a multibyte rune.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// indexNamesTaken collects the names a temporary index must avoid: every
// index on either side of the comparison and the relation's constraints
// (whose backing indexes share the namespace).
func indexNamesTaken(table *model.Table, fromIdx, toIdx []*model.Index) map[string]bool {
	taken := map[string]bool{}
	for _, i := range fromIdx {
		taken[i.Name] = true
	}
	for _, i := range toIdx {
		taken[i.Name] = true
	}
	if table != nil {
		for _, c := range table.Constraints {
			taken[c.Name] = true
		}
	}
	return taken
}
