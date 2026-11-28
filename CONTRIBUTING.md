# Contributing to GoGraphCtx

Thank you for your interest in contributing to **GoGraphCtx**! Every PR—whether it adds a new language resolver, improves call-graph precision, optimizes memory, or improves documentation—is warmly welcomed.

---

## Development Setup

### Prerequisites
- **Go 1.25+** (`go version`)
- **C compiler (`gcc`)** *(optional for normal builds; required only when running `go test -race` locally)*
- Normal builds are **100% pure Go** (`CGO_ENABLED=0`) via `gotreesitter` compiled-in grammars.

```bash
git clone https://github.com/roshankumar0036singh/gographctx.git
cd gographctx

# Build the CLI binary (~15.8 MB stripped)
make build

# Run fast unit tests
go test -short -count=1 ./...

# Run full test suite (includes 1000-file Go + Flask SLA checks)
make test
```

---

## Repository Architecture

```text
cmd/
  gographctx/           # Main CLI binary (index, serve, watch, query, stats, version)
  ggctx-bench/          # SLA, SCIP accuracy & multi-tool comparison benchmark runner
internal/
  config/               # .ggctxrc, .ggctxignore, GGCTX_* env loader
  graph/                # Compact in-memory graph, ReadView, search, impact BFS
  storage/              # Binary snapshot (.ggctx) + CRC-protected WAL reader/writer
  parser/               # Parallel parse coordinator & NameIndex
    treesitter/         # Pure-Go gotreesitter grammar pool
    lang/               # Per-language AST extractors (Go, Python, TS/JS, Rust, Java)
  resolver/             # 8-tier confidence engine, receiver type inference & interfaces
    lang/               # Language-specific import, call & hierarchy resolvers
  pipeline/             # Discovery -> parse -> skeleton -> resolve -> compact snapshot
  watcher/              # DepTracker, incremental engine & debounced fsnotify watcher
  mcp/                  # Stdio JSON-RPC server, 7 tool handlers, token-budget formatter
  bench/                # Performance corpus generators, OS RSS capture, SCIP oracle decoder
docs/                   # User & contributor documentation
```

---

## Common Developer Tasks

### Adding or Improving a Language Parser / Resolver
1. Inspect existing language modules under `internal/parser/lang/` and `internal/resolver/lang/`.
2. Preserve deterministic ordering (sort extracted items cleanly before returning).
3. Never emit speculative guesses as `EdgeCalls` (`TierSamePackage` or stronger)—uncertain multi-candidate edges must remain `TierTypeNarrowed` (`EdgeLikelyCalls`) or `CallSite` (`CallStatusAmbiguous` / `CallStatusUnresolved`).
4. Add unit and cross-file resolution tests covering imports, receiver type inference, inheritance/implements, and edge-case syntax.
5. Register the language in [pipeline.go](internal/pipeline/pipeline.go) (`DefaultParserRegistry` / `DefaultResolverRegistry`).

### Updating MCP Golden Tests
When intentionally modifying MCP response envelopes or fixture schemas:

```bash
UPDATE_GOLDEN=1 go test -v ./internal/mcp/...
```

Inspect every diff in `internal/mcp/testdata/*.golden.json` before committing.

### Running Benchmarks & Accuracy Checks
```bash
# Run SLA + multi-tool comparison report locally
go run ./cmd/ggctx-bench --corpus flask-synth --tools all --assert-sla
```

---

## Pull Request Checklist

- [ ] Code formatted cleanly: `gofmt -l .` produces no output
- [ ] Static checks pass: `go vet ./...` (and `staticcheck ./...` if installed)
- [ ] Unit tests pass: `go test -short -count=1 ./...`
- [ ] Integration / SLA tests verified when changing `pipeline/`, `storage/`, `watcher/`, `mcp/`, or `bench/`
- [ ] Fuzz targets exercised (`go test -run=^$ -fuzz=... -fuzztime=10s`) if touching binary decoding, RPC framing, or ignore matching
- [ ] Documentation updated in `docs/` or `README.md` if CLI flags, config keys, or MCP tool schemas changed

> Note: Full integration workflows run automatically once a PR receives maintainer approval, while fast unit and lint/security workflows run on every PR push.
