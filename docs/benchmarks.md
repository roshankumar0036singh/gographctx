# Benchmarks & Accuracy Validation

GoGraphCtx includes an automated benchmarking and compiler-oracle evaluation harness in [cmd/ggctx-bench](../cmd/ggctx-bench/main.go) and [internal/bench](../internal/bench/perf.go).

---

## Measured Local Baseline (`go1.25.1`, `windows/amd64`, 16 vCPUs)

Verified via `go run ./cmd/ggctx-bench --corpus flask-synth --tools all --assert-sla`:

| Benchmark / Metric | Measured | Target SLA | Status |
|---|---|---|---|
| **Flask-scale Python repo** (83 `.py` files, 705 nodes, 2,208 edges) | **533.39 ms** | `< 1,000 ms` | **PASS** |
| **Flask-scale peak RSS** | **51.02 MB** | `< 80 MB` | **PASS** |
| **Flask strong-tier call edge ratio** | **100.0%** | `>= 85%` | **PASS** |
| **Go monorepo** (1,000 `.go` files across 50 packages, 6,002 nodes, 8,000 edges) | **1,401.70 ms** | `< 3,000 ms` | **PASS** |
| **Go-1k peak RSS** | **116.92 MB** | `< 150 MB` | **PASS** |
| **Incremental body edit (p50)** | **22.78 ms** | `< 50 ms` | **PASS** |
| **Incremental export edit (p50)** | **22.77 ms** | `< 100 ms` | **PASS** |
| **MCP tool query latency (p95)** | **3.80 ms** | `< 10 ms` | **PASS** |
| **CLI cold startup (`gographctx version`, p50)** | **22.80 ms** | `< 50 ms` | **PASS** |
| **Static stripped binary (`CGO_ENABLED=0`)** | **15.79 MB** | `< 30 MB` | **PASS** |

---

## Multi-Tool Architecture & Capability Comparison

| Tool | Category | Index Architecture | Call Graph | Transitive Impact | Confidence Tiers | Zero External DB / Runtime |
|---|---|---|---|---|---|---|
| **GoGraphCtx** | Code-Graph MCP | In-memory + compact binary snapshot/WAL | Yes | Yes (`get_impact` BFS) | Yes (8 tiers + `LIKELY_CALLS`) | **Yes** (`15.8 MB` static binary) |
| **CodeGraphContext (CGC)** | Code-Graph MCP | Python + external/embedded Cypher DB (Neo4j/FalkorDB/Kuzu) | Yes | Yes | Partial (tier guesses emitted as facts; ~39% strong tiers measured on Flask) | No (`~90` pip deps / DB server) |
| **SCIP** (`scip-python`, `scip-go`) | Compiler Oracle | Language type-checker emit `.scip` protobuf | Yes | External tooling needed | Compiler-exact | No (requires venv/toolchain/build env) |
| **Universal Ctags** | Tag Indexer | Regex/heuristic symbol tags | No | No | No | Yes |
| **ast-grep (`sg`)** | AST Search CLI | Ad-hoc per-query AST pattern matching | No | No | Exact AST syntax | Yes |
| **Serena** | LSP MCP Server | Live background language server proxy (`pyright`/`gopls`) | Yes (LSP call hierarchy) | Limited | Type-checker-backed | No (LSP server setup + 50+ tools) |
| **ripgrep (`rg`)** | Lexical Search | Streaming regex file scan | No | No | Lexical match (includes comments/imports/homonyms) | Yes |

*(Note: When running on CI release runners with comparator CLIs installed in `$PATH`, `ggctx-bench` captures live head-to-head index duration, Peak RSS, query p50/p95, noise ratio, and token footprint across all installed tools.)*

---

## Reproducing Locally

```bash
# Run full performance SLA & SCIP decode unit tests
go test -v -count=1 ./internal/bench/...

# Generate JSON + Markdown comparison report across all available tools
go run ./cmd/ggctx-bench --corpus flask-synth --tools all --assert-sla --out-json bench.json --out-md bench.md

# Benchmark a custom local repository against an existing .scip oracle
go run ./cmd/ggctx-bench --corpus custom --repo /path/to/repo --scip-file /path/to/index.scip --tools gographctx,ctags,ast-grep,ripgrep
```
