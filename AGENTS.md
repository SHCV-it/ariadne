# Repository Guidelines

## Project Structure & Module Organization

```
cmd/ariadne/       → CLI client binary (stats, doctor, bench, query, forget)
cmd/ariadned/      → daemon binary (socket-activated user service)
internal/
  core/            → shared domain types: Event, Hash, CtxKind, normalisation, redaction
  daemon/          → socket listener, ingest loop, indexing (daemon.go + daemon_test.go)
  harvest/         → tool-spec scraping (carapace, fish, zsh, man, --help sandbox)
                     + LLM spec synthesis (llm.go, OpenAI-compatible, env-configured)
  index/           → lock-free atomic index serving prefix queries
  proto/           → wire protocol (text-line based) + text framing
  rank/            → 17-feature logistic model + FTRL-Proximal trainer
  store/           → JSONL event log + gob snapshot persistence
shell/ariadne.zsh  → ZLE widget integration (zsh/net/socket, no per-keystroke fork)
shell/ariadne.bash → bash integration (bind -x + coproc nc/socat bridge, PS0 cleanup)
systemd/           → ariadned.service + ariadned.socket (user-scoped)
dist/              → prebuilt linux-amd64 / linux-arm64 binaries
```

All Go code lives under `internal/` except the two `cmd/` entry points. The
`internal/` tree is organised by architectural concern, not by layer — each
package owns its full vertical slice.

## Build, Test, and Development Commands

```sh
# Build both binaries (Go 1.22+)
go build -o ~/.local/bin/ariadned ./cmd/ariadned
go build -o ~/.local/bin/ariadne  ./cmd/ariadne

# Run the test suite
go test ./...

# Run tests with race detection
go test -race ./...

# Run a specific package's tests verbosely
go test -v ./internal/daemon/...

# Vet and staticcheck
go vet ./...
```

There is no Makefile — the build surface is deliberately minimal. The daemon
is intended to run as a systemd user service; see `README.md` for install steps.

## Coding Style & Naming Conventions

- **Standard Go formatting** via `gofmt` / `goimports` — no custom formatter
  config.
- **Package doc comments** in every package (see `core/core.go` for the
  canonical example).
- **Exported symbols** use PascalCase; unexported use camelCase. Acronyms
  follow Go convention (`HashOf`, `GitRoot`, not `HashOf` → `HashOf`).
- **Comments** for exported symbols explain *why*, not what — the README and
  architecture doc carry longer-form rationale.
- **No external dependencies** beyond the Go standard library. The `go.mod`
  must stay empty of `require` blocks.

## Testing Guidelines

- Framework: standard `testing` package. No external test libraries.
- Test files live alongside package code (e.g., `internal/daemon/daemon_test.go`).
- Use `t.Helper()` for shared setup utilities (see `boot()` in daemon_test.go).
- Use `t.TempDir()` for isolated state — the daemon reads/writes files and a
  socket, so tests must never touch production paths.
- Run the full suite before submitting: `go test -race ./...`.

## Commit & Pull Request Guidelines

- **Commit style**: imperative mood, lowercase, short summary. Prefix with the
  package or area when helpful (e.g., `daemon: fix snapshot race on SIGHUP`,
  `rank: cap FTRL learning rate at 0.5`).
- **PR descriptions**: include context on what changed, why, and any
  latency/accuracy impact. Ariadne's keystroke budget is ~3ms; PRs touching
  the hot path should include bench output (`ariadne bench` or `go test -bench`).
- **Binary size and deps**: new dependencies must be justified. The binary
  ships as a single static file; every import is a permanent tax.

## Architecture & Design Constraints

- **Latency budget**: 3ms p50 / 10ms p99 / 20ms hard timeout per keystroke.
  The index is served via `atomic.Pointer` for lock-free reads. Never add a
  mutex to the query path.
- **Privacy-first**: secrets are redacted *before* any disk write. Leading-space
  commands and `-deny` patterns are never recorded. The socket is `0600`.
- **Fall-through semantics**: Ariadne must never break native shell completion.
  If ownership of the current token can't be decided in <2ms, delegate to the
  shell's own completer.
- **Package-owned binaries only** for `--help` sandboxing via bwrap/systemd-run.
