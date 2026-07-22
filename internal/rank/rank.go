// Package rank scores candidates. The model is deliberately small: 17
// features and a dot product. This is not a shortcut — ranking a single user's
// own history is a low-dimensional, in-distribution supervised problem, and a
// logistic model trained on that user's accept/reject log will beat a language
// model at it while costing nanoseconds instead of milliseconds.
package rank

import (
	"math"
	"sort"
	"time"

	"ariadne/internal/core"
	"ariadne/internal/index"
	"ariadne/internal/store"
)

const NumFeatures = 17

// Feature indices. Keep stable: persisted weight vectors are positional.
const (
	FSuccess = iota // log1p(global successes)
	FFailure        // log1p(global failures)
	FRecency        // exp decay since last use
	FCwdExact
	FCwdAncestor
	FRepo
	FBranch
	FHost
	FBigram // P(cmd | previous cmd)
	FSessionRecency
	FMatchQuality
	FLengthPenalty
	FTimeOfDay
	FIsSpec // candidate came from a harvested tool spec, not history
	FSpecConfidence
	FToolPresent // argv0 exists on this host
	FLastAccepted
)

var FeatureNames = [NumFeatures]string{
	"success", "failure", "recency", "cwd_exact", "cwd_ancestor",
	"repo", "branch", "host", "bigram", "session_recency",
	"match_quality", "length_penalty", "time_of_day", "is_spec",
	"spec_confidence", "tool_present", "last_accepted",
}

// DefaultWeights are hand-tuned priors used until ~500 impressions exist.
// A learned vector must beat these on held-out MRR before it is promoted.
func DefaultWeights() []float64 {
	w := make([]float64, NumFeatures)
	w[FSuccess] = 1.10
	w[FFailure] = -0.85
	w[FRecency] = 1.40
	w[FCwdExact] = 1.20
	w[FCwdAncestor] = 0.45
	w[FRepo] = 0.80
	w[FBranch] = 0.25
	w[FHost] = 0.30
	w[FBigram] = 1.60
	w[FSessionRecency] = 0.90
	w[FMatchQuality] = 2.20
	w[FLengthPenalty] = 0.35
	w[FTimeOfDay] = 0.15
	w[FIsSpec] = -0.20
	w[FSpecConfidence] = 0.60
	w[FToolPresent] = 1.00
	w[FLastAccepted] = 1.80
	return w
}

type Source uint8

const (
	SrcHistory Source = iota
	SrcBigram
	SrcSpec
	SrcTemplate
)

func (s Source) String() string {
	switch s {
	case SrcBigram:
		return "next"
	case SrcSpec:
		return "spec"
	case SrcTemplate:
		return "tmpl"
	}
	return "hist"
}

// Candidate is one scored suggestion.
type Candidate struct {
	Text     string
	Display  string
	Desc     string
	Hash     core.Hash
	Source   Source
	Count    int32
	Context  string
	Features [NumFeatures]float64
	Score    float64
}

// Context is everything known about the moment of the query.
type Context struct {
	Cwd          string
	GitRoot      string
	GitBranch    string
	Host         string
	PrevHash     core.Hash
	SessionCmds  []core.Hash // most recent first, capped
	LastAccepted map[string]core.Hash
	Now          time.Time
}

func hourBucket(t time.Time) int { return t.Hour() / 6 }

// FeaturesFor fills the feature vector for a history-derived candidate.
// low/typedLow are precomputed lowercase forms: this runs up to
// MaxCandidates times per keystroke and must not allocate.
func FeaturesFor(ix *index.Index, e *core.Entry, low, typed, typedLow string, ctx *Context) [NumFeatures]float64 {
	var f [NumFeatures]float64
	now := ctx.Now.Unix()

	f[FSuccess] = math.Log1p(float64(e.Global.NSuccess))
	f[FFailure] = math.Log1p(float64(e.Global.NFailure))
	f[FRecency] = index.Decay(float64(now - e.Global.LastTS))

	if s, ok := e.ByCwd[ctx.Cwd]; ok && s != nil {
		f[FCwdExact] = 1
	} else {
		for dir := range e.ByCwd {
			if len(dir) < len(ctx.Cwd) && len(dir) > 1 &&
				ctx.Cwd[:min(len(dir), len(ctx.Cwd))] == dir {
				f[FCwdAncestor] = 1
				break
			}
		}
	}
	if ctx.GitRoot != "" {
		if _, ok := e.ByRepo[ctx.GitRoot]; ok {
			f[FRepo] = 1
		}
	}
	if ctx.GitBranch != "" {
		if _, ok := e.ByRepo[ctx.GitRoot+"#"+ctx.GitBranch]; ok {
			f[FBranch] = 1
		}
	}
	if ctx.Host != "" {
		if _, ok := e.ByHost[ctx.Host]; ok {
			f[FHost] = 1
		}
	}
	f[FBigram] = ix.BigramProb(ctx.PrevHash, e.Hash)

	for i, h := range ctx.SessionCmds {
		if h == e.Hash {
			f[FSessionRecency] = 1.0 / (1.0 + float64(i))
			break
		}
	}
	f[FMatchQuality] = index.MatchQuality(e.Norm, low, typed, typedLow)
	// Prefer suggestions that save typing but are not absurdly long.
	saved := float64(len(e.Norm) - len(typed))
	if saved > 0 {
		f[FLengthPenalty] = math.Log1p(saved) / (1.0 + 0.02*float64(len(e.Norm)))
	}
	tot := int32(0)
	for _, c := range e.Global.HourHist {
		tot += c
	}
	if tot > 0 {
		f[FTimeOfDay] = float64(e.Global.HourHist[hourBucket(ctx.Now)]) / float64(tot)
	}
	if e.Argv0 == "" || ix.ToolsOnPATH[e.Argv0] || isBuiltin(e.Argv0) {
		f[FToolPresent] = 1
	}
	if ctx.LastAccepted != nil {
		if h, ok := ctx.LastAccepted[typed]; ok && h == e.Hash {
			f[FLastAccepted] = 1
		}
	}
	return f
}

var builtins = map[string]bool{
	"cd": true, "export": true, "source": true, "alias": true, "unalias": true,
	"echo": true, "set": true, "unset": true, "exec": true, "eval": true,
	"pushd": true, "popd": true, "jobs": true, "fg": true, "bg": true,
	"kill": true, "type": true, "which": true, "history": true, "exit": true,
}

func isBuiltin(s string) bool { return builtins[s] }

// Score applies the linear model.
func Score(w []float64, f [NumFeatures]float64) float64 {
	var z float64
	for i := 0; i < NumFeatures; i++ {
		z += w[i] * f[i]
	}
	return 1.0 / (1.0 + math.Exp(-z))
}

// SortAndTrim scores in place and returns the top n.
func SortAndTrim(w []float64, cands []Candidate, n int) []Candidate {
	for i := range cands {
		cands[i].Score = Score(w, cands[i].Features)
	}
	sort.SliceStable(cands, func(a, b int) bool {
		if cands[a].Score != cands[b].Score {
			return cands[a].Score > cands[b].Score
		}
		return len(cands[a].Text) < len(cands[b].Text)
	})
	if len(cands) > n {
		cands = cands[:n]
	}
	return cands
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------------- Training ----------------

// FTRL is an FTRL-Proximal logistic learner. Chosen over plain SGD because the
// L1 term drives useless features to exactly zero, which makes the learned
// weight vector auditable: you can read off which signals actually matter for
// this user.
type FTRL struct {
	Alpha, Beta float64
	L1, L2      float64
	z, n        []float64
}

func NewFTRL() *FTRL {
	return &FTRL{
		Alpha: 0.15, Beta: 1.0, L1: 0.002, L2: 1.0,
		z: make([]float64, NumFeatures),
		n: make([]float64, NumFeatures),
	}
}

func (m *FTRL) weights() []float64 {
	w := make([]float64, NumFeatures)
	for i := range w {
		if math.Abs(m.z[i]) <= m.L1 {
			w[i] = 0
			continue
		}
		sign := 1.0
		if m.z[i] < 0 {
			sign = -1.0
		}
		w[i] = -(m.z[i] - sign*m.L1) /
			((m.Beta+math.Sqrt(m.n[i]))/m.Alpha + m.L2)
	}
	return w
}

func (m *FTRL) update(f []float64, y float64) {
	w := m.weights()
	var z float64
	for i := range f {
		z += w[i] * f[i]
	}
	p := 1.0 / (1.0 + math.Exp(-z))
	g := p - y
	for i := range f {
		gi := g * f[i]
		sigma := (math.Sqrt(m.n[i]+gi*gi) - math.Sqrt(m.n[i])) / m.Alpha
		m.z[i] += gi - sigma*w[i]
		m.n[i] += gi * gi
	}
}

// TrainResult reports whether new weights earned promotion.
type TrainResult struct {
	Weights  []float64
	NewMRR   float64
	OldMRR   float64
	NSamples int
	Promoted bool
	Zeroed   []string
}

// Train fits weights on impressions with a held-out tail, and only promotes if
// mean reciprocal rank improves. Refusing to promote a regression is the whole
// point; an autocomplete that silently gets worse is worse than a static one.
func Train(imps []store.Impression, current []float64, minSamples int) TrainResult {
	res := TrainResult{Weights: current, OldMRR: 0, NSamples: len(imps)}
	if len(imps) < minSamples {
		return res
	}
	split := len(imps) * 4 / 5
	train, holdout := imps[:split], imps[split:]

	m := NewFTRL()
	for epoch := 0; epoch < 6; epoch++ {
		for _, im := range train {
			for i, f := range im.Features {
				y := 0.0
				if i == im.Accepted {
					y = 1.0
				}
				m.update(f, y)
			}
		}
	}
	nw := m.weights()
	res.NewMRR = mrr(holdout, nw)
	res.OldMRR = mrr(holdout, current)
	if res.NewMRR > res.OldMRR {
		res.Weights = nw
		res.Promoted = true
	}
	for i, v := range nw {
		if v == 0 {
			res.Zeroed = append(res.Zeroed, FeatureNames[i])
		}
	}
	return res
}

func mrr(imps []store.Impression, w []float64) float64 {
	var sum float64
	var n int
	for _, im := range imps {
		if im.Accepted < 0 || len(im.Features) == 0 {
			continue
		}
		type sc struct {
			i int
			s float64
		}
		scored := make([]sc, len(im.Features))
		for i, f := range im.Features {
			var z float64
			for j := range f {
				if j < len(w) {
					z += w[j] * f[j]
				}
			}
			scored[i] = sc{i, z}
		}
		sort.Slice(scored, func(a, b int) bool { return scored[a].s > scored[b].s })
		for rank, s := range scored {
			if s.i == im.Accepted {
				sum += 1.0 / float64(rank+1)
				n++
				break
			}
		}
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}
