// Package harvest builds tool specifications so completion works before any
// history exists.
//
// Source priority is deliberate and non-obvious: parsing --help output is the
// LAST resort, not the first. Existing structured completion data (carapace,
// fish, zsh) covers the overwhelming majority of tools a working sysadmin
// touches, is already normalized, and is maintained by other people.
package harvest

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"ariadne/internal/core"
)

type Result struct {
	Specs   map[string]*core.ToolSpec
	OnPATH  map[string]bool
	Scanned int
	Errors  []string
}

type Options struct {
	// AllowHelpExec gates execution of unknown binaries entirely.
	AllowHelpExec bool
	// PackageOwnedOnly restricts --help execution to package-manager-owned
	// binaries. Anything in ~/.local/bin from a forgotten curl|bash is skipped.
	PackageOwnedOnly bool
	HelpTimeout      time.Duration
	MaxHelpExecs     int
	FishDirs         []string
	ZshDirs          []string
	ManDirs          []string
	Blacklist        []*regexp.Regexp
	LLM              LLMOptions
	Log              func(string, ...any)
}

func DefaultOptions() Options {
	return Options{
		AllowHelpExec:    true,
		PackageOwnedOnly: true,
		HelpTimeout:      2 * time.Second,
		MaxHelpExecs:     400,
		FishDirs: []string{
			"/usr/share/fish/completions",
			"/usr/share/fish/vendor_completions.d",
			os.ExpandEnv("$HOME/.config/fish/completions"),
		},
		ZshDirs: []string{
			"/usr/share/zsh/functions/Completion/Unix",
			"/usr/share/zsh/site-functions",
		},
		ManDirs: []string{"/usr/share/man", "/usr/local/share/man"},
		Blacklist: []*regexp.Regexp{
			regexp.MustCompile(`d$`), // daemons: sshd, systemd, ...
			regexp.MustCompile(`^(reboot|poweroff|halt|shutdown|init|rm|dd|mkfs.*|fdisk|passwd|su|sudo|login|nologin)$`),
			regexp.MustCompile(`(vi|vim|nvim|emacs|nano|less|more|top|htop|man|ssh|telnet|ftp|python|python3|node|irb|psql|mysql|sqlite3)$`),
		},
		LLM: LLMFromEnv(),
		Log: func(string, ...any) {},
	}
}

// ScanPATH enumerates executables reachable via $PATH.
func ScanPATH() (map[string]string, error) {
	out := map[string]string{}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, ent := range ents {
			if ent.IsDir() {
				continue
			}
			name := ent.Name()
			if _, seen := out[name]; seen {
				continue // first on PATH wins, matching shell resolution
			}
			info, err := ent.Info()
			if err != nil {
				continue
			}
			if info.Mode()&0o111 == 0 {
				continue
			}
			out[name] = filepath.Join(dir, name)
		}
	}
	return out, nil
}

// Run executes the full harvest in priority order. Later sources never
// overwrite a spec from a higher-confidence earlier source.
func Run(ctx context.Context, opt Options) (*Result, error) {
	if opt.Log == nil {
		opt.Log = func(string, ...any) {}
	}
	bins, err := ScanPATH()
	if err != nil {
		return nil, err
	}
	res := &Result{
		Specs:   map[string]*core.ToolSpec{},
		OnPATH:  map[string]bool{},
		Scanned: len(bins),
	}
	for n := range bins {
		res.OnPATH[n] = true
	}

	add := func(s *core.ToolSpec) {
		if s == nil || s.Name == "" || len(s.Flags)+len(s.Subcommands) == 0 {
			return
		}
		if prev, ok := res.Specs[s.Name]; ok && prev.Confidence >= s.Confidence {
			return
		}
		if p, ok := bins[s.Name]; ok {
			s.AbsPath = p
		}
		s.UpdatedAt = time.Now().Unix()
		res.Specs[s.Name] = s
	}

	// 1. carapace: highest confidence, fully structured.
	for _, s := range harvestCarapace(ctx, bins) {
		add(s)
	}
	opt.Log("carapace: %d specs", len(res.Specs))

	// 2. fish completions: declarative, easy to parse, wide coverage.
	n0 := len(res.Specs)
	for _, dir := range opt.FishDirs {
		for _, s := range harvestFishDir(dir) {
			add(s)
		}
	}
	opt.Log("fish: +%d specs", len(res.Specs)-n0)

	// 3. zsh _functions: messier, lower confidence, still worth it.
	n0 = len(res.Specs)
	for _, dir := range opt.ZshDirs {
		for _, s := range harvestZshDir(dir) {
			add(s)
		}
	}
	opt.Log("zsh: +%d specs", len(res.Specs)-n0)

	// 4. man page roff SOURCE (not rendered text: rendering destroys the
	//    structure that makes parsing tractable).
	n0 = len(res.Specs)
	for name := range bins {
		if _, have := res.Specs[name]; have {
			continue
		}
		if p := findManPage(name, opt.ManDirs); p != "" {
			add(parseManRoff(name, p))
		}
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		default:
		}
	}
	opt.Log("man: +%d specs", len(res.Specs)-n0)

	// 5. sandboxed --help, last resort, budgeted.
	//
	// Check for a sandbox ONCE, before anything else. Without bwrap or
	// systemd-run we will not execute a single binary, so there is no point
	// forking the package manager a few hundred times to find that out.
	if opt.AllowHelpExec && !has("bwrap") && !has("systemd-run") {
		opt.Log("help: skipped, no sandbox (bwrap/systemd-run) available")
		opt.AllowHelpExec = false
	}
	if opt.AllowHelpExec {
		n0 = len(res.Specs)
		execs := 0
		for name, path := range bins {
			if _, have := res.Specs[name]; have {
				continue
			}
			if execs >= opt.MaxHelpExecs {
				break
			}
			if blacklisted(name, opt.Blacklist) {
				continue
			}
			if opt.PackageOwnedOnly && !packageOwned(path) {
				continue
			}
			execs++
			if out, ok := runHelpSandboxed(ctx, path, opt.HelpTimeout); ok {
				add(parseHelp(name, out))
			}
		}
		opt.Log("help: +%d specs from %d execs", len(res.Specs)-n0, execs)
	}

	// 6. LLM synthesis: the mop-up for everything the deterministic sources
	//    missed, and the only source that extracts subcommands from raw
	//    documentation text. Runs last, budgeted, and never overwrites.
	if opt.LLM.Enabled() {
		if !opt.LLM.IsLocal() {
			opt.Log("llm: endpoint %s is NOT loopback; only tool names and public documentation text are ever sent", opt.LLM.Endpoint)
		}
		if err := llmPreflight(ctx, &opt.LLM); err != nil {
			opt.Log("llm: %v; synthesis skipped", err)
			return res, nil
		}
		n0 = len(res.Specs)
		attempts := 0
		for name, path := range bins {
			if _, have := res.Specs[name]; have {
				continue
			}
			if attempts >= opt.LLM.MaxTools {
				break
			}
			if blacklisted(name, opt.Blacklist) {
				continue
			}
			// man text first (complete, authoritative); --help as fallback.
			text := ""
			if p := findManPage(name, opt.ManDirs); p != "" {
				if body, err := readMaybeCompressed(p); err == nil {
					text = body
				}
			}
			if text == "" && opt.AllowHelpExec && (!opt.PackageOwnedOnly || packageOwned(path)) {
				if out, ok := runHelpSandboxed(ctx, path, opt.HelpTimeout); ok {
					text = out
				}
			}
			if text == "" {
				continue
			}
			attempts++
			spec, err := SynthesizeSpec(ctx, opt.LLM, name, text)
			if err != nil {
				opt.Log("llm: %s: %v", name, err)
				if errors.Is(err, errLLMUnavailable) {
					break // endpoint died mid-run: stop hammering it
				}
				continue
			}
			add(spec)
			select {
			case <-ctx.Done():
				return res, ctx.Err()
			default:
			}
		}
		opt.Log("llm: +%d specs from %d attempts", len(res.Specs)-n0, attempts)
	}
	return res, nil
}

func blacklisted(name string, bl []*regexp.Regexp) bool {
	for _, re := range bl {
		if re.MatchString(name) {
			return true
		}
	}
	return false
}

// packageOwned asks the system package manager whether it installed this file.
// Unowned binaries are the ones most likely to be hostile or interactive.
func packageOwned(path string) bool {
	type pm struct {
		bin  string
		args []string
	}
	for _, p := range []pm{
		{"pacman", []string{"-Qo", path}},
		{"dpkg", []string{"-S", path}},
		{"rpm", []string{"-qf", path}},
	} {
		if _, err := exec.LookPath(p.bin); err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := exec.CommandContext(ctx, p.bin, p.args...).Run()
		cancel()
		return err == nil
	}
	return false // no package manager: assume not owned, do not execute
}

// runHelpSandboxed executes <bin> --help under the tightest confinement
// available. Executing arbitrary binaries from $PATH to scrape their help text
// is a real code-execution surface; it gets a sandbox or it does not run.
func runHelpSandboxed(ctx context.Context, path string, timeout time.Duration) (string, bool) {
	cctx, cancel := context.WithTimeout(ctx, timeout+time.Second)
	defer cancel()

	var cmd *exec.Cmd
	switch {
	case has("bwrap"):
		cmd = exec.CommandContext(cctx, "bwrap",
			"--ro-bind", "/usr", "/usr",
			"--ro-bind-try", "/etc", "/etc",
			"--symlink", "usr/bin", "/bin",
			"--symlink", "usr/lib", "/lib",
			"--symlink", "usr/lib64", "/lib64",
			"--proc", "/proc", "--dev", "/dev",
			"--tmpfs", "/home", "--tmpfs", "/tmp", "--tmpfs", "/root",
			"--unshare-all", "--die-with-parent", "--new-session",
			"--setenv", "HOME", "/tmp",
			"--", "timeout", "-k", "1", trimSec(timeout), path, "--help")
	case has("systemd-run"):
		cmd = exec.CommandContext(cctx, "systemd-run", "--user", "--scope", "--quiet", "--pipe",
			"-p", "PrivateNetwork=yes", "-p", "ProtectHome=yes",
			"-p", "ProtectSystem=strict", "-p", "NoNewPrivileges=yes",
			"-p", "MemoryMax=256M",
			"--", "timeout", "-k", "1", trimSec(timeout), path, "--help")
	default:
		// No sandbox available: refuse. A missing sandbox is not a reason to
		// execute unknown binaries; it is a reason to skip this source.
		return "", false
	}
	cmd.Stdin = nil
	out, err := cmd.Output()
	if len(out) < 40 {
		if err != nil {
			return "", false
		}
	}
	return string(out), len(out) >= 40
}

func has(bin string) bool { _, err := exec.LookPath(bin); return err == nil }

func trimSec(d time.Duration) string {
	s := int(d.Seconds())
	if s < 1 {
		s = 1
	}
	return itoa(s)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [8]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}

// ---------------- carapace ----------------

func harvestCarapace(ctx context.Context, bins map[string]string) []*core.ToolSpec {
	if !has("carapace") {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "carapace", "--list").Output()
	if err != nil {
		return nil
	}
	var specs []*core.ToolSpec
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		name := strings.TrimSpace(strings.Fields(sc.Text())[0])
		if name == "" {
			continue
		}
		if _, ok := bins[name]; !ok {
			continue
		}
		// carapace is queried lazily at completion time for values; here we
		// only record that a high-quality spec source exists for this tool.
		specs = append(specs, &core.ToolSpec{
			Name: name, Source: "carapace", Confidence: 0.95,
			Flags: []core.Flag{{Long: "--help", Arg: core.ArgNone, Desc: "show help"}},
		})
	}
	return specs
}

// ---------------- fish ----------------

var fishComplete = regexp.MustCompile(`^\s*complete\b(.*)$`)

func harvestFishDir(dir string) []*core.ToolSpec {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var specs []*core.ToolSpec
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".fish") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".fish")
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if s := parseFish(name, string(b)); s != nil {
			specs = append(specs, s)
		}
	}
	return specs
}

func parseFish(name, body string) *core.ToolSpec {
	spec := &core.ToolSpec{Name: name, Source: "fish", Confidence: 0.85}
	seen := map[string]bool{}
	subs := map[string]*core.Subcommand{}

	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		m := fishComplete.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		args := splitArgs(m[1])
		var long, short, desc, cond string
		var takesArg bool
		var isSub bool
		var subName string

		for i := 0; i < len(args); i++ {
			switch args[i] {
			case "-l", "--long-option":
				if i+1 < len(args) {
					long = "--" + unquote(args[i+1])
					i++
				}
			case "-s", "--short-option":
				if i+1 < len(args) {
					short = "-" + unquote(args[i+1])
					i++
				}
			case "-o", "--old-option":
				if i+1 < len(args) {
					long = "-" + unquote(args[i+1])
					i++
				}
			case "-d", "--description":
				if i+1 < len(args) {
					desc = unquote(args[i+1])
					i++
				}
			case "-n", "--condition":
				if i+1 < len(args) {
					cond = unquote(args[i+1])
					i++
				}
			case "-r", "--require-parameter":
				takesArg = true
			case "-a", "--arguments":
				if i+1 < len(args) {
					if cond == "" && long == "" && short == "" {
						isSub = true
						subName = unquote(args[i+1])
					}
					i++
				}
			}
		}
		if isSub {
			for _, s := range strings.Fields(subName) {
				if s == "" || strings.ContainsAny(s, "()$\"'") {
					continue
				}
				if subs[s] == nil {
					subs[s] = &core.Subcommand{Name: s, Desc: desc}
				}
			}
			continue
		}
		if long == "" && short == "" {
			continue
		}
		key := long + "|" + short
		if seen[key] {
			continue
		}
		seen[key] = true
		arg := core.ArgNone
		if takesArg {
			arg = core.ArgRequired
		}
		f := core.Flag{Long: long, Short: short, Arg: arg, Desc: desc}

		// A condition like `__fish_seen_subcommand_from add` scopes the flag.
		if sub := fishSubcommandFromCond(cond); sub != "" {
			if subs[sub] == nil {
				subs[sub] = &core.Subcommand{Name: sub}
			}
			subs[sub].Flags = append(subs[sub].Flags, f)
		} else {
			spec.Flags = append(spec.Flags, f)
		}
	}
	for _, s := range subs {
		spec.Subcommands = append(spec.Subcommands, *s)
	}
	if len(spec.Flags)+len(spec.Subcommands) == 0 {
		return nil
	}
	return spec
}

var condSub = regexp.MustCompile(`__fish_seen_subcommand_from\s+([A-Za-z0-9_-]+)`)

func fishSubcommandFromCond(cond string) string {
	if m := condSub.FindStringSubmatch(cond); m != nil {
		return m[1]
	}
	return ""
}

// ---------------- zsh ----------------

var zshSpec = regexp.MustCompile(`'(?:\([^)]*\))?(--?[A-Za-z0-9][A-Za-z0-9_-]*)(\+|=)?(\[[^\]]*\])?`)

func harvestZshDir(dir string) []*core.ToolSpec {
	var specs []*core.ToolSpec
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		base := filepath.Base(p)
		if !strings.HasPrefix(base, "_") || strings.Contains(base, ".") {
			return nil
		}
		name := strings.TrimPrefix(base, "_")
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		if s := parseZsh(name, string(b)); s != nil {
			specs = append(specs, s)
		}
		return nil
	})
	return specs
}

func parseZsh(name, body string) *core.ToolSpec {
	spec := &core.ToolSpec{Name: name, Source: "zsh", Confidence: 0.70}
	seen := map[string]bool{}
	for _, m := range zshSpec.FindAllStringSubmatch(body, -1) {
		flag, argMark, desc := m[1], m[2], m[3]
		if seen[flag] {
			continue
		}
		seen[flag] = true
		f := core.Flag{Arg: core.ArgNone}
		if strings.HasPrefix(flag, "--") {
			f.Long = flag
		} else {
			f.Short = flag
		}
		if argMark != "" {
			f.Arg = core.ArgRequired
		}
		if len(desc) > 2 {
			f.Desc = strings.TrimSpace(desc[1 : len(desc)-1])
		}
		spec.Flags = append(spec.Flags, f)
	}
	if len(spec.Flags) < 2 {
		return nil
	}
	return spec
}

// ---------------- man (roff source) ----------------

func findManPage(name string, dirs []string) string {
	for _, root := range dirs {
		for _, sec := range []string{"man1", "man8", "man6"} {
			for _, ext := range []string{"", ".gz", ".zst", ".bz2"} {
				p := filepath.Join(root, sec, name+"."+sec[3:]+ext)
				if _, err := os.Stat(p); err == nil {
					return p
				}
			}
		}
	}
	return ""
}

var (
	roffFlag = regexp.MustCompile(`\\fB(--?[A-Za-z0-9][A-Za-z0-9_-]*)\\fR`)
	roffBI   = regexp.MustCompile(`^\.(?:B|BI|BR)\s+(--?[A-Za-z0-9][A-Za-z0-9_-]*)`)
)

func parseManRoff(name, path string) *core.ToolSpec {
	body, err := readMaybeCompressed(path)
	if err != nil {
		return nil
	}
	spec := &core.ToolSpec{Name: name, Source: "man", Confidence: 0.65}
	seen := map[string]bool{}
	lines := strings.Split(body, "\n")

	addFlag := func(flag, desc string) {
		if seen[flag] || len(flag) < 2 {
			return
		}
		seen[flag] = true
		f := core.Flag{Arg: core.ArgNone, Desc: clip(desc, 90)}
		if strings.HasPrefix(flag, "--") {
			f.Long = flag
		} else {
			f.Short = flag
		}
		spec.Flags = append(spec.Flags, f)
	}

	for i, ln := range lines {
		// .TP introduces a tagged paragraph: next line is the tag (the flag),
		// the lines after it are the description. This is the structure that
		// rendering to 80 columns throws away.
		if strings.HasPrefix(ln, ".TP") && i+1 < len(lines) {
			tag := lines[i+1]
			desc := ""
			for j := i + 2; j < len(lines) && j < i+6; j++ {
				if strings.HasPrefix(lines[j], ".") {
					break
				}
				desc += " " + lines[j]
			}
			desc = deroff(desc)
			if m := roffBI.FindStringSubmatch(tag); m != nil {
				addFlag(m[1], desc)
			}
			for _, m := range roffFlag.FindAllStringSubmatch(tag, -1) {
				addFlag(m[1], desc)
			}
		}
	}
	if len(spec.Flags) < 2 {
		return nil
	}
	return spec
}

func readMaybeCompressed(path string) (string, error) {
	switch filepath.Ext(path) {
	case ".gz", ".zst", ".bz2":
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "zcat", "-f", path).Output()
		if err != nil {
			return "", err
		}
		return string(out), nil
	}
	b, err := os.ReadFile(path)
	return string(b), err
}

var roffEsc = regexp.MustCompile(`\\f[BIRP]|\\&|\\-`)

func deroff(s string) string {
	s = roffEsc.ReplaceAllString(s, "")
	return strings.Join(strings.Fields(s), " ")
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// ---------------- --help text ----------------

var helpFlagRe = regexp.MustCompile(
	`^\s{1,10}(?:(-[A-Za-z0-9]),?\s*)?(--[A-Za-z0-9][A-Za-z0-9_-]*)(?:[= ]([<\[]?[A-Z][A-Za-z_]*[>\]]?))?\s{2,}(.*)$`)
var helpShortOnly = regexp.MustCompile(
	`^\s{1,10}(-[A-Za-z0-9])(?:\s+([A-Z][A-Za-z_]*))?\s{2,}(.*)$`)

func parseHelp(name, out string) *core.ToolSpec {
	spec := &core.ToolSpec{Name: name, Source: "help", Confidence: 0.55}
	seen := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		ln := sc.Text()
		if m := helpFlagRe.FindStringSubmatch(ln); m != nil {
			if seen[m[2]] {
				continue
			}
			seen[m[2]] = true
			arg := core.ArgNone
			if m[3] != "" {
				arg = core.ArgRequired
			}
			spec.Flags = append(spec.Flags, core.Flag{
				Long: m[2], Short: m[1], Arg: arg, Desc: clip(strings.TrimSpace(m[4]), 90),
			})
			continue
		}
		if m := helpShortOnly.FindStringSubmatch(ln); m != nil {
			if seen[m[1]] {
				continue
			}
			seen[m[1]] = true
			arg := core.ArgNone
			if m[2] != "" {
				arg = core.ArgRequired
			}
			spec.Flags = append(spec.Flags, core.Flag{
				Short: m[1], Arg: arg, Desc: clip(strings.TrimSpace(m[3]), 90),
			})
		}
	}
	if len(spec.Flags) < 2 {
		return nil
	}
	return spec
}

func splitArgs(s string) []string {
	var out []string
	var cur strings.Builder
	var q byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case q != 0:
			if c == q {
				q = 0
				out = append(out, cur.String())
				cur.Reset()
			} else {
				cur.WriteByte(c)
			}
		case c == '\'' || c == '"':
			q = c
		case c == ' ' || c == '\t':
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func unquote(s string) string {
	return strings.Trim(s, `'"`)
}
