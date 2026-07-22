# Ariadne — Terminal Completion Brain

A learning tab-completion system for zsh and bash. It watches which commands you
actually run (with counts, context, and exit codes), harvests flag/subcommand
knowledge from installed tools, and offers ranked inline + panel suggestions —
falling through to your shell's native completion whenever that would be better.

Single static Go binary, zero runtime dependencies, no cloud, no telemetry.

---

## What it is not

It does **not** replace your shell's completion. It *layers on top* and
delegates. `git checkout <TAB>` still reaches git's real branch completer;
`cat ./s<TAB>` still reaches the file completer. Ariadne owns the first token,
flags/subcommands it has harvested, and inline history ghost text. Everything
dynamic — branches, hosts, files, kube contexts — stays with the shell, decided
by a sub-2ms ownership check on every Tab.

It is also **not** an LLM in your keystroke loop. The keystroke path is a
17-feature logistic model and a prefix scan — microseconds. An optional local
LLM (any OpenAI-compatible endpoint) runs strictly offline to synthesize tool
specs; it never touches a live query.

---

## Install

```sh
# 1. build (Go 1.22+)
go build -o ~/.local/bin/ariadned ./cmd/ariadned
go build -o ~/.local/bin/ariadne  ./cmd/ariadne

# 2. run the daemon (socket-activated user service)
install -Dm644 systemd/ariadned.service ~/.config/systemd/user/ariadned.service
install -Dm644 systemd/ariadned.socket  ~/.config/systemd/user/ariadned.socket
systemctl --user daemon-reload
systemctl --user enable --now ariadned.socket

# 3. hook the shell
echo 'source /path/to/ariadne/shell/ariadne.zsh'  >> ~/.zshrc   # zsh
echo 'source /path/to/ariadne/shell/ariadne.bash' >> ~/.bashrc  # bash
#    (bash needs a socket bridge: socat, openbsd nc, or ncat)

# 4. seed from existing history (one time)
ariadne import          # auto-detects ~/.zsh_history, bash, fish
```

Open a new shell. Type. You should see greyed-out ghost text after a couple of
characters and a suggestion panel under the prompt.

---

## Keys

| Key | Action |
|---|---|
| type | inline ghost suggestion + top-3 panel |
| `Tab` | accept/cycle Ariadne's suggestion if it owns the token, else native completion |
| `→` / `Ctrl-F` | accept the inline ghost |
| `Alt-1/2/3` | pick panel row 1/2/3 |
| `Enter` | run (records a rejection if a panel was shown and ignored) |

Same bindings in both shells. In bash, Tab is rebound only while the daemon
claims the token; otherwise it stays byte-for-byte your original binding.

---

## Commands

```
ariadne stats      # entries, tool coverage, latency, learned weights
ariadne doctor     # health check with real latency measurement
ariadne bench 5000 # latency distribution
ariadne harvest    # rescan $PATH for tool specs now
ariadne train      # force a ranker training round
ariadne forget <regex>   # permanently delete matching history (memory + disk)
ariadne query <buf>      # what would be suggested for this buffer
```

---

## Configuration

Shell-side, set before sourcing `ariadne.zsh` / `ariadne.bash`:

| Variable | Default | Meaning |
|---|---|---|
| `ARIADNE_PANEL_LINES` | `3` | panel rows; `0` = ghost text only |
| `ARIADNE_GHOST` | `1` | inline ghost text on/off |
| `ARIADNE_TIMEOUT` | `0.02` | per-query hard deadline (seconds) |
| `ARIADNE_MIN_CHARS` | `1` | min chars before querying |
| `ARIADNE_COLOR` | `1` | ANSI colour in the panel |

Daemon flags: `-socket`, `-data`, `-deny <regex,regex>`, `-v`.

LLM spec synthesis (optional), configured via the daemon's environment —
e.g. a systemd drop-in at `~/.config/systemd/user/ariadned.service.d/llm.conf`
with `[Service]` / `Environment=...` lines:

| Variable | Default | Meaning |
|---|---|---|
| `ARIADNE_LLM_ENDPOINT` | _(unset = off)_ | OpenAI-compatible base URL, e.g. `http://127.0.0.1:9001/v1` |
| `ARIADNE_LLM_MODEL` | first from `/models` | model id to chat with |
| `ARIADNE_LLM_KEY` | empty | bearer token, if the server wants one |
| `ARIADNE_LLM_MAXTOOLS` | `20` | synthesis attempts per harvest run |
| `ARIADNE_LLM_NOTHINK` | `1` | `0` stops sending `enable_thinking=false` (for reasoning models) |

During harvest, every tool the deterministic sources (carapace/fish/zsh/man/
--help) left without a spec gets its man page or `--help` text synthesized
into flags **and subcommands** by the LLM — the only source that recovers the
full parameter chain. Results land in the spec database with source `llm`
(see `ariadne stats` → `tools_by_source`) and complete exactly like any other
spec. Reasoning models are handled (`chat_template_kwargs.enable_thinking=false`,
with a `reasoning_content` fallback).

---

## How it learns

1. **History.** Every command is normalized (whitespace, `$HOME`→`~`, trailing
   `;`), redacted for secrets, and folded into decayed frecency counters
   bucketed by cwd, git root, git branch, and host. Failures (nonzero exit) are
   recorded as negative signal — that's how it learns your environment's quirks
   (`docker-compose`→`docker compose`).
2. **Tool specs.** A background harvester builds flag/subcommand knowledge from,
   in priority order: carapace, fish completions, zsh `_functions`, man-page
   roff source, sandboxed `--help` (bwrap/systemd-run, package-owned binaries
   only), and finally an optional OpenAI-compatible LLM that synthesizes specs
   — including subcommands — from man/`--help` text for everything still
   uncovered. This is what completes a brand-new tool with zero history.
3. **Ranking.** A 17-feature logistic model scores candidates. Every panel
   render logs an impression; every acceptance is a label. An FTRL-Proximal
   trainer retrains periodically and **only promotes new weights if held-out MRR
   improves** — it will not silently get worse.

---

## Privacy

- Secrets (tokens, passwords, bearer headers, high-entropy blobs) are scrubbed
  **before** anything is written to disk, and redacted commands are never
  suggested.
- Leading-space commands and `-deny` patterns are never recorded.
- The socket is `0600` in `$XDG_RUNTIME_DIR`. No TCP listener exists.
- If an LLM endpoint is configured and is not localhost, **command history is
  never sent** — only tool names and public documentation text, with home
  paths scrubbed. Enforced in code: the synthesizer's only inputs are the
  tool name and that text, and the payload rule is covered by a test.
- `ariadne forget <regex>` deletes from memory and rewrites the on-disk log.

---

## Latency

The budget *is* the architecture: keystroke→ghost p50 3ms / p99 10ms / 20ms hard
timeout → fall through to native. Measured on 50k-command synthetic history:

```
entries=50000  p50=0.16ms  p90=0.51ms  p99=1.72ms  max=11.19ms
```

Run `ariadne bench` on your own data.

---

## Architecture

```
zsh (ZLE) / bash (readline)  ──unix socket, text proto──▶  ariadned (Go)
  ghost + panel                                    atomic.Pointer[Index]  ← lock-free reads
  ownership-gated Tab                              ingest chan → fold → rebuild
  circuit breaker                                  background: harvest, train, snapshot
  fall through to native                           JSONL log + gob snapshot (Store iface)
```

Full design rationale and the corrections to the original brief are in
`ariadne-architecture.md`.

---

## Limitations (honest)

- **bash port deltas**: the bash integration (`shell/ariadne.bash`) requires
  bash ≥ 5 and a socket bridge (`socat`, openbsd `nc`, or `ncat`), and supports
  the **emacs keymap only** — `set -o vi` keeps history learning but loses the
  live layer. Ghost text and the panel are painted with raw ANSI (bash has no
  POSTDISPLAY), so non-ASCII input, bracketed paste, and unbound editing keys
  don't re-query until the next bound keystroke, and multi-line prompts/buffers
  disable painting (Tab still works). Lines that fail to parse (e.g. a lone
  `do`) are invisible to both shells' hook mechanisms and are never ingested.
- **fish** is designed but not built.
- **Multi-host merge** is not implemented; each host learns independently.
- **carapace values** (dynamic completions) aren't consumed live yet — Ariadne
  records that carapace covers a tool but delegates dynamic value completion to
  the shell.
- The **panel may not earn its place**. Ghost text is nearly free; a 3-line
  panel above every prompt costs attention. Acceptance-by-rank is tracked in
  `stats` — if rows 2 and 3 are rarely chosen, set `ARIADNE_PANEL_LINES=0`.
- The **dumbest version** (trie + cwd-frecency, no ML) already captures much of
  the value. The learned ranker has to beat that baseline on your data to
  justify itself; `stats` shows whether it does.
