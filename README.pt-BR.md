<div align="center">

<p>
  <a href="README.md">English</a> ·
  <a href="README.zh-CN.md">简体中文</a> ·
  <a href="README.ja.md">日本語</a> ·
  <a href="README.ko.md">한국어</a> ·
  <a href="README.es.md">Español</a> ·
  <strong>Português (BR)</strong> ·
  <a href="README.de.md">Deutsch</a> ·
  <a href="README.fr.md">Français</a>
</p>

<img src="docs/assets/hero-banner.png" alt="GoGraphCtx — Indexador de grafo de código em binário único e servidor MCP" width="100%"/>

<h1>GoGraphCtx</h1>

<p>
  <strong>Indexador de Grafo de Código em Binário Único, Zero Dependências e Servidor MCP com Níveis de Confiança</strong>
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
  <em>Entregue ao Claude Code, Cursor, Windsurf, VS Code Copilot e Cline um grafo de chamadas preciso, análise de impacto e estrutura do repositório em <strong>533ms</strong>—sem Neo4j, Docker ou Python.</em>
</p>

</div>

---

## Demonstração Rápida

<p align="center">
  <img src="docs/assets/terminal-demo.svg" alt="Demonstração de terminal do GoGraphCtx" width="100%"/>
</p>

<details>
<summary><strong>Ver Infográfico Comparativo e Arquitetura</strong></summary>
<p align="center">
  <img src="docs/assets/demo-preview.png" alt="Comparativo GoGraphCtx vs Grep e bancos de grafo" width="100%"/>
</p>
</details>

---

## Por Que Usar o GoGraphCtx?

1. **Fim do desperdício de tokens com `grep`:** Responde consultas estruturais dentro de um orçamento configurável (`4000` tokens padrão), sem poluir o contexto com comentários ou homônimos.
2. **8 Níveis de Confiança Transparentes:** Separa arestas confirmadas (`CALLS`) de hipóteses (`LIKELY_CALLS` / `CallSite`), garantindo segurança em refatorações automatizadas.
3. **Binário Estático de 15.8 MB sem Banco Externo:** Parsers `gotreesitter` em Go puro (`CGO_ENABLED=0`) para **Go, Python, TypeScript/TSX, JavaScript/JSX, Rust e Java**.
4. **Watcher Incremental em Tempo Real (`~22.8ms`):** `fsnotify` atualiza somente arquivos modificados e dependentes afetados durante sessões MCP ativas.

---

## Início Rápido

```bash
go install github.com/roshankumar0036singh/gographctx/cmd/gographctx@latest

gographctx index ./your-repo
gographctx stats ./your-repo
gographctx query get_impact pkg::Service::Handle ./your-repo
```

```bash
# Claude Code MCP
claude mcp add gographctx -- gographctx watch /absolute/path/to/your/repo
```

---

## Benchmarks Medidos

Executado com [`cmd/ggctx-bench`](cmd/ggctx-bench/main.go) (`go1.25.1`, `windows/amd64`, 16 vCPUs):

| Métrica | Medido | Meta SLA |
|---|---|---|
| **Repositório Python estilo Flask** (83 `.py`) | **533.39 ms** / **51.02 MB RSS** | `< 1s` / `< 80 MB` |
| **Monorepo Go** (1.000 `.go`) | **1,401.70 ms** / **116.92 MB RSS** | `< 3s` / `< 150 MB` |
| **Arestas de alta confiança (Flask)** | **100.0%** | `>= 85%` |
| **Atualização incremental (p50)** | **22.78 ms** (corpo) / **22.77 ms** (export) | `< 50ms` / `< 100ms` |
| **Latência MCP (p95) / Binário** | **3.80 ms** / **15.79 MB** | `< 10ms` / `< 30 MB` |

Veja [docs/benchmarks.md](docs/benchmarks.md) para a comparação detalhada contra **CGC**, **SCIP**, **Universal Ctags**, **ast-grep**, **Serena** e **ripgrep**.

## Documentação

- [Referência de Ferramentas MCP](docs/mcp-tools.md) · [CLI e Configuração](docs/cli-and-config.md) · [Arquitetura](docs/architecture.md) · [Linguagens Suportadas](docs/languages.md) · [Benchmarks](docs/benchmarks.md) · [Guia de Contribuição](CONTRIBUTING.md)

Se o GoGraphCtx ajudar seu fluxo de trabalho com IA, considere deixar uma **Star**!
