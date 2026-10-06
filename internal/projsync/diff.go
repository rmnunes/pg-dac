package projsync

import (
	"fmt"
	"strings"
)

// unifiedDiff renders a minimal unified diff between two texts (two lines of
// context), using the longest common subsequence of their lines.
func unifiedDiff(a, b string) string {
	al := splitLines(a)
	bl := splitLines(b)
	type op struct {
		kind byte // ' ', '-', '+'
		text string
	}
	n, m := len(al), len(bl)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if al[i] == bl[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	var ops []op
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && al[i] == bl[j]:
			ops = append(ops, op{' ', al[i]})
			i++
			j++
		case i < n && (j >= m || lcs[i+1][j] >= lcs[i][j+1]):
			ops = append(ops, op{'-', al[i]})
			i++
		default:
			ops = append(ops, op{'+', bl[j]})
			j++
		}
	}
	// Emit hunks with 2 lines of context.
	const ctx = 2
	var out strings.Builder
	k := 0
	for k < len(ops) {
		if ops[k].kind == ' ' {
			k++
			continue
		}
		start := k - ctx
		if start < 0 {
			start = 0
		}
		end := k
		for end < len(ops) {
			if ops[end].kind != ' ' {
				end++
				continue
			}
			// a run of context: stop when it exceeds 2*ctx without a change
			run := end
			for run < len(ops) && ops[run].kind == ' ' {
				run++
			}
			if run >= len(ops) || run-end > 2*ctx {
				end += ctx
				if end > len(ops) {
					end = len(ops)
				}
				break
			}
			end = run
		}
		if end > len(ops) {
			end = len(ops)
		}
		// hunk header positions (1-based) and counts
		oa, ob := 0, 0
		for _, o := range ops[:start] {
			if o.kind != '+' {
				oa++
			}
			if o.kind != '-' {
				ob++
			}
		}
		ca, cb := 0, 0
		for _, o := range ops[start:end] {
			if o.kind != '+' {
				ca++
			}
			if o.kind != '-' {
				cb++
			}
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", oa+1, ca, ob+1, cb)
		for _, o := range ops[start:end] {
			out.WriteByte(o.kind)
			out.WriteString(o.text)
			out.WriteByte('\n')
		}
		k = end
	}
	return out.String()
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}
