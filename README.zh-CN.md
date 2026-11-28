<div align="center">

<p>
  <a href="README.md">English</a> ·
  <strong>简体中文</strong> ·
  <a href="README.ja.md">日本語</a> ·
  <a href="README.ko.md">한국어</a> ·
  <a href="README.es.md">Español</a> ·
  <a href="README.pt-BR.md">Português (BR)</a> ·
  <a href="README.de.md">Deutsch</a> ·
  <a href="README.fr.md">Français</a>
</p>

<img src="docs/assets/hero-banner.png" alt="GoGraphCtx — 单二进制代码图谱索引器与置信度感知 MCP 服务器" width="100%"/>

<h1>GoGraphCtx</h1>

<p>
  <strong>面向 AI 编程智能体的单二进制、零外部依赖代码图谱索引器与高置信度 MCP 服务器</strong>
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
  <em>在 <strong>533ms</strong> 内为 Claude Code、Cursor、Windsurf、VS Code Copilot 和 Cline 构建高精度调用图、影响范围分析与代码库结构，<strong>无需 Neo4j、Docker 或 Python 依赖</strong>。</em>
</p>

</div>

---

## 交互式终端演示

<p align="center">
  <img src="docs/assets/terminal-demo.svg" alt="GoGraphCtx 终端演示" width="100%"/>
</p>

<details>
<summary><strong>查看架构与性能对比图解</strong></summary>
<p align="center">
  <img src="docs/assets/demo-preview.png" alt="GoGraphCtx 对比传统工具图解" width="100%"/>
</p>
</details>

---

## 为什么选择 GoGraphCtx？

1. **消除 `grep`/`glob` 的“上下文税”**：无需代理反复检索注释、导入行或同名函数，直接返回严格控制在 `4000` Token 预算内的结构化调用关系与影响链。
2. **拒绝将猜测当作事实**：通过 8 级置信度模型明确区分高置信度 `CALLS` 边与不确定候选 `LIKELY_CALLS` / `CallSite` 证据，避免误导模型重构。
3. **15.8 MB 单文件零依赖**：内置纯 Go `gotreesitter` 解析器（`CGO_ENABLED=0`），支持 **Go、Python、TypeScript/TSX、JavaScript/JSX、Rust 和 Java**，无需外部图数据库或虚拟环境。
4. **毫秒级增量监听（`~22.8ms`）**：基于 `fsnotify` 与反向依赖追踪，编辑代码后仅重新索引变更文件及其受影响依赖。

---

## 30 秒快速上手

```bash
# 安装单个静态二进制文件
go install github.com/roshankumar0036singh/gographctx/cmd/gographctx@latest

# 索引代码库并查询调用影响面
gographctx index ./your-repo
gographctx stats ./your-repo
gographctx query get_impact pkg::Service::Handle ./your-repo
```

### 在 Claude Code / Cursor 中接入 MCP

```bash
# Claude Code
claude mcp add gographctx -- gographctx watch /absolute/path/to/your/repo
```

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

---

## 实测基准与多工具对比

通过 [`cmd/ggctx-bench`](cmd/ggctx-bench/main.go) 在 `go1.25.1`（`windows/amd64`、16 vCPU）上实测：

| 基准指标 | 实测结果 | 目标上限 |
|---|---|---|
| **Flask 规模 Python 仓库**（83 个 `.py` 文件，705 节点，2,208 边） | **533.39 ms** / **51.02 MB RSS** | `< 1s` / `< 80 MB` |
| **Go 单体仓**（1,000 个 `.go` 文件，6,002 节点，8,000 边） | **1,401.70 ms** / **116.92 MB RSS** | `< 3s` / `< 150 MB` |
| **Flask 强置信度调用边比例** | **100.0%** | `>= 85%` |
| **增量文件更新（p50）** | **22.78 ms**（函数体）/ **22.77 ms**（导出签名） | `< 50ms` / `< 100ms` |
| **MCP 工具查询延迟（p95）** | **3.80 ms** | `< 10 ms` |
| **CLI 冷启动（p50）/ 静态二进制体积** | **22.80 ms** / **15.79 MB** | `< 50ms` / `< 30 MB` |

| 工具 | 架构与部署 | 调用图 | 影响范围 BFS | 置信度分级 | 与 GoGraphCtx 对比说明 |
|---|---|---|---|---|---|
| **GoGraphCtx** | **15.8 MB 单二进制**，内置快照+WAL | 支持 | 支持 | **支持（8 级）** | 零外部数据库，亚秒级索引与 Token 预算控制 |
| **CodeGraphContext (CGC)** | Python + 约 90 个依赖 + Neo4j/FalkorDB | 支持 | 支持 | 部分（猜测常直接入边） | 依赖图数据库与 Python 运行时 |
| **SCIP**（`scip-python` 等） | 需完整编译器环境与项目依赖 | 支持 | 需外部脚本 | 编译器精标准 | 精度基准参照，但依赖环境配置与全量依赖安装 |
| **Universal Ctags** | 轻量符号标签 CLI | 不支持 | 不支持 | 不支持 | 仅做符号位置标签，无调用链与类型推断 |
| **ast-grep (`sg`)** | AST 结构模式搜索 CLI | 不支持 | 不支持 | 语法精确匹配 | 擅长语法模式搜索，无持久化跨文件调用图 |
| **Serena** | Python + 常驻 LSP 语言服务器 | 支持 | 有限 | 类型检查支持 | 启动与 LSP 预热更重，暴露工具数量较多 |
| **ripgrep (`rg`)** | 文本正则高速扫描 | 不支持 | 不支持 | 纯文本 | 速度快但注释/导入/同名符号噪音较高 |

---

## 完整文档索引

- [MCP 工具参考](docs/mcp-tools.md) · [CLI 与配置指南](docs/cli-and-config.md) · [系统架构与存储](docs/architecture.md) · [语言支持矩阵](docs/languages.md) · [基准测试与 SCIP 验证](docs/benchmarks.md) · [贡献指南](CONTRIBUTING.md)

如果 GoGraphCtx 对您的 AI 编程工作流有帮助，欢迎点亮 **Star** 支持我们！
