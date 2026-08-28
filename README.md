# Ariadne — Your Terminal, Anticipating You

**Ariadne watches how you use your shell and learns to complete your commands before you finish typing them.** It layers on top of zsh and bash with sub-millisecond ghost text, a ranked suggestion panel, and full native-completion fallback. A single static Go binary, no dependencies, no cloud, no telemetry.

<br>

```
$ docker com<TAB>
                                    ┌─────────────────────────────────────────────┐
$ docker compose up -d               │ 1  docker compose up         412×  cwd     │
  ^^^^^^^^^^^^^^^ ghost text         │ 2  docker compose logs       89×   cwd     │
                                     │ 3  docker compose down       54×   global  │
                                     └─────────────────────────────────────────────┘
```

<br>

## Why Ariadne?

Shell history is a goldmine of intent and every shell throws it away. `Ctrl-R` is archaeology — you dig for what you already did. Tab completion knows flags but has no memory of *you*.

Ariadne closes that gap. It learns from every command you run, scores candidates with a 17-feature logistic model trained on your accept/reject behavior, and drops into your keystroke loop with ghost text and a suggestion panel — **all in under half a millisecond** so you never feel it.

When you type `k<TAB>`, it knows you mean `kubectl get pods` in `~/infra/` but `kill %1` in `~/dev/`. New tools whose `--help` Ariadne has scraped complete from the first keystroke, even with zero history. And it never shadows your shell's native completers for paths, branches, hosts, or files — it knows when to step aside.

<br>

## Performance

The architecture exists to serve one number:

```
$ ariadne bench 5000
n=5000  p50=0.16ms  p90=0.51ms  p99=1.72ms  max=11.19ms
```

| Budget | Target | Measured (50k entries) |
|---|---|---|
| Keystroke → ghost | 3ms p50 | **0.16ms** |
| Keystroke → ghost | 10ms p99 | **1.72ms** |
| Hard fallback timeout | 20ms | **11.2ms** worst case |

The query path is lock-free — the daemon publishes a new immutable index via `atomic.Pointer`, and every keystroke reads it without contention. There are no mutexes on the hot path. If the daemon is ever unreachable, a circuit breaker self-disables the widget and falls through to native completion.

<br>

## How It Learns

Ariadne has three independent learning systems, all running offline so the keystroke is never blocked:

### 1. History (every command you run)

Commands are normalized, redacted for secrets, and folded into decayed frecency counters. Ariadne tracks *where* you run things — exact cwd, git root, git branch, host — and surfaces what's relevant to where you are. Failed commands (non-zero exit codes) are recorded as negative signal, so `docker-compose` in an environment where that binary doesn't exist drifts below `docker compose` in the ranking.

### 2. Tool Specs (before any history exists)

A background harvester builds flag and subcommand knowledge from, in priority order:

| Source | Confidence | What it yields |
|---|---|---|
| **Carapace** | 0.95 | Full structured specs from the Carapace ecosystem |
| **Fish completions** | 0.85 | Declarative flag/subcommand definitions |
| **Zsh `_functions`** | 0.70 | Extracted from `_git`, `_kubectl`, etc. |
| **Man page roff source** | 0.65 | Parsed from raw roff (not rendered text — structure matters) |
| **Sandboxed `--help`** | 0.55 | Executed under `bwrap` or `systemd-run` with no network, no home |
| **LLM synthesis** | 0.40 | Optional OpenAI-compatible model synthesizes flags + subcommands from man pages |

The harvest runs every 12 hours. If you install a new tool, Ariadne knows its flags within a day — and if you configure an LLM endpoint, it can extract subcommands from `--help` text, covering tools the regex parsers can't handle. Synthesis is off by default and strictly local-first.

### 3. Ranking (learns what you actually pick)

Every suggestion panel is an *impression*. Every Tab-accept or ignore is a *label*. Ariadne trains a **17-feature logistic model** via FTRL-Proximal (Follow The Regularized Leader) on your accept/reject log. The critical detail: **new weights are only promoted if held-out MRR (Mean Reciprocal Rank) improves.** The model will never silently get worse.

The 17 features include: success count, failure count, recency decay, cwd exact/ancestor match, repo, branch, host, bigram probability, session recency, match quality, length penalty, time-of-day, spec source, spec confidence, tool presence on PATH, and last-accepted feedback.

<br>

## Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│  Shell (zsh ZLE / bash readline)                                 │
│  ┌─────────────┐  ┌──────────┐  ┌──────────────┐                │
│  │ ghost text   │  │ panel    │  │ Tab: owns?   │                │
│  │ (POSTDISPLAY)│  │ (zle -M) │  │ yes→cycle    │                │
│  │              │  │          │  │ no→native    │                │
│  └──────┬───────┘  └────┬─────┘  └──────┬───────┘                │
│         └────────────────┼──────────────┘                        │
│              Unix socket, text protocol                          │
└──────────────────────────┼───────────────────────────────────────┘
                           │
┌──────────────────────────┼───────────────────────────────────────┐
│  ariadned (Go daemon)    ▼                                       │
│  ┌─────────────────────────────────────────────────────────────┐ │
│  │  atomic.Pointer[Index]   ← lock-free reads, no query mutex │ │
│  └─────────────────────────────────────────────────────────────┘ │
│  ┌──────────┐  ┌──────────┐  ┌──────────────┐  ┌─────────────┐  │
│  │ ingest   │  │ harvest  │  │ FTRL trainer │  │ snapshot    │  │
│  │ channel  │  │ 12h tick │  │ every 500    │  │ 2min tick   │  │
│  │ buffered │  │          │  │ impressions  │  │             │  │
│  └────┬─────┘  └────┬─────┘  └──────┬───────┘  └──────┬──────┘  │
│       │             │               │                  │         │
│  ┌────▼─────────────▼───────────────▼──────────────────▼───────┐ │
│  │  Store: JSONL event log + gob snapshot (no SQLite, no cgo) │ │
│  └─────────────────────────────────────────────────────────────┘ │
└──────────────────────────────────────────────────────────────────┘
```

The design rationale lives in the package doc comments — start with `internal/daemon/daemon.go` (ownership, ingest) and `internal/rank/rank.go` (features, FTRL trainer).

### Key design decisions

- **Zero dependencies.** The `go.mod` has no `require` block. The entire binary is stdlib Go.
- **Lock-free query path.** The daemon publishes whole `Index` values via `atomic.Pointer`. Query goroutines never touch a mutex.
- **Ownership-gated Tab.** A sub-2ms check decides whether Ariadne or the shell's native completer handles the token. Paths, branches, and hosts are never shadowed.
- **circuit breaker.** After 5 consecutive timeouts, the shell widget self-disables for 5 minutes and falls through to native completion. An unreliable completer is worse than no completer.
- **Two codecs, one socket.** The shell widget speaks a text line protocol; the Go CLI speaks length-prefixed JSON. The daemon auto-detects which on accept.

<br>

## Install

```bash
# 1. Build (Go 1.22+)
go build -o ~/.local/bin/ariadned ./cmd/ariadned
go build -o ~/.local/bin/ariadne  ./cmd/ariadne

# 2. Install and start the daemon (socket-activated user service)
#    The shipped unit sets the daemon's PATH to include ~/.local/bin — the
#    daemon's tool-presence feature and harvest scan use its PATH, and a
#    systemd default PATH would make every ~/.local/bin tool (herdr, ariadne,
#    …) invisible: their candidates lose the tool_present boost and never
#    get a tool spec. If you override ExecStart or PATH, keep ~/.local/bin.
install -Dm644 systemd/ariadned.service ~/.config/systemd/user/ariadned.service
install -Dm644 systemd/ariadned.socket  ~/.config/systemd/user/ariadned.socket
systemctl --user daemon-reload
systemctl --user enable --now ariadned.socket

# 3. Hook your shell
echo 'source /path/to/ariadne/shell/ariadne.zsh'  >> ~/.zshrc   # zsh
echo 'source /path/to/ariadne/shell/ariadne.bash' >> ~/.bashrc  # bash

# 4. Seed from your existing history
ariadne import
```

Open a new shell. Start typing. You'll see greyed-out ghost text and a suggestion panel within a few keystrokes.

**Bash notes:** The bash integration requires bash ≥ 5, a socket bridge (`socat`, `openbsd-netcat`, or `ncat`), and the emacs keymap. See `shell/ariadne.bash` for details.

<br>

## Keys

| Key | Action |
|---|---|
| Type | Inline ghost text + suggestion panel |
| `Tab` | Accept/cycle Ariadne's suggestion, or native completion if Ariadne doesn't own the token |
| `→` / `Ctrl-F` | Accept the inline ghost text |
| `Alt-1/2/3` | Pick panel row 1, 2, or 3 |
| `Enter` | Run command (records a rejection if the panel was shown and ignored) |

<br>

## Commands

```
ariadne query <buf>     Show what would be suggested for this buffer
ariadne stats           Entries, tool coverage by source, latency, learned weights
ariadne doctor          Health check with real latency measurement
ariadne import [file]   Import zsh/bash/fish history (auto-detects if no path given)
ariadne harvest         Rescan $PATH and rebuild tool specs (including LLM synthesis)
ariadne train           Force a ranker training round
ariadne forget <regex>  Permanently delete matching history from memory and disk
ariadne bench [n]       Measure query latency distribution (default 2000 samples)
```

<br>

## Configuration

Shell-side environment variables — set before sourcing the integration:

| Variable | Default | Meaning |
|---|---|---|
| `ARIADNE_PANEL_LINES` | `3` | Panel rows shown under the prompt; `0` = ghost text only |
| `ARIADNE_GHOST` | `1` | Inline ghost text on/off |
| `ARIADNE_TIMEOUT` | `0.02` | Per-query read timeout in seconds |
| `ARIADNE_MIN_CHARS` | `1` | Minimum characters before querying |
| `ARIADNE_COLOR` | `1` | ANSI color in the panel |

Daemon flags: `-socket`, `-data`, `-deny <regex,regex>` (commands to never record), `-v`.

### LLM Spec Synthesis (optional)

The optional LLM integration runs strictly offline — it synthesizes tool specs from man pages and `--help` text for tools the deterministic sources couldn't cover. It is the only source that recovers subcommands from documentation. Configure via a systemd drop-in at `~/.config/systemd/user/ariadned.service.d/llm.conf`:

```ini
[Service]
Environment=ARIADNE_LLM_ENDPOINT=http://127.0.0.1:8080/v1
Environment=ARIADNE_LLM_MODEL=qwen2.5-coder-7b
Environment=ARIADNE_LLM_MAXTOOLS=20
```

| Variable | Default | Meaning |
|---|---|---|
| `ARIADNE_LLM_ENDPOINT` | _(unset = off)_ | OpenAI-compatible base URL |
| `ARIADNE_LLM_MODEL` | first from `/models` | Model ID |
| `ARIADNE_LLM_KEY` | empty | Bearer token |
| `ARIADNE_LLM_MAXTOOLS` | `20` | Tools to synthesize per harvest run |
| `ARIADNE_LLM_NOTHINK` | `1` | Set to `0` to allow `enable_thinking` for reasoning models |

<br>

## Privacy

Ariadne is a local tool built for people who care about what leaves their machine. Every privacy decision is enforced by construction, not by policy:

- **Secrets are scrubbed before disk write.** Tokens, passwords, API keys, bearer headers, JWT blobs, and high-entropy strings are detected and replaced with `<redacted>` in memory. Redacted commands are never suggested.
- **Leading-space commands and `-deny` patterns are never recorded.** Ariadne also never records its own administration commands (`ariadne forget/import/stats/...`) — that pattern is a built-in default and `-deny` extends it.
- **The socket is `0600`** and lives in `$XDG_RUNTIME_DIR`. No TCP listener exists.
- **Command history is never sent to an LLM**, even if one is configured. The synthesizer's only input is `(tool name, public documentation text)`. No user data can reach the payload — there is no parameter for it.
- **Non-localhost LLM endpoints** are announced loudly in the harvest log. Home paths are scrubbed from documentation text before it leaves the process.
- **`ariadne forget <regex>`** deletes from memory and rewrites the on-disk event log.

<br>

## Limitations

Ariadne is opinionated about what it's good at. Honest limitations:

- **Bash port deltas.** The bash integration needs a socket bridge, only supports the emacs keymap, and uses raw ANSI for ghost text (no `POSTDISPLAY` equivalent in bash). Multi-line prompts disable painting, and non-ASCII input doesn't re-query until the next bound keystroke.
- **Fish** is designed but not built yet.
- **Multi-host merge** is not implemented. Each host learns independently.
- **Carapace values** (dynamic completions like `kubectl get <resource>`) aren't consumed live — Ariadne records that Carapace covers the tool but delegates dynamic value completion to the shell.
- **Subcommand completion** fires only at a fresh token (`tool ␣<TAB>`), not mid-typing (`tool ch<TAB>`).
- **LLM-synthesized specs can hallucinate** despite strict validation. Synthesis is off by default and incremental — coverage of the long tail improves over days, not instantly.
- **The dumbest version** (trie + cwd-frecency, no ML) already captures most of the value. The learned ranker has to beat that baseline on your data. Check `ariadne stats` to see whether it does.

<br>

## Project Structure

```
cmd/ariadne/          CLI client (stats, doctor, bench, query, forget)
cmd/ariadned/         Daemon binary (socket-activated systemd user service)
internal/
  core/               Domain types: Event, Hash, normalization, redaction
  daemon/             Socket listener, ingest loop, query dispatch, ownership logic
  harvest/            Tool spec scraping (carapace, fish, zsh, man, --help, LLM synthesis)
  index/              Lock-free atomic index, prefix/fuzzy search, match quality scoring
  proto/              Wire protocol (text line-based + JSON framing)
  rank/               17-feature logistic model + FTRL-Proximal trainer
  store/              JSONL event log + gob snapshot persistence
shell/ariadne.zsh     ZLE widget integration (zsh/net/socket, no per-keystroke fork)
shell/ariadne.bash    Bash integration (bind -x + coproc, PS0 cleanup)
systemd/              ariadned.service + ariadned.socket (user-scoped)
dist/                 Prebuilt linux-amd64 / linux-arm64 binaries
```

All Go code is in `internal/` except the two `cmd/` entry points. Each package owns its full vertical slice. Zero external dependencies — the `go.mod` has no `require` blocks.

<br>

## Contributing

The latency budget is 3ms p50 / 10ms p99 / 20ms hard timeout per keystroke. PRs touching the hot path should include `ariadne bench` output. New dependencies must be justified — the binary ships as a single static file, and every import is a permanent tax.

Run the full test suite before submitting:

```bash
go test -race ./...
go vet ./...
```
