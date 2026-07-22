// Package core holds the shared domain types plus the two operations that
// must be identical everywhere: normalization and redaction.
package core

import (
	"hash/fnv"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Hash is the identity of a normalized command line.
type Hash uint64

func HashOf(s string) Hash {
	h := fnv.New64a()
	h.Write([]byte(s))
	return Hash(h.Sum64())
}

// Event is one command execution as observed by the shell.
type Event struct {
	Raw        string `json:"raw"`
	Norm       string `json:"norm"`
	Argv0      string `json:"argv0"`
	Cwd        string `json:"cwd,omitempty"`
	GitRoot    string `json:"git_root,omitempty"`
	GitBranch  string `json:"git_branch,omitempty"`
	Host       string `json:"host,omitempty"`
	Session    string `json:"session,omitempty"`
	ExitCode   int    `json:"exit"`
	DurationMS int64  `json:"dur_ms,omitempty"`
	TS         int64  `json:"ts"`
	PrevHash   Hash   `json:"prev,omitempty"`
}

func (e *Event) Hash() Hash { return HashOf(e.Norm) }

// CtxKind enumerates the context dimensions statistics are bucketed by.
type CtxKind uint8

const (
	CtxGlobal CtxKind = iota
	CtxCwd
	CtxGitRoot
	CtxHost
)

// Stat is a decayed frecency counter for (command, context) pairs.
type Stat struct {
	NSuccess int32   `json:"s"`
	NFailure int32   `json:"f"`
	LastTS   int64   `json:"t"`
	Decayed  float64 `json:"d"`
	// HourHist is a 4-bucket time-of-day histogram (00-06,06-12,12-18,18-24).
	HourHist [4]int32 `json:"h"`
}

// Entry is one distinct command line plus everything known about it.
type Entry struct {
	Hash     Hash             `json:"h"`
	Norm     string           `json:"n"`
	Raw      string           `json:"r"`
	Argv0    string           `json:"a"`
	Global   Stat             `json:"g"`
	ByCwd    map[string]*Stat `json:"c,omitempty"`
	ByRepo   map[string]*Stat `json:"p,omitempty"`
	ByHost   map[string]*Stat `json:"o,omitempty"`
	Redacted bool             `json:"x,omitempty"`
}

// ---------------- Tool specifications ----------------

type ArgKind string

const (
	ArgNone     ArgKind = "none"
	ArgRequired ArgKind = "required"
	ArgOptional ArgKind = "optional"
)

type Flag struct {
	Long    string   `json:"long,omitempty"`
	Short   string   `json:"short,omitempty"`
	Arg     ArgKind  `json:"arg"`
	ArgType string   `json:"arg_type,omitempty"`
	Enum    []string `json:"enum,omitempty"`
	Desc    string   `json:"desc,omitempty"`
	NUsed   int32    `json:"n_used,omitempty"`
}

func (f Flag) Display() string {
	if f.Long != "" {
		return f.Long
	}
	return f.Short
}

type Subcommand struct {
	Name  string `json:"name"`
	Desc  string `json:"desc,omitempty"`
	Flags []Flag `json:"flags,omitempty"`
}

type ToolSpec struct {
	Name        string       `json:"name"`
	AbsPath     string       `json:"abs_path,omitempty"`
	Version     string       `json:"version,omitempty"`
	Pkg         string       `json:"pkg,omitempty"`
	Source      string       `json:"source"` // carapace|fish|zsh|man|help|llm
	Confidence  float64      `json:"confidence"`
	Flags       []Flag       `json:"flags,omitempty"`
	Subcommands []Subcommand `json:"subcommands,omitempty"`
	Templates   []string     `json:"templates,omitempty"` // canonical invocations
	BinMTime    int64        `json:"bin_mtime,omitempty"`
	UpdatedAt   int64        `json:"updated_at"`
}

// FlagsFor returns the flag set for a resolved subcommand path (may be empty).
func (t *ToolSpec) FlagsFor(sub string) []Flag {
	if sub != "" {
		for i := range t.Subcommands {
			if t.Subcommands[i].Name == sub {
				return append(append([]Flag{}, t.Subcommands[i].Flags...), t.Flags...)
			}
		}
	}
	return t.Flags
}

// ---------------- Normalization ----------------

var (
	wsRe   = regexp.MustCompile(`\s+`)
	homeRe *regexp.Regexp
)

func init() {
	if h, err := os.UserHomeDir(); err == nil && h != "" && h != "/" {
		homeRe = regexp.MustCompile(`(^|\s)` + regexp.QuoteMeta(h) + `(/|\s|$)`)
	}
}

// Normalize produces the canonical form used for statistics and indexing.
// It must be deterministic and cheap: it runs on every keystroke.
func Normalize(s string) string {
	s = strings.TrimSpace(s)
	s = wsRe.ReplaceAllString(s, " ")
	s = strings.TrimRight(s, ";& ")
	if homeRe != nil {
		s = homeRe.ReplaceAllString(s, "${1}~${2}")
	}
	return s
}

// Argv0 extracts the effective command name, seeing through env-var prefixes
// (FOO=bar cmd), sudo, and command paths.
func Argv0(norm string) string {
	fields := strings.Fields(norm)
	for _, f := range fields {
		if strings.Contains(f, "=") && !strings.HasPrefix(f, "-") &&
			strings.IndexByte(f, '=') < strings.IndexByte(f+" ", ' ') {
			continue // env assignment prefix
		}
		if f == "sudo" || f == "doas" || f == "command" || f == "time" {
			continue
		}
		return filepath.Base(f)
	}
	return ""
}

// ---------------- Tokenization ----------------

type Token struct {
	Text  string
	Start int
	End   int
}

// Tokenize splits a buffer into whitespace-separated tokens with offsets.
// Quote-aware enough for completion purposes; not a shell parser.
func Tokenize(buf string) []Token {
	var out []Token
	i, n := 0, len(buf)
	for i < n {
		for i < n && buf[i] == ' ' {
			i++
		}
		if i >= n {
			break
		}
		start := i
		var quote byte
		for i < n {
			c := buf[i]
			if quote != 0 {
				if c == quote {
					quote = 0
				}
			} else if c == '\'' || c == '"' {
				quote = c
			} else if c == ' ' {
				break
			}
			i++
		}
		out = append(out, Token{Text: buf[start:i], Start: start, End: i})
	}
	return out
}

// TokenAt returns the index of the token the cursor sits in or immediately
// after, and whether the cursor is at a fresh (empty) token position.
func TokenAt(toks []Token, cursor int) (idx int, fresh bool) {
	for i, t := range toks {
		if cursor >= t.Start && cursor <= t.End {
			return i, false
		}
	}
	return len(toks), true
}

// ---------------- Redaction ----------------

// prefixRedactors preserve the captured prefix (flag name, auth header) and
// scrub only the value after it.
var prefixRedactors = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(--?(?:password|passwd|pass|token|secret|api[-_]?key|auth)[= ])\S+`),
	regexp.MustCompile(`(?i)(authorization:\s*bearer\s+)\S+`),
	regexp.MustCompile(`(?i)((?:AWS|GITHUB|GITLAB|OPENAI|ANTHROPIC|HF)_[A-Z_]*(?:TOKEN|KEY|SECRET)=)\S+`),
}

// fullRedactors match the secret itself; there is no prefix worth keeping.
// (Previously these captured the whole secret and replaced it with
// ${1}<redacted> — i.e. they kept the secret verbatim and only marked it.)
var fullRedactors = []*regexp.Regexp{
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{16,}\b`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{20,}\b`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\b`),
}

// entropyRedactors catch unstructured high-entropy blobs; the path check runs
// per match, so they live outside the two slices above.
var entropyRedactors = []*regexp.Regexp{
	regexp.MustCompile(`\b([A-Za-z0-9+/]{40,}={0,2})\b`), // high-entropy blob
	regexp.MustCompile(`\b([0-9a-fA-F]{40,})\b`),
}

const RedactMark = "<redacted>"

// dashPToken matches mysql-style attached passwords (-pSecret). The old rule
// was `(-p)[^\s-]\S*`, which fired on every hyphenated word containing
// "-p<letter>" — "neuromark-core-pipeline", "single-page" — and on the
// most common docker/ssh invocations (-p8080, -p 8080:80). Those commands
// were scrubbed and, being scrubbed, never suggested again. The flag must
// now start a token, and the value must contain a letter to count as a
// password; -p8080 is a port, not a credential.
var dashPToken = regexp.MustCompile(`(^|\s)(-p)([A-Za-z0-9_=-]{2,})`)

const dashPLetters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

func redactDashP(s string) string {
	return dashPToken.ReplaceAllStringFunc(s, func(m string) string {
		sub := dashPToken.FindStringSubmatch(m)
		if !strings.ContainsAny(sub[3], dashPLetters) {
			return m
		}
		return sub[1] + sub[2] + RedactMark
	})
}

// Redact scrubs credentials. Returns the scrubbed string and whether anything
// was removed. Redacted commands are stored but never offered verbatim.
func Redact(s string) (string, bool) {
	out := s
	for _, re := range prefixRedactors {
		out = re.ReplaceAllString(out, "${1}"+RedactMark)
	}
	for _, re := range fullRedactors {
		out = re.ReplaceAllString(out, RedactMark)
	}
	for _, re := range entropyRedactors {
		// Entropy heuristics: skip if the whole token looks like a path.
		out = re.ReplaceAllStringFunc(out, func(m string) string {
			if strings.ContainsAny(m, "/.") {
				return m
			}
			return RedactMark
		})
	}
	out = redactDashP(out)
	return out, out != s
}

// ShouldIgnore implements the leading-space convention and the denylist.
func ShouldIgnore(raw string, deny []*regexp.Regexp) bool {
	if strings.HasPrefix(raw, " ") || strings.HasPrefix(raw, "\t") {
		return true
	}
	for _, re := range deny {
		if re.MatchString(raw) {
			return true
		}
	}
	return false
}
