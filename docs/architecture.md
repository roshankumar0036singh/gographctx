# GoGraphCtx Architecture

GoGraphCtx is designed from the ground up around **deterministic single-binary performance** and **honest confidence metadata**—without requiring Neo4j, FalkorDB, SQLite servers, or Python virtual environments.

---

## End-to-End Indexing Pipeline

```text
  Repository Files
        │
        ▼
  [1. File Discovery]       Nested .gitignore / .ggctxignore, symlink cycle guard
        │
        ▼
  [2. Smart Manifest]       Skip unchanged files via (size, modTime, lang) diff
        │
        ▼
  [3. Parallel AST Parse]   Pure-Go gotreesitter grammar pool -> ParsedFile + NameIndex
        │
        ▼
  [4. Skeleton Build]       Two-pass Graph construction with deterministic xxHash128 UIDs
        │
        ▼
  [5. 8-Tier Resolution]    Imports + Local Type Inference + Calls + Inheritance/Decorators
        │
        ▼
  [6. Compact Snapshot]     Atomic .ggctx/index.ggctx (zstd) + CRC32C WAL (.ggctx/wal.log)
```

---

## Confidence Tier System

Unlike heuristic indexers that emit alphabetical guesses or substring matches as verified call edges, every edge in GoGraphCtx carries a `ResolutionTier`:

| Tier | Name | Confidence | Edge Kind | Meaning |
|---|---|---|---|---|
| 0 | `structural` | `1.00` | `CONTAINS` / `IMPORTS` / `INHERITS` / `IMPLEMENTS` | Direct AST structure or interface satisfaction |
| 1 | `self receiver` | `1.00` | `CALLS` | Explicit `self` / `this` / `super` / receiver method call |
| 2 | `local definition` | `0.98` | `CALLS` | Symbol defined within the same source file |
| 3 | `qualified import` | `0.95` | `CALLS` | Unambiguously imported module/package/symbol |
| 4 | `receiver type` | `0.90` | `CALLS` | Inferred receiver variable type + resolved method |
| 5 | `module resolution` | `0.88` | `CALLS` | Unique match via language module system |
| 6 | `same package` | `0.85` | `CALLS` | Unique match within sibling package files |
| 7 | `type narrowed` | `0.60` | `LIKELY_CALLS` | Multiple candidates after type narrowing (never promoted to certain) |
| 8 | `unresolved` | `0.00` | `CallSite` only | Stored with `CallSite` evidence; never emitted as a graph edge |

---

## Storage Engine (Snapshot + WAL)

- **Compact binary format (`.ggctx/index.ggctx`):** Interned string table, uvarint node/edge encoding, optional zstd frame compression, atomic temp-file write + rename.
- **Write-Ahead Log (`.ggctx/wal.log`):** Appends CRC32C-checksummed file replacement/deletion records on incremental edits; auto-compacts when exceeding `wal_compact_threshold_bytes` (default 4 MiB) and cleanly tolerates trailing partial WAL frames on crash recovery.

---

## Incremental Engine & File Watcher

- **`DepTracker`** maintains reverse dependency sets across files and negative `CallSite` lookup keys so newly added exports automatically re-resolve previously unresolved call sites.
- **Body-only short-circuit:** Editing a function body without changing exports, signatures, or bases re-indexes only that file (`~22.8ms` median on Flask-scale Python).
- **Debounced `FileWatcher`:** Wraps `fsnotify` with non-blocking draining, layered per-directory ignore matchers, canonical path normalization, and configurable burst debouncing (`100ms` default).
