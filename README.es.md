<div align="center">

<p>
  <a href="README.md">English</a> ·
  <a href="README.zh-CN.md">简体中文</a> ·
  <a href="README.ja.md">日本語</a> ·
  <a href="README.ko.md">한국어</a> ·
  <strong>Español</strong> ·
  <a href="README.pt-BR.md">Português (BR)</a> ·
  <a href="README.de.md">Deutsch</a> ·
  <a href="README.fr.md">Français</a>
</p>

<img src="docs/assets/hero-banner.png" alt="GoGraphCtx — Indexador de grafo de código en un solo binario y servidor MCP" width="100%"/>

<h1>GoGraphCtx</h1>

<p>
  <strong>Indexador de Grafo de Código en Un Solo Binario, Sin Dependencias y Servidor MCP con Niveles de Confianza</strong>
</p>

<p>
  <a href="https://github.com/roshankumar0036singh/gographctx/actions/workflows/unit-tests.yml"><img src="https://github.com/roshankumar0036singh/gographctx/actions/workflows/unit-tests.yml/badge.svg" alt="Unit Tests"></a>
  <a href="https://github.com/roshankumar0036singh/gographctx/actions/workflows/lint-security.yml"><img src="https://github.com/roshankumar0036singh/gographctx/actions/workflows/lint-security.yml/badge.svg" alt="Lint & Security"></a>
  <img src="https://img.shields.io/badge/Go-1.25+-00ADD8?logo=go&logoColor=white" alt="Go 1.25+">
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-MIT-green.svg" alt="MIT License"></a>
  <img src="https://img.shields.io/badge/Binary-15.8%20MB%20Static-22c55e" alt="15.8 MB Static Binary">
  <img src="https://img.shields.io/badge/Database-Zero%20External%20DB-0ea5e9" alt="Zero External DB">
  <a href="CONTRIBUTING.md"><img src="https://img.shields.io/badge/PRs-Welcome-brightgreen.svg" alt="PRs Welcome"></a>
</p>

<p>
  <em>Proporciona a Claude Code, Cursor, Windsurf, VS Code Copilot y Cline un grafo de llamadas preciso, análisis de impacto y estructura del repositorio en <strong>533ms</strong>—sin Neo4j, Docker ni Python.</em>
</p>

</div>

---

## Demostración Interactiva

<p align="center">
  <img src="docs/assets/terminal-demo.svg" alt="Demo en terminal de GoGraphCtx" width="100%"/>
</p>

<details>
<summary><strong>Ver Infografía Comparativa y Arquitectura</strong></summary>
<p align="center">
  <img src="docs/assets/demo-preview.png" alt="Comparación visual GoGraphCtx vs Grep y BD de grafos" width="100%"/>
</p>
</details>

---

## ¿Por Qué Usar GoGraphCtx?

1. **Elimina el "Impuesto de Contexto" de `grep`:** En lugar de gastar miles de tokens leyendo comentarios o símbolos homónimos, responde con grafos estructurados bajo presupuestos estrictos (`4000` tokens por defecto).
2. **Niveles de Confianza Honestos (8 Tiers):** Distingue aristas verificadas (`CALLS`) de candidatos inciertos (`LIKELY_CALLS` / `CallSite`), evitando alucinaciones al refactorizar.
3. **Binario Estático de 15.8 MB Sin Dependencias:** Gramáticas `gotreesitter` compiladas en Go puro (`CGO_ENABLED=0`) para **Go, Python, TypeScript/TSX, JavaScript/JSX, Rust y Java**.
4. **Actualizaciones Incrementales en Tiempo Real (`~22.8ms`):** `fsnotify` re-indexa únicamente los archivos modificados y sus dependientes mientras sirve MCP por stdio.

---

## Inicio Rápido

```bash
go install github.com/roshankumar0036singh/gographctx/cmd/gographctx@latest

gographctx index ./your-repo
gographctx stats ./your-repo
gographctx query get_impact pkg::Service::Handle ./your-repo
```

```bash
# Integración directa con Claude Code
claude mcp add gographctx -- gographctx watch /absolute/path/to/your/repo
```

---

## Benchmarks Medidos y Comparativa Multi-Herramienta

Medido con [`cmd/ggctx-bench`](cmd/ggctx-bench/main.go) (`go1.25.1`, `windows/amd64`, 16 vCPUs):

| Métrica | Resultado Medido | Objetivo SLA |
|---|---|---|
| **Repositorio Python tipo Flask** (83 `.py`) | **533.39 ms** / **51.02 MB RSS** | `< 1s` / `< 80 MB` |
| **Monorepo Go** (1,000 `.go`) | **1,401.70 ms** / **116.92 MB RSS** | `< 3s` / `< 150 MB` |
| **Proporción de aristas de alta confianza** | **100.0%** | `>= 85%` |
| **Actualización incremental (p50)** | **22.78 ms** (cuerpo) / **22.77 ms** (exportación) | `< 50ms` / `< 100ms` |
| **Latencia MCP (p95) / Tamaño binario** | **3.80 ms** / **15.79 MB** | `< 10ms` / `< 30 MB` |

Consulta [docs/benchmarks.md](docs/benchmarks.md) para la comparativa completa frente a **CGC**, **SCIP**, **Universal Ctags**, **ast-grep**, **Serena** y **ripgrep** (incluyendo **98.4 % de precisión en Flask** vs. **69.2 % en CGC**, divulgación explícita `unresolved_hints` y `~492 ms` frente a `92.7 s` en indexación).

## Documentación Completa

- [Herramientas MCP](docs/mcp-tools.md) · [CLI y Configuración](docs/cli-and-config.md) · [Arquitectura](docs/architecture.md) · [Lenguajes Soportados](docs/languages.md) · [Benchmarks](docs/benchmarks.md) · [Contribuir](CONTRIBUTING.md)

Si GoGraphCtx te resulta útil, ¡deja una **Star** en el repositorio!
