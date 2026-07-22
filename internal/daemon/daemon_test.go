package daemon

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"ariadne/internal/core"
	"ariadne/internal/proto"
)

func boot(t *testing.T) (*Daemon, string, func()) {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "s.sock")
	cfg := DefaultConfig()
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.SocketPath = sock
	cfg.SnapshotEvery = time.Hour
	cfg.HarvestEvery = time.Hour
	cfg.HarvestOnStart = false

	d, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Suppress the startup harvest: it scans the whole filesystem and this
	// test is about the query path.
	d.br.tools = map[string]*core.ToolSpec{}
	d.br.onPath = map[string]bool{"git": true, "kubectl": true, "rg": true, "docker": true}

	ctx, cancel := context.WithCancel(context.Background())
	go d.Serve(ctx)
	for i := 0; i < 100; i++ {
		if c, err := net.Dial("unix", sock); err == nil {
			c.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return d, sock, cancel
}

func rpc(t *testing.T, sock string, req *proto.Request) *proto.Response {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if err := proto.Write(c, req); err != nil {
		t.Fatalf("write: %v", err)
	}
	var resp proto.Response
	if err := proto.Read(c, &resp); err != nil {
		t.Fatalf("read: %v", err)
	}
	return &resp
}

func feed(t *testing.T, d *Daemon, cmds []string, cwd string) {
	t.Helper()
	now := time.Now().Unix()
	var evs []*core.Event
	for i, c := range cmds {
		red, _ := core.Redact(c)
		n := core.Normalize(red)
		evs = append(evs, &core.Event{
			Raw: red, Norm: n, Argv0: core.Argv0(n),
			Cwd: cwd, Host: "testhost", Session: "s1",
			TS: now - int64(len(cmds)-i)*60,
		})
	}
	d.ImportHistory(evs)
}

var corpus = []string{
	"git status", "git status", "git status", "git status",
	"git commit -m wip", "git push origin main",
	"kubectl get pods -n prod", "kubectl get pods -n prod", "kubectl get pods -n prod",
	"kubectl get pods -A",
	"docker compose up -d", "docker compose up -d",
	"docker-compose up -d",
	"systemctl status vllm",
	"rg --no-ignore-vcs TODO",
}

func TestRankingPrefersFrecency(t *testing.T) {
	d, sock, stop := boot(t)
	defer stop()
	feed(t, d, corpus, "/work")

	resp := rpc(t, sock, &proto.Request{
		Op: proto.OpQuery, Buffer: "git", Cursor: 3,
		Cwd: "/work", Host: "testhost", Session: "q", Limit: 3,
	})
	if !resp.OK || len(resp.Candidates) == 0 {
		t.Fatalf("no candidates: %+v", resp)
	}
	if resp.Candidates[0].Text != "git status" {
		t.Errorf("top candidate = %q, want %q (4 uses beats 1)",
			resp.Candidates[0].Text, "git status")
	}
	if resp.Ghost != " status" {
		t.Errorf("ghost = %q, want %q", resp.Ghost, " status")
	}
	if !resp.OwnsToken {
		t.Error("must own the first token")
	}
}

// The ownership decision is the highest-risk logic in the system: claiming a
// token the shell should have handled means shadowing a live completer.
func TestOwnershipDelegatesPaths(t *testing.T) {
	d, sock, stop := boot(t)
	defer stop()
	feed(t, d, corpus, "/work")

	cases := []struct {
		buf      string
		wantOwns bool
		why      string
	}{
		{"git", true, "first token is always ours"},
		{"g", true, "first token, partial"},
		{"kubectl get pods -n", true, "flag token is ours"},
		{"cat /etc/pas", false, "path must reach the shell's file completer"},
		{"cat ./src/ma", false, "relative path must delegate"},
		{"vim ~/note", false, "tilde path must delegate"},
		{"git checkout featur", false, "unknown branch name, no history: delegate to git"},
		{"echo $HO", false, "variable expansion belongs to the shell"},
	}
	for _, tc := range cases {
		resp := rpc(t, sock, &proto.Request{
			Op: proto.OpQuery, Buffer: tc.buf, Cursor: len(tc.buf),
			Cwd: "/work", Host: "testhost", Session: "q", Limit: 3,
		})
		if resp.OwnsToken != tc.wantOwns {
			t.Errorf("owns(%q) = %v, want %v — %s",
				tc.buf, resp.OwnsToken, tc.wantOwns, tc.why)
		}
	}
}

func TestSpecFlagCompletion(t *testing.T) {
	d, sock, stop := boot(t)
	defer stop()
	d.br.mu.Lock()
	d.br.tools["rg"] = &core.ToolSpec{
		Name: "rg", Source: "fish", Confidence: 0.85,
		Flags: []core.Flag{
			{Long: "--no-ignore-vcs", Arg: core.ArgNone, Desc: "Do not respect VCS ignore files"},
			{Long: "--no-heading", Arg: core.ArgNone, Desc: "Do not group matches by file"},
			{Long: "--type", Short: "-t", Arg: core.ArgRequired, Desc: "Only search files of TYPE"},
		},
	}
	d.br.mu.Unlock()
	d.rebuild()

	resp := rpc(t, sock, &proto.Request{
		Op: proto.OpQuery, Buffer: "rg --no-", Cursor: 8,
		Cwd: "/work", Host: "testhost", Session: "q", Limit: 5,
	})
	got := map[string]bool{}
	for _, c := range resp.Candidates {
		got[c.Text] = true
	}
	// Zero history for rg: this must come entirely from the harvested spec.
	if !got["rg --no-ignore-vcs"] || !got["rg --no-heading"] {
		t.Errorf("spec flags missing; got %v", keys(got))
	}
	if got["rg --type"] {
		t.Error("--type does not match prefix --no-")
	}
	if !resp.OwnsToken {
		t.Error("flag tokens are ours")
	}
}

func TestBigramPredictsNextCommand(t *testing.T) {
	d, sock, stop := boot(t)
	defer stop()

	// Establish a habit: `git add -A` is nearly always followed by a commit.
	c, err := net.Dial("unix", d.cfg.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	send := func(cmd string) {
		proto.Write(c, &proto.Request{Op: proto.OpIngest, Event: &core.Event{
			Raw: cmd, Cwd: "/work", Host: "testhost", Session: "habit",
			TS: time.Now().Unix(),
		}})
		var r proto.Response
		proto.Read(c, &r)
	}
	for i := 0; i < 8; i++ {
		send("git add -A")
		send("git commit -m wip")
	}
	send("git add -A")
	time.Sleep(300 * time.Millisecond)
	d.rebuild()

	resp := rpc(t, sock, &proto.Request{
		Op: proto.OpQuery, Buffer: "", Cursor: 0,
		Cwd: "/work", Host: "testhost", Session: "habit", Limit: 3,
	})
	if len(resp.Candidates) == 0 {
		t.Fatal("no next-command prediction at an empty prompt")
	}
	if resp.Candidates[0].Text != "git commit -m wip" {
		t.Errorf("next-command prediction = %q, want %q",
			resp.Candidates[0].Text, "git commit -m wip")
	}
}

func TestSecretsAreRedactedAndNeverSuggested(t *testing.T) {
	d, sock, stop := boot(t)
	defer stop()

	secret := `curl -H "Authorization: Bearer sk-abcdef1234567890abcdefzz" https://api.example.com`
	rpc(t, sock, &proto.Request{Op: proto.OpIngest, Event: &core.Event{
		Raw: secret, Cwd: "/work", Host: "testhost", Session: "s", TS: time.Now().Unix(),
	}})
	time.Sleep(300 * time.Millisecond)
	d.rebuild()

	resp := rpc(t, sock, &proto.Request{
		Op: proto.OpQuery, Buffer: "curl", Cursor: 4,
		Cwd: "/work", Host: "testhost", Session: "q", Limit: 5,
	})
	for _, c := range resp.Candidates {
		if strings.Contains(c.Text, "sk-abcdef") {
			t.Fatalf("credential leaked into a suggestion: %q", c.Text)
		}
	}
	// It must not be offered at all, redacted or otherwise: a command with a
	// scrubbed secret in it is useless to re-run and dangerous to display.
	if len(resp.Candidates) != 0 {
		t.Errorf("redacted command was still suggested: %+v", resp.Candidates)
	}
}

func TestFailuresAreRecordedAsNegativeSignal(t *testing.T) {
	d, sock, stop := boot(t)
	defer stop()

	now := time.Now().Unix()
	var evs []*core.Event
	for i := 0; i < 6; i++ {
		evs = append(evs, &core.Event{
			Raw: "docker-compose up", Norm: "docker-compose up", Argv0: "docker-compose",
			Cwd: "/work", Host: "testhost", ExitCode: 127, TS: now - int64(600-i),
		})
		evs = append(evs, &core.Event{
			Raw: "docker compose up", Norm: "docker compose up", Argv0: "docker",
			Cwd: "/work", Host: "testhost", ExitCode: 0, TS: now - int64(590-i),
		})
	}
	d.ImportHistory(evs)

	resp := rpc(t, sock, &proto.Request{
		Op: proto.OpQuery, Buffer: "docker", Cursor: 6,
		Cwd: "/work", Host: "testhost", Session: "q", Limit: 3,
	})
	if len(resp.Candidates) == 0 {
		t.Fatal("no candidates")
	}
	// Equal counts, but one always exits 127 and the binary is not on PATH.
	if resp.Candidates[0].Text != "docker compose up" {
		t.Errorf("top = %q, want the variant that actually works", resp.Candidates[0].Text)
	}
}

// The latency budget is the architecture. If this fails, the design is wrong,
// not the test.
func TestLatencyBudget(t *testing.T) {
	d, sock, stop := boot(t)
	defer stop()

	// Synthetic history at a realistic scale.
	now := time.Now().Unix()
	var evs []*core.Event
	tools := []string{"git", "kubectl", "docker", "systemctl", "rg", "vim", "ssh", "curl"}
	for i := 0; i < 50000; i++ {
		cmd := fmt.Sprintf("%s sub%d --flag%d value%d", tools[i%len(tools)], i%97, i%31, i%13)
		evs = append(evs, &core.Event{
			Raw: cmd, Norm: cmd, Argv0: tools[i%len(tools)],
			Cwd: fmt.Sprintf("/work/p%d", i%50), Host: "testhost",
			TS: now - int64(i),
		})
	}
	d.ImportHistory(evs)
	if d.Entries() < 40000 {
		t.Fatalf("expected ~50k entries, got %d", d.Entries())
	}

	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	probes := []string{"", "g", "gi", "git", "git s", "kubectl get", "docker ru", "rg --", "systemctl st"}
	var samples []int64
	for i := 0; i < 3000; i++ {
		b := probes[i%len(probes)]
		t0 := time.Now()
		c.SetDeadline(time.Now().Add(2 * time.Second))
		if err := proto.Write(c, &proto.Request{
			Op: proto.OpQuery, Buffer: b, Cursor: len(b),
			Cwd: "/work/p3", Host: "testhost", Session: "bench", Limit: 3,
		}); err != nil {
			t.Fatal(err)
		}
		var r proto.Response
		if err := proto.Read(c, &r); err != nil {
			t.Fatal(err)
		}
		samples = append(samples, time.Since(t0).Microseconds())
	}
	sort.Slice(samples, func(a, b int) bool { return samples[a] < samples[b] })
	p := func(q float64) int64 { return samples[int(float64(len(samples)-1)*q)] }
	p50, p99, max := p(0.50), p(0.99), samples[len(samples)-1]
	t.Logf("entries=%d  p50=%.2fms  p90=%.2fms  p99=%.2fms  max=%.2fms",
		d.Entries(), f(p50), f(p(0.90)), f(p99), f(max))

	if p50 > 3000 {
		t.Errorf("p50 %.2fms exceeds the 3ms budget", f(p50))
	}
	if p99 > 10000 {
		t.Errorf("p99 %.2fms exceeds the 10ms budget", f(p99))
	}
}

func f(us int64) float64 { return float64(us) / 1000 }

// Both codecs must work on the same socket, autodetected.
func TestTextCodec(t *testing.T) {
	d, sock, stop := boot(t)
	defer stop()
	feed(t, d, corpus, "/work")

	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))

	req := "QUERY\tbuf=git\tcur=3\tcwd=/work\thost=testhost\tsess=z\tn=3\n"
	if _, err := c.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	var lines []string
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		l = strings.TrimRight(l, "\n")
		if l == "." {
			break
		}
		lines = append(lines, l)
	}
	joined := strings.Join(lines, "|")
	if !strings.Contains(joined, "OWNS\t1") {
		t.Errorf("missing OWNS in text response: %q", joined)
	}
	if !strings.Contains(joined, "CAND\tgit status") {
		t.Errorf("missing candidate in text response: %q", joined)
	}
	if !strings.Contains(joined, "GHOST\t status") {
		t.Errorf("missing ghost in text response: %q", joined)
	}
}

func TestForgetIsDurable(t *testing.T) {
	d, sock, stop := boot(t)
	defer stop()
	feed(t, d, append(corpus, "ssh root@prod-db-01"), "/work")

	before := rpc(t, sock, &proto.Request{
		Op: proto.OpQuery, Buffer: "ssh", Cursor: 3,
		Cwd: "/work", Host: "testhost", Session: "q", Limit: 3,
	})
	if len(before.Candidates) == 0 {
		t.Fatal("setup: ssh command not indexed")
	}
	r := rpc(t, sock, &proto.Request{Op: proto.OpForget, Pattern: "prod-db-01"})
	if !r.OK {
		t.Fatalf("forget failed: %v", r.Err)
	}
	after := rpc(t, sock, &proto.Request{
		Op: proto.OpQuery, Buffer: "ssh", Cursor: 3,
		Cwd: "/work", Host: "testhost", Session: "q", Limit: 3,
	})
	if len(after.Candidates) != 0 {
		t.Errorf("forget left %d candidates behind", len(after.Candidates))
	}
	// And it must be gone from disk, not just from memory.
	n := 0
	d.st.LoadEvents(func(e *core.Event) {
		if strings.Contains(e.Raw, "prod-db-01") {
			n++
		}
	})
	if n != 0 {
		t.Errorf("forget left %d events in the durable log", n)
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
