<div align="center">

<p>
  <a href="README.md">English</a> ·
  <a href="README.zh-CN.md">简体中文</a> ·
  <strong>日本語</strong> ·
  <a href="README.ko.md">한국어</a> ·
  <a href="README.es.md">Español</a> ·
  <a href="README.pt-BR.md">Português (BR)</a> ·
  <a href="README.de.md">Deutsch</a> ·
  <a href="README.fr.md">Français</a>
</p>

<img src="docs/assets/hero-banner.png" alt="GoGraphCtx — 単一バイナリのコードグラフインデクサ & MCP サーバー" width="100%"/>

<h1>GoGraphCtx</h1>

<p>
  <strong>AI コーディングエージェント向けの単一バイナリ・ゼロ依存コードグラフインデクサ &amp; 信頼度付き MCP サーバー</strong>
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
  <em>Claude Code、Cursor、Windsurf、VS Code Copilot、Cline 向けに正確なコールグラフ、影響範囲分析（Blast Radius）、リポジトリ構造を <strong>533ms</strong> で構築。<strong>Neo4j・Docker・Python 依存一切不要</strong>。</em>
</p>

</div>

---

## デモプレビュー

<p align="center">
  <img src="docs/assets/terminal-demo.svg" alt="GoGraphCtx ターミナルデモ" width="100%"/>
</p>

<details>
<summary><strong>アーキテクチャ・比較インフォグラフィックを表示</strong></summary>
<p align="center">
  <img src="docs/assets/demo-preview.png" alt="GoGraphCtx 比較インフォグラフィック" width="100%"/>
</p>
</details>

---

## なぜ GoGraphCtx なのか？

1. **`grep` / `glob` によるコンテキスト浪費を解消**：コメントやインポート、同名シンボルを大量に読み込む代わりに、トークン予算（デフォルト `4000`）内に収めた構造化レスポンスを返します。
2. **曖昧な推測を事実として扱わない**：8 段階の信頼度ティアにより、確実な `CALLS` エッジと不確実な `LIKELY_CALLS` / `CallSite` 候補を厳密に分離します。
3. **15.8 MB 単一静的バイナリ**：組み込み `gotreesitter`（`CGO_ENABLED=0`）で **Go、Python、TypeScript/TSX、JavaScript/JSX、Rust、Java** を解析。外部 DB 不要です。
4. **高速な差分更新（`~22.8ms`）**：`fsnotify` により変更ファイルと逆依存ファイルのみを即座に再解決しながら MCP サーバーを提供します。

---

## クイックスタート

```bash
go install github.com/roshankumar0036singh/gographctx/cmd/gographctx@latest

gographctx index ./your-repo
gographctx stats ./your-repo
gographctx query get_impact pkg::Service::Handle ./your-repo
```

```bash
# Claude Code への登録
claude mcp add gographctx -- gographctx watch /absolute/path/to/your/repo
```

---

## ベンチマーク実測値と比較

[`cmd/ggctx-bench`](cmd/ggctx-bench/main.go)（`go1.25.1`、`windows/amd64`、16 vCPU）での実測結果：

| 指標 | 実測値 | 目標 SLA |
|---|---|---|
| **Flask 規模 Python**（83 `.py` ファイル） | **533.39 ms** / **51.02 MB RSS** | `< 1s` / `< 80 MB` |
| **Go モノレポ**（1,000 `.go` ファイル） | **1,401.70 ms** / **116.92 MB RSS** | `< 3s` / `< 150 MB` |
| **Flask 高信頼ティア比率** | **100.0%** | `>= 85%` |
| **インクリメンタル更新（p50）** | **22.78 ms**（本体）/ **22.77 ms**（エクスポート） | `< 50ms` / `< 100ms` |
| **MCP クエリ遅延（p95）/ バイナリサイズ** | **3.80 ms** / **15.79 MB** | `< 10ms` / `< 30 MB` |

詳細比較（CGC、SCIP、Universal Ctags、ast-grep、Serena、ripgrep との比較、**Flask 精度 98.4% vs CGC 69.2%**、`unresolved_hints` による未解決呼び出しの明示、**492 ms vs 92.7 s** のインデックス速度実測）は [docs/benchmarks.md](docs/benchmarks.md) をご覧ください。

## ドキュメント

- [MCP ツール仕様](docs/mcp-tools.md) · [CLI と設定](docs/cli-and-config.md) · [アーキテクチャ](docs/architecture.md) · [対応言語](docs/languages.md) · [ベンチマーク](docs/benchmarks.md) · [コントリビュートガイド](CONTRIBUTING.md)

役に立ったらぜひ **Star** をお願いします！
