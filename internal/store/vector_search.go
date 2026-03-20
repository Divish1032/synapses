package store

import "container/heap"

// scoredID holds a candidate's identity and cosine similarity score during
// the lightweight first-pass scan. Content and metadata are fetched in a
// second pass only for the top-K winners.
type scoredID struct {
	id    string
	score float32
	stale bool // true when the embedding was marked stale (anchored entity changed)
}

// topKHeap is a min-heap of scoredID capped at size k.
// During a vector scan the heap retains only the k highest-scoring candidates.
// When full, a new candidate replaces the root (minimum) only if its score
// exceeds the current minimum — giving O(N log K) selection instead of
// O(N log N) sort over all candidates.
type topKHeap struct {
	k     int
	items []scoredID
}

func (h *topKHeap) Len() int           { return len(h.items) }
func (h *topKHeap) Less(i, j int) bool { return h.items[i].score < h.items[j].score } // min-heap
func (h *topKHeap) Swap(i, j int)      { h.items[i], h.items[j] = h.items[j], h.items[i] }

func (h *topKHeap) Push(x any) { h.items = append(h.items, x.(scoredID)) }

func (h *topKHeap) Pop() any {
	old := h.items
	n := len(old)
	item := old[n-1]
	h.items = old[:n-1]
	return item
}

// tryPush adds a candidate to the heap if it belongs in the top-K.
// Returns true if the candidate was accepted.
// stale indicates the embedding was marked stale by the file watcher
// (anchored entity changed) — propagated to MemorySearchResult so
// agents know to verify the memory before trusting it.
func (h *topKHeap) tryPush(id string, score float32, stale bool) bool {
	if h.k <= 0 {
		return false
	}
	if len(h.items) < h.k {
		heap.Push(h, scoredID{id: id, score: score, stale: stale})
		return true
	}
	// Heap is full — only accept if better than current minimum.
	if score > h.items[0].score {
		h.items[0] = scoredID{id: id, score: score, stale: stale}
		heap.Fix(h, 0)
		return true
	}
	return false
}

// drain returns all items sorted by descending score (best first)
// and empties the heap.
func (h *topKHeap) drain() []scoredID {
	n := len(h.items)
	if n == 0 {
		return nil
	}
	result := make([]scoredID, n)
	// Pop from min-heap gives ascending order; fill from the back for descending.
	for i := n - 1; i >= 0; i-- {
		result[i] = heap.Pop(h).(scoredID)
	}
	return result
}
