package harvest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseSpecJSON(t *testing.T) {
	body := `Here is the spec you asked for:
` + "```json" + `
{"flags":[
  {"long":"--verbose","short":"-v","arg":"none","desc":"be loud"},
  {"long":"--output","arg":"required","desc":"write here"},
  {"long":"not-a-flag","arg":"none","desc":"hallucinated"},
  {"long":"--verbose","short":"-v","arg":"none","desc":"dupe"}
],
"subcommands":[
  {"name":"apply","desc":"apply config","flags":[{"long":"--force","arg":"none","desc":"do it"}]},
  {"name":"bad name","desc":"has space"},
  {"name":"delete","desc":"remove things"}
]}
` + "```" + `
Hope this helps!`
	spec, err := parseSpecJSON("kubectl", body)
	if err != nil {
		t.Fatalf("parseSpecJSON: %v", err)
	}
	if spec.Name != "kubectl" || spec.Source != "llm" || spec.Confidence != 0.60 {
		t.Fatalf("spec header wrong: %+v", spec)
	}
	if len(spec.Flags) != 2 {
		t.Fatalf("want 2 clean flags, got %d: %+v", len(spec.Flags), spec.Flags)
	}
	if spec.Flags[0].Long != "--verbose" || spec.Flags[0].Short != "-v" || spec.Flags[0].Arg != "none" {
		t.Fatalf("flag 0 wrong: %+v", spec.Flags[0])
	}
	if spec.Flags[1].Arg != "required" {
		t.Fatalf("arg mapping wrong: %+v", spec.Flags[1])
	}
	if len(spec.Subcommands) != 2 || spec.Subcommands[0].Name != "apply" || spec.Subcommands[1].Name != "delete" {
		t.Fatalf("subcommands wrong: %+v", spec.Subcommands)
	}
	if len(spec.Subcommands[0].Flags) != 1 || spec.Subcommands[0].Flags[0].Long != "--force" {
		t.Fatalf("subcommand flags wrong: %+v", spec.Subcommands[0].Flags)
	}
}

func TestParseSpecJSONEmpty(t *testing.T) {
	if _, err := parseSpecJSON("true", `{"flags":[],"subcommands":[]}`); err == nil {
		t.Fatal("empty doc must be an error so harvest records no spec")
	}
	if _, err := parseSpecJSON("true", "I cannot parse this."); err == nil {
		t.Fatal("non-JSON response must error")
	}
}

// The privacy contract, tested explicitly: the request body may contain the
// tool name and the documentation text — and nothing else. There is no API
// for anything else, so this guards against future regressions that add
// "helpful context" to the prompt.
func TestSynthesizeSpecPayloadHygiene(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"role": "assistant", "content": `{"flags":[{"long":"--fast","arg":"none","desc":"go fast"}],"subcommands":[{"name":"run","desc":"run it"}]}`}},
			},
		})
	}))
	defer srv.Close()

	t.Setenv("HOME", "/home/steffen")
	opt := LLMOptions{Endpoint: srv.URL, Model: "m", Timeout: 5 * time.Second, MaxTextChars: 4000}
	spec, err := SynthesizeSpec(context.Background(), opt, "frob", "Usage: frob [--fast] — frobnicate /home/steffen/secret/dir")
	if err != nil {
		t.Fatalf("SynthesizeSpec: %v", err)
	}
	if spec == nil || len(spec.Flags) != 1 || spec.Flags[0].Long != "--fast" {
		t.Fatalf("bad spec: %+v", spec)
	}
	if len(spec.Subcommands) != 1 || spec.Subcommands[0].Name != "run" {
		t.Fatalf("subcommand missing: %+v", spec)
	}
	body := string(gotBody)
	if !strings.Contains(body, "frob") {
		t.Fatal("tool name missing from payload")
	}
	if strings.Contains(body, "/home/steffen") {
		t.Fatal("home path leaked into payload; scrubbing regressed")
	}
	if !strings.Contains(body, "$HOME") {
		t.Fatal("home path should be $HOME-scrubbed in payload")
	}
}

func TestSynthesizeSpecEndpointDown(t *testing.T) {
	opt := LLMOptions{Endpoint: "http://127.0.0.1:1", Model: "m", Timeout: 500 * time.Millisecond, MaxTextChars: 1000}
	if _, err := SynthesizeSpec(context.Background(), opt, "x", "text"); err == nil {
		t.Fatal("want error for dead endpoint")
	}
}

func TestLLMIsLocal(t *testing.T) {
	for ep, want := range map[string]bool{
		"http://127.0.0.1:8080/v1": true,
		"http://localhost:11434":   true,
		"http://[::1]:8080":        true,
		"http://10.66.0.3:8080/v1": false,
		"https://api.openai.com":   false,
	} {
		if got := (LLMOptions{Endpoint: ep}).IsLocal(); got != want {
			t.Errorf("IsLocal(%q) = %v, want %v", ep, got, want)
		}
	}
}

func TestLLMFromEnv(t *testing.T) {
	t.Setenv("ARIADNE_LLM_ENDPOINT", "http://127.0.0.1:8080/v1/")
	t.Setenv("ARIADNE_LLM_MODEL", "qwen")
	t.Setenv("ARIADNE_LLM_MAXTOOLS", "12")
	o := LLMFromEnv()
	if !o.Enabled() || o.Endpoint != "http://127.0.0.1:8080/v1" || o.Model != "qwen" || o.MaxTools != 12 {
		t.Fatalf("env mapping wrong: %+v", o)
	}
	if o.Timeout <= 0 || o.MaxTextChars <= 0 {
		t.Fatalf("defaults missing: %+v", o)
	}
}

func TestLLMPreflightPicksModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "auto-model"}}})
	}))
	defer srv.Close()
	opt := LLMOptions{Endpoint: srv.URL}
	if err := llmPreflight(context.Background(), &opt); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if opt.Model != "auto-model" {
		t.Fatalf("model not auto-picked: %q", opt.Model)
	}
}
