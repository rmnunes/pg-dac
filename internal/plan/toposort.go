package plan

import "container/heap"

// toposort performs a stable, priority-aware topological sort — the same shape
// of algorithm pg_dump's pg_dump_sort.c uses (Knuth's topological sort driven by
// a binary heap). Among all nodes whose prerequisites are satisfied, it always
// emits the one with the lowest (priority, ordinal) key, so object-type priority
// acts as a deterministic tie-breaker on top of the real dependency edges.
//
// edges[a] lists nodes that must come AFTER node a (a is their prerequisite).
// priority[i] and ordinal[i] are the sort keys. n is the node count.
//
// If a dependency cycle prevents a complete ordering, the remaining nodes are
// appended in (priority, ordinal) order so the caller still gets every node;
// cycles among real PostgreSQL objects (mutual FKs, view/rule loops) are rare in
// a single migration and are handled structurally elsewhere (FKs are emitted in a
// later priority band than their tables).
func toposort(n int, edges map[int][]int, priority, ordinal []int) []int {
	indeg := make([]int, n)
	for _, outs := range edges {
		for _, b := range outs {
			indeg[b]++
		}
	}

	pq := &nodeHeap{priority: priority, ordinal: ordinal}
	for i := 0; i < n; i++ {
		if indeg[i] == 0 {
			pq.items = append(pq.items, i)
		}
	}
	heap.Init(pq)

	out := make([]int, 0, n)
	for pq.Len() > 0 {
		i := heap.Pop(pq).(int)
		out = append(out, i)
		for _, b := range edges[i] {
			indeg[b]--
			if indeg[b] == 0 {
				heap.Push(pq, b)
			}
		}
	}

	if len(out) < n {
		// Cycle fallback: append any nodes never emitted, in key order.
		emitted := make([]bool, n)
		for _, i := range out {
			emitted[i] = true
		}
		var rest []int
		for i := 0; i < n; i++ {
			if !emitted[i] {
				rest = append(rest, i)
			}
		}
		sortByKey(rest, priority, ordinal)
		out = append(out, rest...)
	}
	return out
}

func sortByKey(idx, priority, ordinal []int) {
	// insertion sort is fine for the small cycle-fallback set
	for i := 1; i < len(idx); i++ {
		for j := i; j > 0 && less(idx[j], idx[j-1], priority, ordinal); j-- {
			idx[j], idx[j-1] = idx[j-1], idx[j]
		}
	}
}

func less(a, b int, priority, ordinal []int) bool {
	if priority[a] != priority[b] {
		return priority[a] < priority[b]
	}
	return ordinal[a] < ordinal[b]
}

type nodeHeap struct {
	items    []int
	priority []int
	ordinal  []int
}

func (h nodeHeap) Len() int            { return len(h.items) }
func (h nodeHeap) Less(i, j int) bool  { return less(h.items[i], h.items[j], h.priority, h.ordinal) }
func (h nodeHeap) Swap(i, j int)       { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *nodeHeap) Push(x interface{}) { h.items = append(h.items, x.(int)) }
func (h *nodeHeap) Pop() interface{} {
	old := h.items
	n := len(old)
	x := old[n-1]
	h.items = old[:n-1]
	return x
}
