# Security Policy

## Supported Versions

GoGraphCtx is actively maintained on `main`. Security fixes are backported to the latest minor release tag (`v*.*.*`).

| Version | Supported |
|---------|-----------|
| Latest release (`v*`) & `main` | Yes |
| Older releases | No |

## Threat Model & Built-In Safeguards

- **Local-only operation:** GoGraphCtx runs 100% locally against files on disk and never transmits source code, symbols, or telemetry over the network.
- **No dynamic query execution:** The MCP server exposes 7 typed read-only graph tools (`find_symbol`, `get_callers`, `get_callees`, `get_impact`, `file_outline`, `search_code`, `repo_overview`) with no raw Cypher/SQL/shell execution surface.
- **Defensive wire framing & persistence guards:** JSON-RPC `Content-Length` framing enforces a 32 MiB message cap (`maxRPCMessageBytes`), and binary snapshot/WAL readers validate CRCs, version headers, and preallocation bounds (continuously fuzzed via `FuzzSnapshotAndWALDecode` and `FuzzMCPReadRPCMessage`).
- **Automated vulnerability scanning:** Every PR runs `govulncheck ./...`, `staticcheck ./...`, `go vet ./...`, and `go mod verify` via [lint-security.yml](.github/workflows/lint-security.yml).

## Reporting a Vulnerability

Please **do not** open a public GitHub issue for security vulnerabilities.

1. Use GitHub's **Private Vulnerability Reporting** ("Security" tab → "Report a vulnerability") on [roshankumar0036singh/gographctx](https://github.com/roshankumar0036singh/gographctx/security), **or**
2. Contact the maintainer directly with reproduction steps, affected commit/tag, and potential impact.

You can expect an acknowledgment within 48 hours and a coordinated fix/advisory timeline prior to public disclosure.
