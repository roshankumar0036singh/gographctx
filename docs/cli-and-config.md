# CLI, Configuration & IDE Integration Guide

## CLI Commands

```text
gographctx <command> [flags] [args]
```

| Command | Purpose |
|---|---|
| `gographctx index <path>` | Discover, parse, resolve, and persist `.ggctx/index.ggctx` + `manifest.json` |
| `gographctx serve [path]` | Load (or build) the index and start the MCP JSON-RPC stdio server |
| `gographctx watch [path]` | Watch filesystem changes incrementally while serving MCP over stdio (or `--no-mcp` headless) |
| `gographctx query <tool> [args]` | Execute any of the 7 MCP tools from the CLI (`--format json` or `text`) |
| `gographctx stats <path>` | Read repository graph & resolution stats directly from `.ggctx/index.ggctx` |
| `gographctx version` | Print `gographctx` CLI version and binary format version |

### Common Flags

| Flag | Description |
|---|---|
| `--config <file>` | Custom `.ggctxrc` file path |
| `--force` | Ignore `manifest.json` smart cache and perform a full re-index |
| `--workers <n>` | Override worker concurrency |
| `--format <json\|text>` | Output format (`json` or `text`) |
| `--verbose` | Emit detailed stage timings and diagnostics to `stderr` |
| `--no-mcp` | (`watch` only) Run file watcher updates without attaching MCP stdio |

> Flags can appear either before or after positional arguments (e.g., `gographctx index ./repo --force`).

---

## Configuration (`.ggctxrc` & Environment Variables)

Configuration precedence: **Defaults → `<repo>/.ggctxrc` (or `--config`) → `GGCTX_*` environment variables → CLI flags**.

`.ggctxrc` supports JSON or `key = value` syntax:

```json
{
  "workers": 8,
  "token_budget": 4000,
  "debounce_ms": 100,
  "compress_snapshots": true,
  "sync_writes": false,
  "wal_compact_threshold_bytes": 4194304,
  "data_dir": ".ggctx",
  "ignore_file": ".ggctxignore",
  "use_gitignore": true,
  "follow_symlinks": false,
  "max_call_chain_depth": 10,
  "max_search_results": 100,
  "languages": ["go", "python", "typescript", "javascript", "rust", "java"]
}
```

Supported environment overrides:
`GGCTX_WORKERS`, `GGCTX_TOKEN_BUDGET`, `GGCTX_DEBOUNCE_MS`, `GGCTX_COMPRESS_SNAPSHOTS`, `GGCTX_SYNC_WRITES`, `GGCTX_WAL_COMPACT_BYTES`, `GGCTX_DATA_DIR`, `GGCTX_IGNORE_FILE`, `GGCTX_USE_GITIGNORE`, `GGCTX_FOLLOW_SYMLINKS`, `GGCTX_MAX_CALL_CHAIN_DEPTH`, `GGCTX_MAX_SEARCH_RESULTS`, `GGCTX_LANGUAGES`.

### `.ggctxignore`

Uses gitignore-compatible syntax (`#` comments, `!` negation, `/` anchoring, `**` recursive globs, directory trailing slashes) layered on top of nested `.gitignore` files and built-in ignores (`.git/`, `node_modules/`, `vendor/`, `.venv/`, `target/`, `.ggctx/`, etc.).

---

## IDE & Coding Agent MCP Setup

### Claude Code
```bash
claude mcp add gographctx -- gographctx serve /path/to/your/repo
```
*(Use `gographctx watch /path/to/your/repo` instead if you want live incremental re-indexing while editing.)*

### Cursor / Windsurf / Claude Desktop / Cline (`mcpServers`)
```json
{
  "mcpServers": {
    "gographctx": {
      "command": "gographctx",
      "args": ["watch", "/absolute/path/to/your/repo"]
    }
  }
}
```

### VS Code Copilot MCP (`.vscode/mcp.json`)
```json
{
  "servers": {
    "gographctx": {
      "type": "stdio",
      "command": "gographctx",
      "args": ["serve", "${workspaceFolder}"]
    }
  }
}
```
