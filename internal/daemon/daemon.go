package daemon

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ariadne/internal/core"
	"ariadne/internal/harvest"
	"ariadne/internal/index"
	"ariadne/internal/proto"
	"ariadne/internal/rank"
	"ariadne/internal/store"
)

type Config struct {
	SocketPath      string
	DataDir         string
	SnapshotEvery   time.Duration
	HarvestEvery    time.Duration
	TrainEvery      int // impressions
	MinTrainSamples int
	HarvestOnStart  bool
	MaxSessionCmds  int
	Deny            []*regexp.Regexp
	Verbose         bool
}

func DefaultConfig() Config {
	rt := os.Getenv("XDG_RUNTIME_DIR")
	if rt == "" {
		rt = os.TempDir()
	}
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" {
		data = filepath.Join(os.Getenv("HOME"), ".local", "share")
	}
	return Config{
		SocketPath:      filepath.Join(rt, "ariadne.sock"),
		DataDir:         filepath.Join(data, "ariadne"),
		SnapshotEvery:   2 * time.Minute,
		HarvestEvery:    12 * time.Hour,
		TrainEvery:      500,
		MinTrainSamples: 500,
		HarvestOnStart:  true,
		MaxSessionCmds:  20,
		// The daemon must not record its own administration commands
		// (ariadne forget/import/stats/...). They are meta noise that would
		// otherwise pollute the very history they manage. The -deny flag
		// appends additional patterns.
		Deny: []*regexp.Regexp{regexp.MustCompile(`^ariadne `)},
	}
}

// brain is the mutable authority. Writers hold mu; readers of the query path
// never touch it — they read the atomically published *index.Index instead.
type brain struct {
	mu      sync.Mutex
	entries map[core.Hash]*core.Entry
	bigrams map[core.Hash]map[core.Hash]int32
	tools   map[string]*core.ToolSpec
	onPath  map[string]bool
	dirty   bool
}

type Daemon struct {
	cfg   Config
	st    store.Store
	br    *brain
	ix    atomic.Pointer[index.Index]
	wts   atomic.Pointer[[]float64]
	wtVer atomic.Int64

	ingestCh chan *core.Event

	sessMu   sync.Mutex
	sessions map[string]*session

	impMu        sync.Mutex
	imps         []store.Impression
	lastShow     map[string]*shown // session -> last panel rendered
	accepted     map[string]core.Hash
	acceptedSnap atomicMap

	nQuery    atomic.Int64
	nIngest   atomic.Int64
	latSum    atomic.Int64
	latMax    atomic.Int64
	startedAt time.Time
}

type session struct {
	recent []core.Hash
	prev   core.Hash
}

// atomicMap publishes an immutable map without a mutex on the read side.
type atomicMap struct {
	p atomic.Pointer[map[string]core.Hash]
}

func (a *atomicMap) Load() map[string]core.Hash {
	if m := a.p.Load(); m != nil {
		return *m
	}
	return nil
}
func (a *atomicMap) Store(m *map[string]core.Hash) { a.p.Store(m) }

type shown struct {
	prefix   string
	features [][]float64
	hashes   []core.Hash
}

func New(cfg Config) (*Daemon, error) {
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	d := &Daemon{
		cfg:       cfg,
		st:        st,
		br:        &brain{entries: map[core.Hash]*core.Entry{}, bigrams: map[core.Hash]map[core.Hash]int32{}, tools: map[string]*core.ToolSpec{}, onPath: map[string]bool{}},
		ingestCh:  make(chan *core.Event, 4096),
		sessions:  map[string]*session{},
		lastShow:  map[string]*shown{},
		accepted:  map[string]core.Hash{},
		startedAt: time.Now(),
	}
	w := rank.DefaultWeights()
	d.wts.Store(&w)
	empty := map[string]core.Hash{}
	d.acceptedSnap.Store(&empty)
	if err := d.load(); err != nil {
		log.Printf("ariadned: load degraded: %v (starting from empty brain)", err)
	}
	d.rebuild()
	return d, nil
}

func (d *Daemon) load() error {
	snap, err := d.st.LoadSnapshot()
	if err != nil {
		// Corrupt snapshot is recoverable: the event log is the source of truth.
		log.Printf("ariadned: %v; replaying event log", err)
	}
	if snap != nil {
		for _, e := range snap.Entries {
			d.br.entries[e.Hash] = e
		}
		if snap.Bigrams != nil {
			d.br.bigrams = snap.Bigrams
		}
		if snap.Tools != nil {
			d.br.tools = snap.Tools
		}
		if len(snap.Weights) == rank.NumFeatures {
			w := snap.Weights
			d.wts.Store(&w)
			d.wtVer.Store(int64(snap.WeightsVer))
		}
		d.imps = snap.Impressions
	}
	if len(d.br.entries) == 0 {
		// Cold start or corrupt snapshot: rebuild statistics from the log.
		if err := d.st.LoadEvents(func(e *core.Event) { d.apply(e) }); err != nil {
			return err
		}
	}
	bins, _ := harvest.ScanPATH()
	for n := range bins {
		d.br.onPath[n] = true
	}
	return nil
}

// apply folds one event into the brain. Caller must hold br.mu, except during
// single-threaded startup replay.
func (d *Daemon) apply(e *core.Event) {
	if e.Norm == "" {
		return
	}
	h := e.Hash()
	en := d.br.entries[h]
	if en == nil {
		en = &core.Entry{Hash: h, Norm: e.Norm, Raw: e.Raw, Argv0: e.Argv0}
		d.br.entries[h] = en
	}
	if strings.Contains(e.Norm, core.RedactMark) {
		en.Redacted = true
	}
	bump := func(s *core.Stat) {
		if e.ExitCode == 0 {
			s.NSuccess++
		} else {
			s.NFailure++
		}
		if e.TS > s.LastTS {
			s.LastTS = e.TS
		}
		// Decayed counter, updated in place: decay what was there, add 1.
		dt := float64(e.TS - s.LastTS)
		if dt < 0 {
			dt = 0
		}
		s.Decayed = s.Decayed*index.Decay(dt) + 1
		s.HourHist[(time.Unix(e.TS, 0).Hour())/6]++
	}
	bump(&en.Global)
	if e.Cwd != "" {
		if en.ByCwd == nil {
			en.ByCwd = map[string]*core.Stat{}
		}
		if en.ByCwd[e.Cwd] == nil {
			en.ByCwd[e.Cwd] = &core.Stat{}
		}
		bump(en.ByCwd[e.Cwd])
	}
	if e.GitRoot != "" {
		if en.ByRepo == nil {
			en.ByRepo = map[string]*core.Stat{}
		}
		for _, k := range []string{e.GitRoot, e.GitRoot + "#" + e.GitBranch} {
			if en.ByRepo[k] == nil {
				en.ByRepo[k] = &core.Stat{}
			}
			bump(en.ByRepo[k])
		}
	}
	if e.Host != "" {
		if en.ByHost == nil {
			en.ByHost = map[string]*core.Stat{}
		}
		if en.ByHost[e.Host] == nil {
			en.ByHost[e.Host] = &core.Stat{}
		}
		bump(en.ByHost[e.Host])
	}
	if e.PrevHash != 0 {
		m := d.br.bigrams[e.PrevHash]
		if m == nil {
			m = map[core.Hash]int32{}
			d.br.bigrams[e.PrevHash] = m
		}
		m[h]++
	}
	d.br.dirty = true
}

// rebuild publishes a fresh immutable index. Query goroutines pick it up on
// their next atomic load; none of them block.
func (d *Daemon) rebuild() {
	d.br.mu.Lock()
	entries := make([]*core.Entry, 0, len(d.br.entries))
	for _, e := range d.br.entries {
		entries = append(entries, e)
	}
	bg := d.br.bigrams
	tools := d.br.tools
	onPath := d.br.onPath
	d.br.mu.Unlock()

	d.ix.Store(index.Build(entries, bg, tools, onPath, time.Now()))
}

// ---------------- serving ----------------

func (d *Daemon) Serve(ctx context.Context) error {
	ln, activated, err := listen(d.cfg.SocketPath)
	if err != nil {
		return err
	}
	if !activated {
		defer os.Remove(d.cfg.SocketPath)
	}

	go d.ingestLoop(ctx)
	go d.maintenanceLoop(ctx)

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	d.br.mu.Lock()
	log.Printf("ariadned: listening on %s (%d entries, %d tools)",
		d.cfg.SocketPath, len(d.br.entries), len(d.br.tools))
	d.br.mu.Unlock()

	for {
		c, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				d.shutdown()
				return nil
			default:
				return err
			}
		}
		go d.handle(ctx, c)
	}
}

func (d *Daemon) shutdown() {
	d.snapshot()
	d.st.Close()
}

// listen returns the daemon's listener. When systemd passed a socket-
// activated descriptor (LISTEN_FDS=1, fd 3), it is used as-is: the .socket
// unit then owns the path, so the daemon never creates, chmods, or unlinks
// it. That is what lets the unit keep ProtectSystem=strict — /run stays
// read-only in the service's mount namespace — while the socket lives in
// /run. The boolean reports whether activation happened.
func listen(path string) (net.Listener, bool, error) {
	if os.Getenv("LISTEN_FDS") == "1" && os.Getenv("LISTEN_PID") == strconv.Itoa(os.Getpid()) {
		ln, err := net.FileListener(os.NewFile(3, "ariadne.sock"))
		if err != nil {
			return nil, false, fmt.Errorf("socket activation: %w", err)
		}
		return ln, true, nil
	}
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, false, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, false, err
	}
	return ln, false, nil
}

// handle autodetects the codec from the first byte of the connection.
// Uppercase ASCII verb ("QUERY\t...") means the line-based text protocol used
// by the shell widget; anything else is length-prefixed JSON from the Go
// client. One socket, two codecs, no configuration.
func (d *Daemon) handle(ctx context.Context, c net.Conn) {
	defer c.Close()
	br := bufio.NewReaderSize(c, 16<<10)
	first, err := br.Peek(1)
	if err != nil {
		return
	}
	if first[0] >= 'A' && first[0] <= 'Z' {
		d.handleText(ctx, c, br)
		return
	}
	for {
		var req proto.Request
		if err := proto.Read(br, &req); err != nil {
			return
		}
		resp := d.dispatch(ctx, &req)
		if err := proto.Write(c, resp); err != nil {
			return
		}
	}
}

func (d *Daemon) handleText(ctx context.Context, c net.Conn, br *bufio.Reader) {
	bw := bufio.NewWriterSize(c, 16<<10)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		req := proto.ParseTextRequest(line)
		if req == nil {
			bw.WriteString("ERR\tbad request\n.\n")
			bw.Flush()
			continue
		}
		resp := d.dispatch(ctx, req)
		if err := proto.WriteTextResponse(bw, resp); err != nil {
			return
		}
	}
}

func (d *Daemon) dispatch(ctx context.Context, req *proto.Request) *proto.Response {
	switch req.Op {
	case proto.OpQuery:
		return d.doQuery(req)
	case proto.OpIngest:
		return d.doIngest(req)
	case proto.OpAccept:
		return d.doAccept(req, req.Chosen)
	case proto.OpReject:
		return d.doAccept(req, -1)
	case proto.OpStats:
		return d.doStats()
	case proto.OpHarvest:
		go d.runHarvest(context.Background())
		return &proto.Response{OK: true, Text: "harvest started"}
	case proto.OpTrain:
		return d.doTrain()
	case proto.OpForget:
		return d.doForget(req.Pattern)
	case proto.OpImport:
		return d.doImport(req.Events)
	case proto.OpPing:
		return &proto.Response{OK: true, Text: "pong"}
	}
	return &proto.Response{Err: "unknown op: " + req.Op}
}

// ---------------- query ----------------

func (d *Daemon) doQuery(req *proto.Request) *proto.Response {
	t0 := time.Now()
	ix := d.ix.Load()
	w := *d.wts.Load()

	limit := req.Limit
	if limit <= 0 {
		limit = 3
	}

	buf := req.Buffer
	if req.Cursor > 0 && req.Cursor <= len(buf) {
		buf = buf[:req.Cursor]
	}
	typed := core.Normalize(buf)

	d.sessMu.Lock()
	s := d.sessions[req.Session]
	if s == nil {
		s = &session{}
		d.sessions[req.Session] = s
	}
	recent := append([]core.Hash(nil), s.recent...)
	prev := s.prev
	d.sessMu.Unlock()

	ctx := &rank.Context{
		Cwd: req.Cwd, GitRoot: req.GitRoot, GitBranch: req.GitBranch,
		Host: req.Host, PrevHash: prev, SessionCmds: recent,
		LastAccepted: d.acceptedSnap.Load(), Now: time.Now(),
	}

	cands, owns := d.generate(ix, typed, buf, ctx)
	cands = rank.SortAndTrim(w, cands, limit)

	resp := &proto.Response{OK: true, OwnsToken: owns}
	if len(cands) > 0 && strings.HasPrefix(cands[0].Text, typed) &&
		len(cands[0].Text) > len(typed) && cands[0].Source != rank.SrcSpec {
		resp.Ghost = cands[0].Text[len(typed):]
	}
	for _, c := range cands {
		resp.Candidates = append(resp.Candidates, proto.Candidate{
			Text: c.Text, Display: c.Display, Desc: c.Desc,
			Count: c.Count, Context: c.Context,
			Source: c.Source.String(), Score: int(c.Score * 100),
		})
	}

	// Record the impression for the ranker's training set.
	if len(cands) > 0 && req.Session != "" {
		sh := &shown{prefix: typed}
		for _, c := range cands {
			f := make([]float64, rank.NumFeatures)
			copy(f, c.Features[:])
			sh.features = append(sh.features, f)
			sh.hashes = append(sh.hashes, c.Hash)
		}
		d.impMu.Lock()
		d.lastShow[req.Session] = sh
		d.impMu.Unlock()
	}

	el := time.Since(t0).Microseconds()
	resp.ElapsedUS = el
	d.nQuery.Add(1)
	d.latSum.Add(el)
	for {
		old := d.latMax.Load()
		if el <= old || d.latMax.CompareAndSwap(old, el) {
			break
		}
	}
	return resp
}

// generate produces candidates from every source and decides whether Ariadne
// owns the current token at all.
//
// The ownership decision is the most important line of code in the system.
// Getting it wrong means shadowing the shell's live completers — `git checkout
// <TAB>` must reach git, not a stale frecency list.
func (d *Daemon) generate(ix *index.Index, typed, rawBuf string, ctx *rank.Context) ([]rank.Candidate, bool) {
	toks := core.Tokenize(rawBuf)
	tokIdx, fresh := core.TokenAt(toks, len(rawBuf))
	var curTok string
	if !fresh && tokIdx < len(toks) {
		curTok = toks[tokIdx].Text
	}

	typedLow := strings.ToLower(typed)
	out := make([]rank.Candidate, 0, index.MaxCandidates)
	seen := make(map[string]bool, index.MaxCandidates)

	addHist := func(id int32, src rank.Source) {
		e := ix.Get(id)
		if e == nil || e.Redacted || seen[e.Norm] {
			return
		}
		seen[e.Norm] = true
		f := rank.FeaturesFor(ix, e, ix.Lower(id), typed, typedLow, ctx)
		if src == rank.SrcBigram {
			f[rank.FMatchQuality] = 1.0
		}
		c := rank.Candidate{
			Text: e.Norm, Hash: e.Hash, Source: src,
			Count: e.Global.NSuccess, Features: f,
		}
		c.Context = contextLabel(e, ctx)
		out = append(out, c)
	}

	// 1. Empty buffer: predict the next command from the bigram chain.
	if typed == "" {
		if m, ok := ix.Bigrams[ctx.PrevHash]; ok {
			for h := range m {
				if id, ok := ix.ByHash(h); ok {
					addHist(id, rank.SrcBigram)
				}
			}
		}
		for _, id := range ix.PrefixCandidates("") {
			addHist(id, rank.SrcHistory)
		}
		return out, true
	}

	// 2. History prefix pass.
	for _, id := range ix.PrefixCandidates(typed) {
		addHist(id, rank.SrcHistory)
	}
	// 3. Fuzzy pass only if the exact pass came back thin.
	if len(out) < 20 {
		for _, id := range ix.FuzzyCandidates(typed, 60) {
			addHist(id, rank.SrcHistory)
		}
	}

	// 4. Flag completion from harvested specs. This is what carries a brand
	//    new tool that has zero history behind it.
	isFlag := strings.HasPrefix(curTok, "-")
	if len(toks) > 0 {
		tool := core.Argv0(typed)
		if spec := ix.Tools[tool]; spec != nil {
			sub := resolveSubcommand(spec, toks)
			if isFlag {
				prefixTok := curTok
				for _, fl := range spec.FlagsFor(sub) {
					disp := fl.Display()
					if disp == "" {
						continue
					}
					// Match the typed prefix against either written form: a
					// user who types -t should still be offered --tty.
					if !strings.HasPrefix(disp, prefixTok) &&
						!(fl.Short != "" && strings.HasPrefix(fl.Short, prefixTok)) {
						continue
					}
					full := rawBuf[:toks[tokIdx].Start] + disp
					if seen[full] {
						continue
					}
					seen[full] = true
					out = append(out, specCandidate(ix, spec, fl, full, disp, typed, ctx))
				}
			} else if fresh && sub == "" && len(spec.Subcommands) > 0 && len(toks) == 1 {
				for _, sc := range spec.Subcommands {
					full := strings.TrimRight(rawBuf, " ") + " " + sc.Name
					if seen[full] {
						continue
					}
					seen[full] = true
					out = append(out, specCandidate(ix, spec,
						core.Flag{Long: sc.Name, Desc: sc.Desc}, full, sc.Name, typed, ctx))
				}
			}
		}
	}

	// ---- ownership ----
	owns := true
	switch {
	case len(toks) <= 1:
		owns = true // first token: always ours
	case isFlag:
		owns = true // flags: ours
	case looksLikePath(curTok):
		owns = false // paths belong to the shell's file completer, always
	case len(out) == 0:
		owns = false
	default:
		// Argument position with history behind it. Only claim it if a
		// candidate is a genuine continuation, not a fuzzy stretch.
		owns = false
		for _, c := range out {
			if c.Source != rank.SrcSpec && strings.HasPrefix(c.Text, typed) &&
				c.Features[rank.FSuccess] > 0.7 {
				owns = true
				break
			}
		}
	}
	return out, owns
}

func specCandidate(ix *index.Index, spec *core.ToolSpec, fl core.Flag,
	full, disp, typed string, ctx *rank.Context) rank.Candidate {

	var f [rank.NumFeatures]float64
	f[rank.FMatchQuality] = 1.0
	f[rank.FIsSpec] = 1
	f[rank.FSpecConfidence] = spec.Confidence
	f[rank.FToolPresent] = 1
	f[rank.FSuccess] = 0.15 * float64(fl.NUsed)
	saved := float64(len(full) - len(typed))
	if saved > 0 {
		f[rank.FLengthPenalty] = 0.4
	}
	return rank.Candidate{
		Text: full, Display: disp, Desc: fl.Desc,
		Source: rank.SrcSpec, Features: f, Hash: core.HashOf(full),
		Context: spec.Source,
	}
}

func resolveSubcommand(spec *core.ToolSpec, toks []core.Token) string {
	if len(toks) < 2 {
		return ""
	}
	for _, t := range toks[1:] {
		if strings.HasPrefix(t.Text, "-") {
			continue
		}
		for _, sc := range spec.Subcommands {
			if sc.Name == t.Text {
				return sc.Name
			}
		}
		return ""
	}
	return ""
}

func looksLikePath(tok string) bool {
	return strings.ContainsAny(tok, "/*?") ||
		strings.HasPrefix(tok, "~") || strings.HasPrefix(tok, "./") ||
		strings.HasPrefix(tok, "$")
}

func contextLabel(e *core.Entry, ctx *rank.Context) string {
	if _, ok := e.ByCwd[ctx.Cwd]; ok {
		return "cwd"
	}
	if ctx.GitRoot != "" {
		if _, ok := e.ByRepo[ctx.GitRoot]; ok {
			return filepath.Base(ctx.GitRoot)
		}
	}
	return "global"
}

// publishAccepted rebuilds the immutable accepted-map and publishes it.
// Copying this map on every query put a mutex acquisition plus an allocation
// on the keystroke path; feedback events are thousands of times rarer than
// queries, so the copy belongs here instead.
func (d *Daemon) publishAccepted() {
	m := make(map[string]core.Hash, len(d.accepted))
	for k, v := range d.accepted {
		m[k] = v
	}
	d.acceptedSnap.Store(&m)
}

// ---------------- ingest ----------------

func (d *Daemon) doIngest(req *proto.Request) *proto.Response {
	e := req.Event
	if e == nil || strings.TrimSpace(e.Raw) == "" {
		return &proto.Response{OK: true}
	}
	if core.ShouldIgnore(e.Raw, d.cfg.Deny) {
		return &proto.Response{OK: true}
	}
	red, changed := core.Redact(e.Raw)
	e.Raw = red
	e.Norm = core.Normalize(red)
	e.Argv0 = core.Argv0(e.Norm)
	if e.TS == 0 {
		e.TS = time.Now().Unix()
	}
	_ = changed

	d.sessMu.Lock()
	s := d.sessions[e.Session]
	if s == nil {
		s = &session{}
		d.sessions[e.Session] = s
	}
	e.PrevHash = s.prev
	h := e.Hash()
	s.prev = h
	s.recent = append([]core.Hash{h}, s.recent...)
	if len(s.recent) > d.cfg.MaxSessionCmds {
		s.recent = s.recent[:d.cfg.MaxSessionCmds]
	}
	d.sessMu.Unlock()

	// Non-blocking. Losing a history entry is acceptable; blocking a prompt
	// never is.
	select {
	case d.ingestCh <- e:
	default:
	}
	return &proto.Response{OK: true}
}

func (d *Daemon) ingestLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-d.ingestCh:
			d.br.mu.Lock()
			d.apply(e)
			d.br.mu.Unlock()
			d.st.AppendEvent(e)
			d.nIngest.Add(1)
		}
	}
}

// ---------------- feedback ----------------

func (d *Daemon) doAccept(req *proto.Request, chosen int) *proto.Response {
	d.impMu.Lock()
	sh := d.lastShow[req.Session]
	delete(d.lastShow, req.Session)
	if sh == nil {
		d.impMu.Unlock()
		return &proto.Response{OK: true}
	}
	im := store.Impression{
		TS: time.Now().Unix(), Prefix: sh.prefix,
		Features: sh.features, Hashes: sh.hashes, Accepted: chosen,
	}
	d.imps = append(d.imps, im)
	if chosen >= 0 && chosen < len(sh.hashes) {
		d.accepted[sh.prefix] = sh.hashes[chosen]
		d.publishAccepted()
	}
	n := len(d.imps)
	d.impMu.Unlock()

	d.st.AppendImpression(im)
	if d.cfg.TrainEvery > 0 && n%d.cfg.TrainEvery == 0 {
		go d.doTrain()
	}
	return &proto.Response{OK: true}
}

func (d *Daemon) doTrain() *proto.Response {
	d.impMu.Lock()
	imps := append([]store.Impression(nil), d.imps...)
	d.impMu.Unlock()

	cur := *d.wts.Load()
	res := rank.Train(imps, cur, d.cfg.MinTrainSamples)
	info := map[string]any{
		"samples": res.NSamples, "old_mrr": res.OldMRR,
		"new_mrr": res.NewMRR, "promoted": res.Promoted,
		"zeroed_features": res.Zeroed,
	}
	if res.Promoted {
		w := res.Weights
		d.wts.Store(&w)
		d.wtVer.Add(1)
		log.Printf("ariadned: weights promoted v%d, MRR %.3f -> %.3f (n=%d)",
			d.wtVer.Load(), res.OldMRR, res.NewMRR, res.NSamples)
	}
	return &proto.Response{OK: true, Info: info}
}

// ---------------- maintenance ----------------

func (d *Daemon) maintenanceLoop(ctx context.Context) {
	snapT := time.NewTicker(d.cfg.SnapshotEvery)
	rebuildT := time.NewTicker(20 * time.Second)
	harvestT := time.NewTicker(d.cfg.HarvestEvery)
	defer snapT.Stop()
	defer rebuildT.Stop()
	defer harvestT.Stop()

	// The startup harvest walks the whole filesystem. Delay it so that the
	// first interactive keystrokes after a login are not competing with a
	// full $PATH scan for CPU.
	if d.cfg.HarvestOnStart && len(d.br.tools) == 0 {
		go func() {
			select {
			case <-time.After(20 * time.Second):
				d.runHarvest(ctx)
			case <-ctx.Done():
			}
		}()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-rebuildT.C:
			d.br.mu.Lock()
			dirty := d.br.dirty
			d.br.dirty = false
			d.br.mu.Unlock()
			if dirty {
				d.rebuild()
			}
		case <-snapT.C:
			d.snapshot()
		case <-harvestT.C:
			go d.runHarvest(ctx)
		}
	}
}

func (d *Daemon) snapshot() {
	d.br.mu.Lock()
	entries := make([]*core.Entry, 0, len(d.br.entries))
	for _, e := range d.br.entries {
		entries = append(entries, e)
	}
	snap := &store.Snapshot{
		Version: 1, Entries: entries,
		Bigrams: d.br.bigrams, Tools: d.br.tools,
	}
	d.br.mu.Unlock()

	d.impMu.Lock()
	snap.Impressions = d.imps
	d.impMu.Unlock()

	w := *d.wts.Load()
	snap.Weights = w
	snap.WeightsVer = int(d.wtVer.Load())
	if err := d.st.SaveSnapshot(snap); err != nil {
		log.Printf("ariadned: snapshot failed: %v", err)
	}
}

func (d *Daemon) runHarvest(ctx context.Context) {
	opt := harvest.DefaultOptions()
	if d.cfg.Verbose {
		opt.Log = func(f string, a ...any) { log.Printf("harvest: "+f, a...) }
	}
	// The previous specs make LLM synthesis idempotent: the map is only ever
	// replaced wholesale, never mutated, so handing it over unlocked is safe.
	d.br.mu.Lock()
	opt.Prev = d.br.tools
	d.br.mu.Unlock()
	hctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	res, err := harvest.Run(hctx, opt)
	if err != nil {
		log.Printf("ariadned: harvest: %v", err)
		if res == nil {
			return
		}
	}
	d.br.mu.Lock()
	d.br.tools = res.Specs
	d.br.onPath = res.OnPATH
	d.br.dirty = true
	d.br.mu.Unlock()
	d.rebuild()
	log.Printf("ariadned: harvest complete: %d specs for %d binaries",
		len(res.Specs), res.Scanned)
}

// ---------------- admin ----------------

func (d *Daemon) doStats() *proto.Response {
	ix := d.ix.Load()
	n := d.nQuery.Load()
	var avg int64
	if n > 0 {
		avg = d.latSum.Load() / n
	}
	d.impMu.Lock()
	nimp := len(d.imps)
	acc := 0
	rankHist := map[int]int{}
	for _, im := range d.imps {
		if im.Accepted >= 0 {
			acc++
			rankHist[im.Accepted]++
		}
	}
	d.impMu.Unlock()

	w := *d.wts.Load()
	wm := map[string]float64{}
	for i, v := range w {
		wm[rank.FeatureNames[i]] = round3(v)
	}
	bySrc := map[string]int{}
	for _, t := range ix.Tools {
		bySrc[t.Source]++
	}
	return &proto.Response{OK: true, Info: map[string]any{
		"entries":          ix.Len(),
		"tools":            len(ix.Tools),
		"tools_by_source":  bySrc,
		"binaries_on_path": len(ix.ToolsOnPATH),
		"bigram_heads":     len(ix.Bigrams),
		"queries":          n,
		"ingested":         d.nIngest.Load(),
		"latency_avg_us":   avg,
		"latency_max_us":   d.latMax.Load(),
		"impressions":      nimp,
		"accepted":         acc,
		"accept_by_rank":   rankHist,
		"weights_version":  d.wtVer.Load(),
		"weights":          wm,
		"llm_endpoint":     os.Getenv("ARIADNE_LLM_ENDPOINT"),
		"llm_model":        os.Getenv("ARIADNE_LLM_MODEL"),
		"uptime_s":         int64(time.Since(d.startedAt).Seconds()),
		"data_dir":         d.st.Dir(),
	}}
}

func round3(f float64) float64 {
	return float64(int(f*1000+0.5*sign(f))) / 1000
}
func sign(f float64) float64 {
	if f < 0 {
		return -1
	}
	return 1
}

// doImport applies a batch synchronously and rebuilds once. Unlike live
// ingest, it must not drop events on channel pressure: an import that silently
// loses most of a 50k-line history is worse than useless, because the user
// believes it succeeded.
func (d *Daemon) doImport(evs []*core.Event) *proto.Response {
	n := 0
	d.br.mu.Lock()
	var fresh []*core.Event
	// Hashes applied within this batch. Duplicate lines in one history file
	// are real repeat executions and must each count; only commands already
	// known before this import starts are skipped (idempotent re-import).
	applied := make(map[core.Hash]bool)
	for _, e := range evs {
		if e == nil || strings.TrimSpace(e.Raw) == "" {
			continue
		}
		if core.ShouldIgnore(e.Raw, d.cfg.Deny) {
			continue
		}
		red, _ := core.Redact(e.Raw)
		e.Raw = red
		e.Norm = core.Normalize(red)
		e.Argv0 = core.Argv0(e.Norm)
		if e.Norm == "" {
			continue
		}
		if e.TS == 0 {
			e.TS = time.Now().Unix()
		}
		h := e.Hash()
		if _, ok := d.br.entries[h]; ok && !applied[h] {
			// Already known from live ingest or an earlier import — do not
			// count it again. Without this, every re-import inflates counts
			// and dilutes the failure signal with duplicate successes.
			continue
		}
		d.apply(e)
		n++
		applied[h] = true
		fresh = append(fresh, e)
	}
	d.br.mu.Unlock()
	for _, e := range fresh {
		if e.Norm != "" {
			d.st.AppendEvent(e)
		}
	}
	d.rebuild()
	go d.snapshot()
	return &proto.Response{OK: true, Info: map[string]any{"imported": n}}
}

func (d *Daemon) doForget(pattern string) *proto.Response {
	if pattern == "" {
		return &proto.Response{Err: "empty pattern"}
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return &proto.Response{Err: fmt.Sprintf("bad pattern: %v", err)}
	}
	d.br.mu.Lock()
	removed := 0
	for h, e := range d.br.entries {
		if re.MatchString(e.Norm) || re.MatchString(e.Raw) {
			delete(d.br.entries, h)
			delete(d.br.bigrams, h)
			for _, m := range d.br.bigrams {
				delete(m, h)
			}
			removed++
		}
	}
	d.br.dirty = true
	d.br.mu.Unlock()
	d.rebuild()

	// Forget must be durable, not just in-memory, or it is a lie.
	d.snapshot()
	if err := d.rewriteLog(re); err != nil {
		return &proto.Response{Err: err.Error()}
	}
	return &proto.Response{OK: true, Text: fmt.Sprintf("forgot %d entries (memory + log)", removed)}
}

func (d *Daemon) rewriteLog(re *regexp.Regexp) error {
	var kept []*core.Event
	if err := d.st.LoadEvents(func(e *core.Event) {
		if !re.MatchString(e.Norm) && !re.MatchString(e.Raw) {
			cp := *e
			kept = append(kept, &cp)
		}
	}); err != nil {
		return err
	}
	path := filepath.Join(d.st.Dir(), "events.jsonl")
	if err := os.Truncate(path, 0); err != nil {
		return err
	}
	for _, e := range kept {
		d.st.AppendEvent(e)
	}
	return nil
}

// ImportHistory bulk-loads an existing shell history file.
func (d *Daemon) ImportHistory(events []*core.Event) int {
	d.br.mu.Lock()
	var fresh []*core.Event
	applied := make(map[core.Hash]bool)
	for _, e := range events {
		if e == nil || e.Norm == "" {
			continue
		}
		h := e.Hash()
		if _, ok := d.br.entries[h]; ok && !applied[h] {
			continue // idempotent — see doImport
		}
		d.apply(e)
		fresh = append(fresh, e)
		applied[h] = true
	}
	d.br.mu.Unlock()
	for _, e := range fresh {
		d.st.AppendEvent(e)
	}
	d.rebuild()
	d.snapshot()
	return len(fresh)
}

func (d *Daemon) Entries() int {
	return d.ix.Load().Len()
}
