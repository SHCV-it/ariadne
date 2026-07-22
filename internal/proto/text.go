package proto

// Text codec.
//
// The zsh widget cannot spawn a process per keystroke — fork+exec is 2–5ms,
// which eats half the budget before any work happens. So it talks to the
// daemon directly through zsh/net/socket. That module gives you `print -u fd`
// and `read -u fd`: line-oriented text, no binary framing, no JSON parser.
//
// Hence a second codec. Requests are one line of tab-separated key=value
// pairs; responses are tab-separated records terminated by a lone ".".
// Both sides escape \, \t and \n. A zsh `read -A` parses this in one builtin
// call.

import (
	"bufio"
	"strconv"
	"strings"

	"ariadne/internal/core"
)

func escape(s string) string {
	r := strings.NewReplacer("\\", "\\\\", "\t", "\\t", "\n", "\\n", "\r", "")
	return r.Replace(s)
}

func unescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 't':
			b.WriteByte('\t')
		case 'n':
			b.WriteByte('\n')
		case '\\':
			b.WriteByte('\\')
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// ParseTextRequest decodes one request line.
func ParseTextRequest(line string) *Request {
	fields := strings.Split(strings.TrimRight(line, "\r\n"), "\t")
	if len(fields) == 0 {
		return nil
	}
	req := &Request{}
	switch strings.ToUpper(fields[0]) {
	case "QUERY":
		req.Op = OpQuery
	case "INGEST":
		req.Op = OpIngest
	case "ACCEPT":
		req.Op = OpAccept
	case "REJECT":
		req.Op = OpReject
	case "PING":
		req.Op = OpPing
	case "STATS":
		req.Op = OpStats
	default:
		return nil
	}
	var ev core.Event
	haveEv := false

	for _, f := range fields[1:] {
		eq := strings.IndexByte(f, '=')
		if eq < 0 {
			continue
		}
		k, v := f[:eq], unescape(f[eq+1:])
		switch k {
		case "buf":
			req.Buffer = v
		case "cur":
			req.Cursor, _ = strconv.Atoi(v)
		case "cwd":
			req.Cwd = v
			ev.Cwd = v
		case "repo":
			req.GitRoot = v
			ev.GitRoot = v
		case "branch":
			req.GitBranch = v
			ev.GitBranch = v
		case "host":
			req.Host = v
			ev.Host = v
		case "sess":
			req.Session = v
			ev.Session = v
		case "n":
			req.Limit, _ = strconv.Atoi(v)
		case "chosen":
			req.Chosen, _ = strconv.Atoi(v)
		case "raw":
			ev.Raw = v
			haveEv = true
		case "exit":
			ev.ExitCode, _ = strconv.Atoi(v)
		case "dur":
			ev.DurationMS, _ = strconv.ParseInt(v, 10, 64)
		case "ts":
			ev.TS, _ = strconv.ParseInt(v, 10, 64)
		}
	}
	if req.Op == OpIngest && haveEv {
		req.Event = &ev
	}
	return req
}

// WriteTextResponse emits the response in the shell-parseable form.
func WriteTextResponse(w *bufio.Writer, r *Response) error {
	if r.Err != "" {
		w.WriteString("ERR\t" + escape(r.Err) + "\n")
		w.WriteString(".\n")
		return w.Flush()
	}
	if r.OwnsToken {
		w.WriteString("OWNS\t1\n")
	} else {
		w.WriteString("OWNS\t0\n")
	}
	if r.Ghost != "" {
		w.WriteString("GHOST\t" + escape(r.Ghost) + "\n")
	}
	for _, c := range r.Candidates {
		disp := c.Display
		if disp == "" {
			disp = c.Text
		}
		w.WriteString("CAND\t" + escape(c.Text) +
			"\t" + escape(disp) +
			"\t" + escape(c.Desc) +
			"\t" + strconv.Itoa(int(c.Count)) +
			"\t" + escape(c.Context) +
			"\t" + escape(c.Source) +
			"\t" + strconv.Itoa(c.Score) + "\n")
	}
	if r.Text != "" {
		w.WriteString("TEXT\t" + escape(r.Text) + "\n")
	}
	w.WriteString("US\t" + strconv.FormatInt(r.ElapsedUS, 10) + "\n")
	w.WriteString(".\n")
	return w.Flush()
}
