<div align="center">

<p>
  <a href="README.md">English</a> ·
  <a href="README.zh-CN.md">简体中文</a> ·
  <a href="README.ja.md">日本語</a> ·
  <a href="README.ko.md">한국어</a> ·
  <a href="README.es.md">Español</a> ·
  <a href="README.pt-BR.md">Português (BR)</a> ·
  <a href="README.de.md">Deutsch</a> ·
  <strong>Français</strong>
</p>

<img src="docs/assets/hero-banner.png" alt="GoGraphCtx — Indexeur de graphe de code en binaire unique et serveur MCP" width="100%"/>

<h1>GoGraphCtx</h1>

<p>
  <strong>Indexeur de Graphe de Code en Binaire Unique Sans Dépendance &amp; Serveur MCP avec Indices de Confiance</strong>
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
  <em>Offrez à Claude Code, Cursor, Windsurf, VS Code Copilot et Cline un graphe d'appels fiable, une analyse d'impact et l'architecture du dépôt en <strong>533ms</strong>—sans Neo4j, Docker ni Python.</em>
</p>

</div>

---

## Démo Interactive

<p align="center">
  <img src="docs/assets/terminal-demo.svg" alt="Démo terminal GoGraphCtx" width="100%"/>
</p>

<details>
<summary><strong>Voir l'Infographie Comparative et d'Architecture</strong></summary>
<p align="center">
  <img src="docs/assets/demo-preview.png" alt="Infographie comparative GoGraphCtx" width="100%"/>
</p>
</details>

---

## Pourquoi Choisir GoGraphCtx ?

1. **Supprimez le coût en tokens du `grep` :** Retourne des réponses structurées sous budget strict (`4000` tokens par défaut) au lieu de parcourir des commentaires et homonymes.
2. **Modèle de Confiance à 8 Niveaux :** Sépare strictement les appels certifiés (`CALLS`) des candidats probables (`LIKELY_CALLS` / `CallSite`).
3. **Binaire Statique de 15,8 Mo Sans Base Externe :** Parseurs `gotreesitter` compilés en Go pur (`CGO_ENABLED=0`) pour **Go, Python, TypeScript/TSX, JavaScript/JSX, Rust et Java**.
4. **Surveillance Incrémentale en Direct (`~22,8ms`) :** `fsnotify` ré-indexe uniquement les fichiers modifiés et leurs dépendants pendant le service MCP.

---

## Démarrage Rapide

```bash
go install github.com/roshankumar0036singh/gographctx/cmd/gographctx@latest

gographctx index ./your-repo
gographctx stats ./your-repo
gographctx query get_impact pkg::Service::Handle ./your-repo
```

```bash
# Ajouter à Claude Code
claude mcp add gographctx -- gographctx watch /absolute/path/to/your/repo
```

---

## Benchmarks Mesurés & Comparatif Multi-Outils

Mesuré via [`cmd/ggctx-bench`](cmd/ggctx-bench/main.go) (`go1.25.1`, `windows/amd64`, 16 vCPUs) :

| Métrique | Valeur Mesurée | Objectif SLA |
|---|---|---|
| **Dépôt Python type Flask** (83 `.py`) | **533.39 ms** / **51.02 MB RSS** | `< 1s` / `< 80 MB` |
| **Monorepo Go** (1 000 `.go`) | **1,401.70 ms** / **116.92 MB RSS** | `< 3s` / `< 150 MB` |
| **Taux d'arêtes haute confiance (Flask)** | **100.0%** | `>= 85%` |
| **Mise à jour incrémentale (p50)** | **22.78 ms** (corps) / **22.77 ms** (export) | `< 50ms` / `< 100ms` |
| **Latence MCP (p95) / Taille du binaire** | **3.80 ms** / **15.79 MB** | `< 10ms` / `< 30 MB` |

Consultez [docs/benchmarks.md](docs/benchmarks.md) pour le comparatif détaillé face à **CGC**, **SCIP**, **Universal Ctags**, **ast-grep**, **Serena** et **ripgrep** (incluant **98,4 % de précision sur Flask** contre **69,2 % pour CGC**, la transparence `unresolved_hints` et `~492 ms` contre `92,7 s` d'indexation).

## Documentation

- [Référence des outils MCP](docs/mcp-tools.md) · [CLI & Configuration](docs/cli-and-config.md) · [Architecture](docs/architecture.md) · [Langages supportés](docs/languages.md) · [Benchmarks](docs/benchmarks.md) · [Contribuer](CONTRIBUTING.md)

Si GoGraphCtx vous fait gagner du temps et des tokens, n'hésitez pas à ajouter une **Star** !
