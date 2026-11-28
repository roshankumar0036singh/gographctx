# GoGraphCtx MCP Tools Reference

GoGraphCtx implements the **Model Context Protocol (MCP)** over stdio (`initialize`, `ping`, `tools/list`, `tools/call`) with both **newline-delimited JSON** and **`Content-Length`** framing. Rather than exposing raw graph queries or bloating LLM context with 30–50 overlapping tools, GoGraphCtx exposes **7 focused, read-only tools** with token budgets (`default: 4000`) and confidence metadata.

---

## Envelope Format (Returned by All 7 Tools)

Every `tools/call` response wraps the tool result in a deterministic JSON envelope:

```json
{
  "tool": "get_impact",
  "summary": "2 impacted symbol(s) across 2 file(s) within depth 3",
  "truncated": false,
  "total": 2,
  "returned": 2,
  "confidence_summary": {
    "certain": 2,
    "likely": 0,
    "unresolved": 0,
    "overall_label": "certain",
    "min_confidence": 0.95,
    "max_confidence": 0.95
  },
  "metadata": {
    "tool": "get_impact",
    "index_version": 1,
    "repo_name": "my-service",
    "timestamp": "2026-11-28T00:00:00Z",
    "resolution_coverage": 1.0,
    "token_budget": 4000,
    "estimated_tokens": 382
  },
  "data": { ... }
}
```

- **Explicit Truncation:** When results exceed `token_budget` (estimated as `ceil(chars / 4)`), items are binary-searched down to fit strictly inside the budget and `"truncated": true`, `"total": N`, `"returned": M` are set explicitly.
- **Path Stripping:** All paths returned inside MCP payloads are normalized POSIX repo-relative paths—absolute workspace directories are never leaked.

---

## 1. `find_symbol`

Find functions, methods, classes, structs, interfaces, enums, variables, properties, or type aliases.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `name` | `string` | No | Name or glob pattern (`*`, `?`; plain text matches case-insensitive substring) |
| `kind` | `string` | No | `Function`, `Class`, `Struct`, `Interface`, `Enum`, `Variable`, `TypeAlias`, `Property` |
| `file` | `string` | No | Repo-relative file path or glob |
| `language` | `string` | No | `go`, `python`, `typescript`, `tsx`, `javascript`, `rust`, `java` |
| `exported_only` | `boolean` | No | Return only exported/public symbols |
| `limit` | `integer` | No | Max items `1..200` (default `50`) |

---

## 2. `get_callers`

Return direct callers of a target function or method, enriched with resolution tier, numeric confidence, label (`"certain"` or `"likely"`), and exact source call sites.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `symbol` | `string` | **Yes** | Short name, `qualified_name` (e.g. `pkg::Type::Method`), or 32-char UID hex |
| `file` | `string` | No | Optional repo-relative file path to disambiguate colliding short names |
| `include_likely` | `boolean` | No | Include Tier 7 `LIKELY_CALLS` candidates (default `true`) |
| `limit` | `integer` | No | Max edges `1..200` (default `50`) |

---

## 3. `get_callees`

Return direct functions and methods invoked by a source symbol. Accepts the exact same parameters (`symbol`, `file`, `include_likely`, `limit`) as `get_callers`.

---

## 4. `get_impact`

Compute transitive reverse-dependency blast radius via BFS up to `max_depth` hops, preserving both the hop path (`via`) and weakest-link certainty (`certain: true|false`, `effective_tier`).

| Parameter | Type | Required | Description |
|---|---|---|---|
| `symbol` | `string` | **Yes** | Target symbol name, `qualified_name`, or 32-char UID hex |
| `file` | `string` | No | Optional disambiguating file path |
| `max_depth` | `integer` | No | Max traversal hops `1..10` (default `3`) |
| `limit` | `integer` | No | Max impacted entries `1..200` (default `50`) |

---

## 5. `file_outline`

Return a structured outline (module, imports, classes/structs/interfaces/fields, functions/methods, variables, type aliases) for a single indexed file.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `file` | `string` | **Yes** | Repository-relative file path |
| `include_private` | `boolean` | No | Include unexported/private symbols (default `true`) |

---

## 6. `search_code`

Multi-criteria search across symbols with cyclomatic complexity filtering and pagination.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `name_pattern` | `string` | No | Symbol name glob or substring |
| `qname_pattern` | `string` | No | Qualified name glob or substring |
| `kinds` | `string[]` | No | Node kind list |
| `languages` | `string[]` | No | Language list |
| `file_pattern` | `string` | No | File glob or substring |
| `exported_only` | `boolean` | No | Exported symbols only |
| `min_complexity` | `integer` | No | Minimum cyclomatic complexity (`>= 0`) |
| `max_complexity` | `integer` | No | Maximum cyclomatic complexity (`0` = unbounded) |
| `offset` | `integer` | No | Pagination offset |
| `limit` | `integer` | No | Max items `1..200` (default `50`) |

---

## 7. `repo_overview`

Aggregate repository statistics: indexed file count, symbol/edge counts, language distribution, resolution coverage, complexity buckets, and top complex functions.

| Parameter | Type | Required | Description |
|---|---|---|---|
| `include_top_complex` | `boolean` | No | Include `top_complex_functions` list (default `true`) |
| `limit` | `integer` | No | Max complex functions `1..50` (default `10`) |
