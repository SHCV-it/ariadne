// Package index is the hot path. Everything here is read-only once built;
// the daemon swaps whole Index values via atomic.Pointer so queries never
// take a lock.
package index

import (
	"math"
	"sort"
	"strings"
	"time"

	"ariadne/internal/core"
)

// HalfLife governs frecency decay. 30 days: a command used once today outranks
// one used four times two months ago.
const HalfLife = 30 * 24 * time.Hour

var lambda = math.Ln2 / HalfLife.Seconds()

// Decay returns the multiplier for an observation made dt seconds ago.
func Decay(dtSeconds float64) float64 {
	if dtSeconds < 0 {
		dtSeconds = 0
	}
	return math.Exp(-lambda * dtSeconds)
}

// Tunables for candidate generation. These bound worst-case query cost.
const (
	MaxDirectRange = 400  // scan the alphabetical range directly below this size
	ScoreScanDepth = 6000 // otherwise filter this many top-frecency entries
	MaxCandidates  = 200
	FuzzyDepth     = 4000
)

// Index is an immutable query-side snapshot.
type Index struct {
	Entries []*core.Entry

	sorted  []int32 // entry ids ordered by Norm (for prefix ranges)
	byScore []int32 // entry ids ordered by global decayed score, descending
	byHash  map[core.Hash]int32

	// lower holds the lowercased normal form per entry, precomputed at build.
	// Computing it per query allocated once per scanned candidate and was the
	// single largest contributor to tail latency.
	lower []string

	ByCwd  map[string][]int32
	ByRepo map[string][]int32

	Bigrams     map[core.Hash]map[core.Hash]int32
	bigramTot   map[core.Hash]int32
	Tools       map[string]*core.ToolSpec
	ToolsOnPATH map[string]bool

	BuiltAt time.Time
}

// Build constructs an immutable index. Cost is O(n log n); measured at ~180ms
// for 500k entries, which is the cold-start budget, not the query budget.
func Build(entries []*core.Entry, bigrams map[core.Hash]map[core.Hash]int32,
	tools map[string]*core.ToolSpec, onPath map[string]bool, now time.Time) *Index {

	ix := &Index{
		Entries:     entries,
		byHash:      make(map[core.Hash]int32, len(entries)),
		ByCwd:       make(map[string][]int32),
		ByRepo:      make(map[string][]int32),
		Bigrams:     bigrams,
		bigramTot:   make(map[core.Hash]int32, len(bigrams)),
		Tools:       tools,
		ToolsOnPATH: onPath,
		BuiltAt:     now,
	}
	if ix.Tools == nil {
		ix.Tools = map[string]*core.ToolSpec{}
	}
	if ix.ToolsOnPATH == nil {
		ix.ToolsOnPATH = map[string]bool{}
	}

	ix.sorted = make([]int32, len(entries))
	ix.byScore = make([]int32, len(entries))
	ix.lower = make([]string, len(entries))
	nowUnix := now.Unix()

	for i, e := range entries {
		id := int32(i)
		ix.sorted[i] = id
		ix.byScore[i] = id
		ix.lower[i] = strings.ToLower(e.Norm)
		ix.byHash[e.Hash] = id
		for k := range e.ByCwd {
			ix.ByCwd[k] = append(ix.ByCwd[k], id)
		}
		for k := range e.ByRepo {
			ix.ByRepo[k] = append(ix.ByRepo[k], id)
		}
	}

	sort.Slice(ix.sorted, func(a, b int) bool {
		return entries[ix.sorted[a]].Norm < entries[ix.sorted[b]].Norm
	})
	score := func(id int32) float64 {
		e := entries[id]
		return e.Global.Decayed * Decay(float64(nowUnix-e.Global.LastTS))
	}
	sort.Slice(ix.byScore, func(a, b int) bool {
		return score(ix.byScore[a]) > score(ix.byScore[b])
	})

	for prev, m := range bigrams {
		var t int32
		for _, n := range m {
			t += n
		}
		ix.bigramTot[prev] = t
	}
	return ix
}

func (ix *Index) Len() int { return len(ix.Entries) }

// Lower returns the precomputed lowercase form for an entry id.
func (ix *Index) Lower(id int32) string {
	if id < 0 || int(id) >= len(ix.lower) {
		return ""
	}
	return ix.lower[id]
}

func (ix *Index) Get(id int32) *core.Entry {
	if id < 0 || int(id) >= len(ix.Entries) {
		return nil
	}
	return ix.Entries[id]
}

func (ix *Index) ByHash(h core.Hash) (int32, bool) {
	id, ok := ix.byHash[h]
	return id, ok
}

// BigramProb returns P(next | prev) with add-one smoothing on the denominator.
func (ix *Index) BigramProb(prev, next core.Hash) float64 {
	m, ok := ix.Bigrams[prev]
	if !ok {
		return 0
	}
	n := m[next]
	if n == 0 {
		return 0
	}
	return float64(n) / float64(ix.bigramTot[prev]+1)
}

// prefixRange returns the [lo,hi) span of ix.sorted matching the prefix.
func (ix *Index) prefixRange(prefix string) (int, int) {
	n := len(ix.sorted)
	lo := sort.Search(n, func(i int) bool {
		return ix.Entries[ix.sorted[i]].Norm >= prefix
	})
	hi := sort.Search(n, func(i int) bool {
		return !strings.HasPrefix(ix.Entries[ix.sorted[i]].Norm, prefix)
	})
	if hi < lo {
		hi = lo
	}
	return lo, hi
}

// PrefixCandidates returns entry ids whose normalized form starts with prefix.
//
// Two regimes, chosen by range size:
//   - narrow range: scan it directly (alphabetical, complete)
//   - wide range (short prefixes): filter the top-frecency list instead, so
//     that "g" returns your most-used g-commands rather than the alphabetically
//     first ones. Bounded by ScoreScanDepth, so cost is constant.
func (ix *Index) PrefixCandidates(prefix string) []int32 {
	if prefix == "" {
		n := MaxCandidates
		if n > len(ix.byScore) {
			n = len(ix.byScore)
		}
		return append([]int32(nil), ix.byScore[:n]...)
	}
	lo, hi := ix.prefixRange(prefix)
	if hi-lo == 0 {
		return nil
	}
	if hi-lo <= MaxDirectRange {
		out := make([]int32, hi-lo)
		copy(out, ix.sorted[lo:hi])
		return out
	}
	depth := ScoreScanDepth
	if depth > len(ix.byScore) {
		depth = len(ix.byScore)
	}
	out := make([]int32, 0, MaxCandidates)
	for _, id := range ix.byScore[:depth] {
		if strings.HasPrefix(ix.Entries[id].Norm, prefix) {
			out = append(out, id)
			if len(out) >= MaxCandidates {
				break
			}
		}
	}
	return out
}

// FuzzyCandidates does a subsequence match over the top-frecency band. Only
// invoked when the exact-prefix pass came back thin.
func (ix *Index) FuzzyCandidates(pattern string, limit int) []int32 {
	if pattern == "" {
		return nil
	}
	pat := strings.ToLower(pattern)
	depth := FuzzyDepth
	if depth > len(ix.byScore) {
		depth = len(ix.byScore)
	}
	out := make([]int32, 0, limit)
	for _, id := range ix.byScore[:depth] {
		if _, ok := subseqScore(ix.lower[id], pat); ok {
			out = append(out, id)
			if len(out) >= limit {
				break
			}
		}
	}
	return out
}

// subseqScore reports whether pat is a subsequence of s, and how tight the
// match is (1.0 = contiguous from position 0).
func subseqScore(s, pat string) (float64, bool) {
	if pat == "" {
		return 1, true
	}
	si, first, last := 0, -1, -1
	for pi := 0; pi < len(pat); pi++ {
		found := false
		for ; si < len(s); si++ {
			if s[si] == pat[pi] {
				if first < 0 {
					first = si
				}
				last = si
				si++
				found = true
				break
			}
		}
		if !found {
			return 0, false
		}
	}
	span := float64(last - first + 1)
	if span <= 0 {
		return 0, false
	}
	density := float64(len(pat)) / span
	head := 1.0 / (1.0 + float64(first)*0.25)
	return density * head, true
}

// MatchQuality scores how well a candidate matches what was typed. Both
// lowercase forms are precomputed by the caller; this function allocates
// nothing, because it runs up to MaxCandidates times per keystroke.
func MatchQuality(norm, low, typed, typedLow string) float64 {
	if typed == "" {
		return 0.5
	}
	if strings.HasPrefix(norm, typed) {
		return 1.0
	}
	if strings.HasPrefix(low, typedLow) {
		return 0.9
	}
	q, ok := subseqScore(low, typedLow)
	if !ok {
		return 0
	}
	return 0.7 * q
}
