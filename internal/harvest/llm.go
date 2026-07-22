// LLM-backed spec synthesis.
//
// The deterministic sources (carapace/fish/zsh/man-roff/--help-regex) leave a
// tail of tools with no spec at all, and the text parsers can only extract
// flags — never subcommands. A small local model is a strictly better parser
// of --help and man text: it produces flags AND subcommands, i.e. the
// "parameter chain" that makes a brand-new tool usable from the first query.
//
// Privacy is enforced by construction, not by policy: SynthesizeSpec accepts
// exactly (tool name, public documentation text). There is no parameter
// through which history, cwd, or any user data could reach the payload. The
// text is additionally home-path scrubbed before it leaves the process, and a
// non-loopback endpoint is announced loudly via the harvest log.
package harvest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"ariadne/internal/core"
)

type LLMOptions struct {
	Endpoint     string // OpenAI-compatible base URL, e.g. http://127.0.0.1:8080/v1
	Model        string // empty = first model the server lists
	APIKey       string
	Timeout      time.Duration
	MaxTools     int // synthesis attempts per harvest run
	MaxTextChars int // documentation text truncation per tool
	// NoThink asks reasoning models (Qwen3-style chat templates) to skip
	// their thinking pass; a spec-extraction prompt that reasons for 2k
	// tokens and then runs out of budget returns empty content.
	NoThink bool
}

func LLMFromEnv() LLMOptions {
	o := LLMOptions{
		Endpoint:     strings.TrimRight(os.Getenv("ARIADNE_LLM_ENDPOINT"), "/"),
		Model:        os.Getenv("ARIADNE_LLM_MODEL"),
		APIKey:       os.Getenv("ARIADNE_LLM_KEY"),
		Timeout:      120 * time.Second,
		MaxTools:     20,
		MaxTextChars: 12000,
		NoThink:      true,
	}
	if os.Getenv("ARIADNE_LLM_NOTHINK") == "0" {
		o.NoThink = false
	}
	if v := os.Getenv("ARIADNE_LLM_MAXTOOLS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			o.MaxTools = n
		}
	}
	return o
}

func (o LLMOptions) Enabled() bool { return o.Endpoint != "" }

// IsLocal reports whether the endpoint is loopback. Remote endpoints are
// allowed (payloads are public documentation by construction) but the fact
// is surfaced, because a user who typo'd a public URL should notice.
func (o LLMOptions) IsLocal() bool {
	u, err := url.Parse(o.Endpoint)
	if err != nil {
		return false
	}
	h := u.Hostname()
	if h == "localhost" || h == "" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

var errLLMUnavailable = errors.New("llm endpoint unavailable")

var llmHTTP = &http.Client{Timeout: 180 * time.Second}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
	// llama.cpp / vLLM / llama-swap accept chat_template_kwargs and ignore
	// unknown keys inside it; servers that reject unknown top-level fields
	// can be accommodated via ARIADNE_LLM_NOTHINK=0.
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			chatMessage
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// llmPreflight verifies the endpoint answers and resolves the model name
// once per harvest run, so a dead endpoint costs one 4s probe instead of
// MaxTools per-call timeouts.
func llmPreflight(ctx context.Context, opt *LLMOptions) error {
	if opt.Model != "" {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, "GET", opt.Endpoint+"/models", nil)
	if err != nil {
		return err
	}
	if opt.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+opt.APIKey)
	}
	resp, err := llmHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", errLLMUnavailable, err)
	}
	defer resp.Body.Close()
	var models struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&models); err != nil || len(models.Data) == 0 {
		return fmt.Errorf("%w: /models returned no usable model list", errLLMUnavailable)
	}
	opt.Model = models.Data[0].ID
	return nil
}

const synthSystem = `You convert CLI documentation into a JSON completion spec for a shell completion engine. Output ONLY one JSON object — no markdown, no commentary.

Schema:
{"flags":[{"long":"--verbose","short":"-v","arg":"none","desc":"..."}],
 "subcommands":[{"name":"commit","desc":"...","flags":[{"long":"--amend","arg":"none","desc":"..."}]}]}

Rules:
- use only flags and subcommands that appear verbatim in the text; never invent any
- "arg" is "none" for booleans, "required" when the flag takes a value, "optional" when the value is optional
- omit "short" when the flag has none
- desc: terse English, max 60 chars
- at most 40 flags and 20 subcommands; when truncating, keep the most useful ones
- if the text contains no usable options or subcommands, output {"flags":[],"subcommands":[]}`

// SynthesizeSpec turns one tool's public documentation (man roff or --help
// text) into a ToolSpec. Only the tool name and that text ever leave the
// process — history and context have no path into this function.
func SynthesizeSpec(ctx context.Context, opt LLMOptions, name, text string) (*core.ToolSpec, error) {
	if home, err := os.UserHomeDir(); err == nil && home != "" && home != "/" {
		text = strings.ReplaceAll(text, home, "$HOME")
	}
	if len(text) > opt.MaxTextChars {
		text = text[:opt.MaxTextChars]
	}
	body := chatRequest{
		Model: opt.Model,
		Messages: []chatMessage{
			{Role: "system", Content: synthSystem},
			{Role: "user", Content: "Tool: " + name + "\n\nDocumentation text:\n\"\"\"\n" + text + "\n\"\"\""},
		},
		Temperature: 0,
		MaxTokens:   4000,
	}
	if opt.NoThink {
		body.ChatTemplateKwargs = map[string]any{"enable_thinking": false}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	cctx, cancel := context.WithTimeout(ctx, opt.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, "POST", opt.Endpoint+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if opt.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+opt.APIKey)
	}
	resp, err := llmHTTP.Do(req)
	if err != nil {
		// A per-call deadline (cold model load, big man page) is this tool's
		// problem, not the endpoint's — the harvest loop continues on it.
		if errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err) {
			return nil, fmt.Errorf("llm: call timed out: %w", err)
		}
		return nil, fmt.Errorf("%w: %v", errLLMUnavailable, err)
	}
	defer resp.Body.Close()

	var cr chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return nil, fmt.Errorf("llm: undecodable response: %w", err)
	}
	if cr.Error != nil {
		return nil, fmt.Errorf("llm: %s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 {
		return nil, fmt.Errorf("llm: empty choices")
	}
	content := cr.Choices[0].Message.Content
	if strings.TrimSpace(content) == "" && cr.Choices[0].Message.ReasoningContent != "" {
		// The server ignored enable_thinking and spent the whole token budget
		// on reasoning; the draft JSON often survives inside it.
		content = cr.Choices[0].Message.ReasoningContent
	}
	return parseSpecJSON(name, content)
}

// ---------------- response parsing ----------------

var (
	llmFlagName = regexp.MustCompile(`^--?[A-Za-z0-9][A-Za-z0-9_-]*$`)
	llmSubName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]*$`)
)

type llmSpecDoc struct {
	Flags []struct {
		Long  string `json:"long"`
		Short string `json:"short"`
		Arg   string `json:"arg"`
		Desc  string `json:"desc"`
	} `json:"flags"`
	Subcommands []struct {
		Name  string `json:"name"`
		Desc  string `json:"desc"`
		Flags []struct {
			Long  string `json:"long"`
			Short string `json:"short"`
			Arg   string `json:"arg"`
			Desc  string `json:"desc"`
		} `json:"flags"`
	} `json:"subcommands"`
}

// parseSpecJSON tolerates markdown fences and prose around the object, but
// every flag/subcommand name must match the strict shape — anything else is
// a hallucination and is dropped, not trusted.
func parseSpecJSON(name, content string) (*core.ToolSpec, error) {
	content = strings.TrimSpace(content)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	start := strings.IndexByte(content, '{')
	end := strings.LastIndexByte(content, '}')
	if start < 0 || end <= start {
		return nil, fmt.Errorf("llm: no JSON object in response")
	}
	var doc llmSpecDoc
	if err := json.Unmarshal([]byte(content[start:end+1]), &doc); err != nil {
		return nil, fmt.Errorf("llm: bad JSON: %w", err)
	}

	argKind := func(s string) core.ArgKind {
		switch s {
		case "required":
			return core.ArgRequired
		case "optional":
			return core.ArgOptional
		default:
			return core.ArgNone
		}
	}

	spec := &core.ToolSpec{Name: name, Source: "llm", Confidence: 0.60}
	addFlag := func(dst []core.Flag, seen map[string]bool, long, short, arg, desc string) []core.Flag {
		if long == "" && short == "" {
			return dst
		}
		if long != "" && !llmFlagName.MatchString(long) {
			return dst
		}
		if short != "" && !llmFlagName.MatchString(short) {
			return dst
		}
		key := long + "|" + short
		if seen[key] {
			return dst
		}
		seen[key] = true
		return append(dst, core.Flag{Long: long, Short: short, Arg: argKind(arg), Desc: clip(desc, 90)})
	}

	seenTop := map[string]bool{}
	for _, f := range doc.Flags {
		if len(spec.Flags) >= 40 {
			break
		}
		spec.Flags = addFlag(spec.Flags, seenTop, f.Long, f.Short, f.Arg, f.Desc)
	}
	for _, sc := range doc.Subcommands {
		if len(spec.Subcommands) >= 20 {
			break
		}
		if !llmSubName.MatchString(sc.Name) {
			continue
		}
		sub := core.Subcommand{Name: sc.Name, Desc: clip(sc.Desc, 90)}
		seenSub := map[string]bool{}
		for _, f := range sc.Flags {
			if len(sub.Flags) >= 40 {
				break
			}
			sub.Flags = addFlag(sub.Flags, seenSub, f.Long, f.Short, f.Arg, f.Desc)
		}
		spec.Subcommands = append(spec.Subcommands, sub)
	}
	if len(spec.Flags)+len(spec.Subcommands) == 0 {
		return nil, fmt.Errorf("llm: no usable options in response")
	}
	return spec, nil
}
