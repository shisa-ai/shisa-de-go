package tokenizer

import "container/heap"

// merge applies the BPE merge table to a symbol sequence.
//
// It repeatedly takes the adjacent pair with the lowest merge rank and merges
// it, which is the definition of BPE. Candidates live in a heap keyed by
// (rank, left position), and every pop is re-checked against the current
// sequence, so stale entries cost a comparison and nothing else. That keeps a
// long prompt linear-ish rather than quadratic: the pinned tokenizer normalizes
// a whole prompt into one pre-token, so this can be tens of thousands of
// symbols in one call.
func (t *Tokenizer) merge(symbols []int32) []int32 {
	n := len(symbols)
	if n < 2 {
		return symbols
	}

	// A doubly linked list over the slice's indices. Indices are stable; only
	// the links and the symbol at each index change.
	prev := make([]int32, n)
	next := make([]int32, n)
	removed := make([]bool, n)
	for i := range symbols {
		prev[i] = int32(i) - 1
		next[i] = int32(i) + 1
	}
	next[n-1] = -1

	queue := make(mergeHeap, 0, n)
	for i := 0; i+1 < n; i++ {
		if m, ok := t.merges[pair{symbols[i], symbols[i+1]}]; ok {
			queue = append(queue, mergeItem{
				rank: m.rank, left: int32(i), right: int32(i + 1),
				a: symbols[i], b: symbols[i+1], id: m.id,
			})
		}
	}
	heap.Init(&queue)

	for queue.Len() > 0 {
		item := heap.Pop(&queue).(mergeItem)
		l, r := item.left, item.right
		if removed[l] || removed[r] || next[l] != r ||
			symbols[l] != item.a || symbols[r] != item.b {
			continue
		}
		symbols[l] = item.id
		removed[r] = true
		if after := next[r]; after >= 0 {
			next[l] = after
			prev[after] = l
			t.pushPair(&queue, symbols, l, after)
		} else {
			next[l] = -1
		}
		if before := prev[l]; before >= 0 {
			t.pushPair(&queue, symbols, before, l)
		}
	}

	out := make([]int32, 0, n)
	for i := range symbols {
		if !removed[i] {
			out = append(out, symbols[i])
		}
	}
	return out
}

func (t *Tokenizer) pushPair(queue *mergeHeap, symbols []int32, l, r int32) {
	m, ok := t.merges[pair{symbols[l], symbols[r]}]
	if !ok {
		return
	}
	heap.Push(queue, mergeItem{rank: m.rank, left: l, right: r, a: symbols[l], b: symbols[r], id: m.id})
}

// mergeItem is one merge candidate. a and b are the symbol ids the candidate was
// created for, so a candidate whose symbols have since changed is recognised as
// stale instead of merging the wrong pair.
type mergeItem struct {
	rank        int32
	left, right int32
	a, b        int32
	id          int32
}

type mergeHeap []mergeItem

func (h mergeHeap) Len() int { return len(h) }

func (h mergeHeap) Less(i, j int) bool {
	if h[i].rank != h[j].rank {
		return h[i].rank < h[j].rank
	}
	return h[i].left < h[j].left
}

func (h mergeHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *mergeHeap) Push(x any) { *h = append(*h, x.(mergeItem)) }

func (h *mergeHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	old[n-1] = mergeItem{}
	*h = old[:n-1]
	return item
}
