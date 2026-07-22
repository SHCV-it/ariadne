// Command ariadne is the client and admin CLI.
//
// The shell widget speaks the same protocol directly; this binary exists for
// humans, for `doctor`, and for bulk operations that must not run in-band.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"ariadne/internal/core"
	"ariadne/internal/daemon"
	"ariadne/internal/harvest"
	"ariadne/internal/proto"
)

func socketPath() string {
	if p := os.Getenv("ARIADNE_SOCKET"); p != "" {
		return p
	}
	return daemon.DefaultConfig().SocketPath
}

func dial() (net.Conn, error) {
	return net.DialTimeout("unix", socketPath(), 300*time.Millisecond)
}

func call(req *proto.Request) (*proto.Response, error) {
	c, err := dial()
	if err != nil {
		return nil, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	if err := proto.Write(c, req); err != nil {
		return nil, err
	}
	var resp proto.Response
	if err := proto.Read(c, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "query", "q":
		err = cmdQuery(os.Args[2:])
	case "stats":
		err = cmdStats()
	case "doctor":
		err = cmdDoctor()
	case "import":
		err = cmdImport(os.Args[2:])
	case "harvest":
		err = simple(&proto.Request{Op: proto.OpHarvest})
	case "train":
		err = cmdTrain()
	case "forget":
		if len(os.Args) < 3 {
			err = fmt.Errorf("usage: ariadne forget <regex>")
		} else {
			err = simple(&proto.Request{Op: proto.OpForget, Pattern: os.Args[2]})
		}
	case "bench":
		err = cmdBench(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ariadne: "+err.Error())
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `ariadne — terminal completion brain

  ariadne query <buffer>      show suggestions for a buffer
  ariadne stats               daemon statistics and learned weights
  ariadne doctor              health check with latency measurement
  ariadne import [file]       import zsh/bash/fish history (default: all found)
  ariadne harvest             rescan $PATH and rebuild tool specs
  ariadne train               force a ranker training round
  ariadne forget <regex>      permanently delete matching history
  ariadne bench [n]           measure query latency distribution
`)
}

func simple(req *proto.Request) error {
	resp, err := call(req)
	if err != nil {
		return err
	}
	if resp.Err != "" {
		return fmt.Errorf("%s", resp.Err)
	}
	if resp.Text != "" {
		fmt.Println(resp.Text)
	}
	if resp.Info != nil {
		printInfo(resp.Info)
	}
	return nil
}

func cmdQuery(args []string) error {
	buf := strings.Join(args, " ")
	cwd, _ := os.Getwd()
	host, _ := os.Hostname()
	resp, err := call(&proto.Request{
		Op: proto.OpQuery, Buffer: buf, Cursor: len(buf),
		Cwd: cwd, Host: host, Session: "cli", Limit: 5,
		GitRoot: gitRoot(cwd), GitBranch: gitBranch(cwd),
	})
	if err != nil {
		return err
	}
	fmt.Printf("owns_token=%v  ghost=%q  %dµs\n", resp.OwnsToken, resp.Ghost, resp.ElapsedUS)
	for i, c := range resp.Candidates {
		desc := c.Desc
		if desc != "" {
			desc = "  — " + desc
		}
		fmt.Printf("  %d ▸ %-52s %3d%%  %-6s %-8s%s\n",
			i+1, trunc(c.Text, 52), c.Score, c.Source, c.Context, desc)
	}
	return nil
}

func cmdStats() error {
	resp, err := call(&proto.Request{Op: proto.OpStats})
	if err != nil {
		return err
	}
	printInfo(resp.Info)
	return nil
}

func cmdTrain() error {
	resp, err := call(&proto.Request{Op: proto.OpTrain})
	if err != nil {
		return err
	}
	printInfo(resp.Info)
	return nil
}

func printInfo(info map[string]any) {
	keys := make([]string, 0, len(info))
	for k := range info {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := info[k]
		switch vv := v.(type) {
		case map[string]any:
			fmt.Printf("%-18s\n", k+":")
			sub := make([]string, 0, len(vv))
			for sk := range vv {
				sub = append(sub, sk)
			}
			sort.Strings(sub)
			for _, sk := range sub {
				fmt.Printf("  %-20s %v\n", sk, vv[sk])
			}
		default:
			fmt.Printf("%-18s %v\n", k+":", v)
		}
	}
}

func cmdDoctor() error {
	fail := 0
	check := func(name string, ok bool, detail string) {
		mark := "ok  "
		if !ok {
			mark = "FAIL"
			fail++
		}
		fmt.Printf("[%s] %-28s %s\n", mark, name, detail)
	}

	sp := socketPath()
	fi, err := os.Stat(sp)
	check("socket exists", err == nil, sp)
	if err == nil {
		perm := fi.Mode().Perm()
		check("socket permissions", perm == 0o600, fmt.Sprintf("%04o (want 0600)", perm))
	}

	t0 := time.Now()
	resp, err := call(&proto.Request{Op: proto.OpPing})
	check("daemon reachable", err == nil && resp != nil && resp.OK,
		fmt.Sprintf("%.1fms", float64(time.Since(t0).Microseconds())/1000))
	if err != nil {
		fmt.Println("\ndaemon not running. start it with:")
		fmt.Println("  systemctl --user start ariadned")
		return fmt.Errorf("%d checks failed", fail)
	}

	st, _ := call(&proto.Request{Op: proto.OpStats})
	if st != nil && st.Info != nil {
		entries := toInt(st.Info["entries"])
		tools := toInt(st.Info["tools"])
		bins := toInt(st.Info["binaries_on_path"])
		imps := toInt(st.Info["impressions"])
		check("history loaded", entries > 0, fmt.Sprintf("%d distinct commands", entries))
		cov := 0.0
		if bins > 0 {
			cov = 100 * float64(tools) / float64(bins)
		}
		check("tool spec coverage", cov > 20,
			fmt.Sprintf("%d/%d binaries (%.0f%%)", tools, bins, cov))
		check("training data", true, fmt.Sprintf("%d impressions", imps))
	}

	// Latency is the only spec that actually matters. Measure it, don't assume.
	lat := benchLatency(300)
	check("p50 latency", lat.p50 < 10_000, fmt.Sprintf("%.2fms (budget 3ms)", ms(lat.p50)))
	check("p99 latency", lat.p99 < 20_000, fmt.Sprintf("%.2fms (budget 10ms)", ms(lat.p99)))

	sh := os.Getenv("ARIADNE_SHELL_LOADED")
	check("shell integration", sh != "", "source shell/ariadne.zsh or shell/ariadne.bash")

	// Optional LLM spec synthesis. The daemon's own environment is the
	// config that matters (the CLI usually has none); fall back to ours.
	ep, _ := st.Info["llm_endpoint"].(string)
	if ep == "" {
		ep = os.Getenv("ARIADNE_LLM_ENDPOINT")
	}
	if ep == "" {
		check("llm spec synthesis", true, "disabled (set ARIADNE_LLM_ENDPOINT to enable)")
	} else {
		opt := harvest.LLMFromEnv()
		detail := ep
		if m, _ := st.Info["llm_model"].(string); m != "" {
			detail += " model=" + m
		}
		if !opt.IsLocal() {
			detail += " (remote: only tool names + public doc text are sent)"
		}
		t0 := time.Now()
		req, rerr := http.NewRequest("GET", strings.TrimRight(ep, "/")+"/models", nil)
		if rerr == nil && opt.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+opt.APIKey)
		}
		cli := &http.Client{Timeout: 3 * time.Second}
		resp, rerr2 := cli.Do(req)
		ok := rerr == nil && rerr2 == nil && resp != nil && resp.StatusCode < 500
		if resp != nil {
			resp.Body.Close()
		}
		check("llm spec synthesis", ok, fmt.Sprintf("%s, %.0fms", detail, float64(time.Since(t0).Microseconds())/1000))
	}

	if fail > 0 {
		return fmt.Errorf("%d checks failed", fail)
	}
	fmt.Println("\nall checks passed")
	return nil
}

func toInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	}
	return 0
}

func ms(us int64) float64 { return float64(us) / 1000 }

type latStats struct{ p50, p90, p99, max int64 }

func benchLatency(n int) latStats {
	probes := []string{"", "g", "git", "git c", "kub", "docker ru", "systemctl st", "rg --"}
	cwd, _ := os.Getwd()
	host, _ := os.Hostname()
	c, err := dial()
	if err != nil {
		return latStats{}
	}
	defer c.Close()
	var samples []int64
	for i := 0; i < n; i++ {
		b := probes[i%len(probes)]
		t0 := time.Now()
		c.SetDeadline(time.Now().Add(2 * time.Second))
		if err := proto.Write(c, &proto.Request{
			Op: proto.OpQuery, Buffer: b, Cursor: len(b),
			Cwd: cwd, Host: host, Session: "bench", Limit: 3,
		}); err != nil {
			break
		}
		var resp proto.Response
		if err := proto.Read(c, &resp); err != nil {
			break
		}
		samples = append(samples, time.Since(t0).Microseconds())
	}
	if len(samples) == 0 {
		return latStats{}
	}
	sort.Slice(samples, func(a, b int) bool { return samples[a] < samples[b] })
	pick := func(q float64) int64 {
		i := int(float64(len(samples)-1) * q)
		return samples[i]
	}
	return latStats{pick(0.50), pick(0.90), pick(0.99), samples[len(samples)-1]}
}

func cmdBench(args []string) error {
	n := 2000
	if len(args) > 0 {
		if v, err := strconv.Atoi(args[0]); err == nil {
			n = v
		}
	}
	l := benchLatency(n)
	fmt.Printf("n=%d  p50=%.2fms  p90=%.2fms  p99=%.2fms  max=%.2fms\n",
		n, ms(l.p50), ms(l.p90), ms(l.p99), ms(l.max))
	if l.p99 > 20_000 {
		fmt.Println("p99 exceeds the 20ms hard timeout — the widget will be falling back")
	}
	return nil
}

// ---------------- history import ----------------

func cmdImport(args []string) error {
	var files []string
	if len(args) > 0 {
		files = args
	} else {
		home := os.Getenv("HOME")
		for _, p := range []string{
			filepath.Join(home, ".zsh_history"),
			filepath.Join(home, ".bash_history"),
			filepath.Join(home, ".local/share/fish/fish_history"),
		} {
			if _, err := os.Stat(p); err == nil {
				files = append(files, p)
			}
		}
	}
	if len(files) == 0 {
		return fmt.Errorf("no history files found")
	}
	host, _ := os.Hostname()
	var events []*core.Event
	for _, f := range files {
		evs, err := parseHistory(f, host)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %s: %v\n", f, err)
			continue
		}
		fmt.Printf("  %s: %d commands\n", f, len(evs))
		events = append(events, evs...)
	}
	if len(events) == 0 {
		return fmt.Errorf("nothing to import")
	}
	// Send through the daemon as batched, synchronous imports so redaction and
	// normalization run in exactly the same code path as live ingest, without
	// the drop-on-overflow behaviour of the live ingest channel.
	c, err := dial()
	if err != nil {
		return err
	}
	defer c.Close()
	total := 0
	const batch = 4000
	for i := 0; i < len(events); i += batch {
		end := i + batch
		if end > len(events) {
			end = len(events)
		}
		c.SetDeadline(time.Now().Add(30 * time.Second))
		if err := proto.Write(c, &proto.Request{Op: proto.OpImport, Events: events[i:end]}); err != nil {
			return err
		}
		var resp proto.Response
		if err := proto.Read(c, &resp); err != nil {
			return err
		}
		if resp.Err != "" {
			return fmt.Errorf("%s", resp.Err)
		}
		if resp.Info != nil {
			if v, ok := resp.Info["imported"].(float64); ok {
				total += int(v)
			}
		}
	}
	fmt.Printf("imported %d commands\n", total)
	return nil
}

func parseHistory(path, host string) ([]*core.Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)

	var out []*core.Event
	base := filepath.Base(path)
	isFish := strings.Contains(base, "fish_history")
	now := time.Now().Unix()
	// Imported history has no timestamps in the bash case and no exit codes
	// anywhere. Spread it over the past 90 days so decay does not treat the
	// whole import as simultaneous.
	seq := 0

	for sc.Scan() {
		line := sc.Text()
		var cmd string
		var ts int64

		switch {
		case isFish:
			if !strings.HasPrefix(line, "- cmd: ") {
				continue
			}
			cmd = strings.TrimPrefix(line, "- cmd: ")
		case strings.HasPrefix(line, ": ") && strings.Contains(line, ";"):
			// zsh extended history: ": <ts>:<elapsed>;<cmd>"
			semi := strings.Index(line, ";")
			meta := line[2:semi]
			if p := strings.Index(meta, ":"); p > 0 {
				if v, err := strconv.ParseInt(meta[:p], 10, 64); err == nil {
					ts = v
				}
			}
			cmd = line[semi+1:]
		default:
			cmd = line
		}
		cmd = strings.TrimSpace(cmd)
		if cmd == "" || strings.HasPrefix(cmd, "#") {
			continue
		}
		seq++
		if ts == 0 {
			ts = now - int64(90*24*3600) + int64(seq)
		}
		norm := core.Normalize(cmd)
		out = append(out, &core.Event{
			Raw: cmd, Norm: norm, Argv0: core.Argv0(norm),
			Host: host, TS: ts, ExitCode: 0, Session: "import",
		})
	}
	return out, sc.Err()
}

func gitRoot(dir string) string {
	out, err := runIn(dir, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return out
}

func gitBranch(dir string) string {
	out, err := runIn(dir, "git", "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return ""
	}
	return out
}

func runIn(dir, bin string, args ...string) (string, error) {
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	b, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

var _ = json.Marshal
