package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/roshankumar0036singh/gographctx/internal/config"
	"github.com/roshankumar0036singh/gographctx/internal/graph"
	"github.com/roshankumar0036singh/gographctx/internal/mcp"
	"github.com/roshankumar0036singh/gographctx/internal/pipeline"
	"github.com/roshankumar0036singh/gographctx/internal/storage"
	"github.com/roshankumar0036singh/gographctx/internal/watcher"
)

// Version is injected at build time via -ldflags; defaults to dev version.
var Version = "0.1.0"

type commonFlags struct {
	configPath string
	verbose    bool
	force      bool
	workers    int
	format     string
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, in io.Reader, out io.Writer, errOut io.Writer) int {
	if len(args) == 0 {
		printUsage(errOut)
		return 2
	}

	// Support top-level `--help` / `--version` shortcuts cleanly.
	switch args[0] {
	case "-h", "--help", "help":
		printUsage(out)
		return 0
	case "--version", "-v":
		return runVersion(args[1:], out, errOut)
	}

	subcmd := args[0]
	rest := args[1:]

	switch subcmd {
	case "version":
		return runVersion(rest, out, errOut)
	case "index":
		return runIndex(ctx, rest, out, errOut)
	case "serve":
		return runServe(ctx, rest, in, out, errOut)
	case "watch":
		return runWatch(ctx, rest, in, out, errOut)
	case "query":
		return runQuery(ctx, rest, out, errOut)
	case "stats":
		return runStats(rest, out, errOut)
	default:
		_, _ = fmt.Fprintf(errOut, "unknown command %q\n\n", subcmd)
		printUsage(errOut)
		return 2
	}
}

func registerCommonFlags(fs *flag.FlagSet, cf *commonFlags, defaultFormat string) {
	cf.format = defaultFormat
	fs.StringVar(&cf.configPath, "config", "", "path to .ggctxrc config file")
	fs.BoolVar(&cf.verbose, "verbose", false, "enable verbose logging to stderr")
	fs.BoolVar(&cf.force, "force", false, "force full re-index even if unchanged")
	fs.IntVar(&cf.workers, "workers", 0, "override parser/resolver worker count")
	fs.StringVar(&cf.format, "format", defaultFormat, "output format: json or text")
}

func validateFormat(format string) (string, error) {
	norm := strings.ToLower(strings.TrimSpace(format))
	if norm == "" {
		return "text", nil
	}
	if norm != "json" && norm != "text" {
		return "", fmt.Errorf("invalid --format %q (expected json or text)", format)
	}
	return norm, nil
}

func loadRepoConfig(repoPath string, cf commonFlags) (config.Config, error) {
	var cfg config.Config
	var err error
	if strings.TrimSpace(cf.configPath) != "" {
		cfg, err = config.LoadFromFile(cf.configPath)
	} else {
		cfg, err = config.Load(repoPath)
	}
	if err != nil {
		return config.Config{}, err
	}
	if cf.workers > 0 {
		cfg.Workers = cf.workers
	}
	if err := cfg.Validate(); err != nil {
		return config.Config{}, err
	}
	return cfg, nil
}

func resolveStoreDir(repoRoot string, cfg config.Config) string {
	dataDir := strings.TrimSpace(cfg.DataDir)
	if dataDir == "" {
		dataDir = config.DefaultDataDirName
	}
	if filepath.IsAbs(dataDir) {
		return dataDir
	}
	return filepath.Join(repoRoot, dataDir)
}

func loadOrIndexGraph(ctx context.Context, repoRoot string, cfg config.Config, force bool, verbose bool, errOut io.Writer) (*graph.Graph, error) {
	storeDir := resolveStoreDir(repoRoot, cfg)
	snapPath := filepath.Join(storeDir, storage.SnapshotFileName)
	if !force {
		if _, err := os.Stat(snapPath); err == nil {
			store, g, lstats, loadErr := storage.LoadStore(
				storeDir,
				storage.WriteOptions{Compress: cfg.CompressSnapshots, Sync: cfg.SyncWrites},
				storage.WALOptions{Sync: cfg.SyncWrites, CompactThresholdBytes: cfg.WALCompactThresholdBytes},
				storage.LoadOptions{AllowTrailingPartialWAL: true},
			)
			if loadErr == nil && lstats.HasSnapshot {
				_ = store.Close()
				if verbose {
					_, _ = fmt.Fprintf(errOut, "loaded index (%d nodes, %d edges, wal=%d frames) in %.1fms\n",
						g.NodeCount(), g.EdgeCount(), lstats.WALFramesApplied, lstats.DurationMs)
				}
				return g, nil
			}
			if store != nil {
				_ = store.Close()
			}
		}
	}

	if verbose {
		_, _ = fmt.Fprintf(errOut, "indexing %s...\n", repoRoot)
	}
	res, err := pipeline.IndexRepository(ctx, repoRoot, cfg, pipeline.IndexOptions{Force: force, StoreDir: storeDir})
	if err != nil {
		return nil, err
	}
	if verbose {
		_, _ = fmt.Fprint(errOut, res.FormatSummary())
	}
	return res.Graph, nil
}

func runVersion(args []string, out io.Writer, errOut io.Writer) int {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(errOut)
	var format string
	fs.StringVar(&format, "format", "text", "output format: json or text")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	normFmt, err := validateFormat(format)
	if err != nil {
		_, _ = fmt.Fprintln(errOut, err)
		return 2
	}
	if normFmt == "json" {
		_ = json.NewEncoder(out).Encode(map[string]any{
			"version":       Version,
			"index_version": storage.FormatVersion,
		})
		return 0
	}
	_, _ = fmt.Fprintf(out, "gographctx v%s (index format v%d)\n", Version, storage.FormatVersion)
	return 0
}

func runIndex(ctx context.Context, args []string, out io.Writer, errOut io.Writer) int {
	fs := flag.NewFlagSet("index", flag.ContinueOnError)
	fs.SetOutput(errOut)
	var cf commonFlags
	registerCommonFlags(fs, &cf, "text")
	if err := fs.Parse(reorderFlagsBeforeArgs(args, map[string]bool{"format": true, "config": true, "workers": true})); err != nil {
		return 2
	}
	normFmt, err := validateFormat(cf.format)
	if err != nil {
		_, _ = fmt.Fprintln(errOut, err)
		return 2
	}

	target := "."
	if fs.NArg() > 0 {
		target = fs.Arg(0)
	}
	absRoot, err := filepath.Abs(target)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "resolve path: %v\n", err)
		return 1
	}
	cfg, err := loadRepoConfig(absRoot, cf)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "load config: %v\n", err)
		return 1
	}

	res, err := pipeline.IndexRepository(ctx, absRoot, cfg, pipeline.IndexOptions{Force: cf.force})
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "index failed: %v\n", err)
		return 1
	}
	if cf.verbose && len(res.Errors) > 0 {
		for _, fe := range res.Errors {
			_, _ = fmt.Fprintf(errOut, "warn: %v\n", fe)
		}
	}

	if normFmt == "json" {
		errStrings := make([]string, len(res.Errors))
		for i, e := range res.Errors {
			errStrings[i] = e.Error()
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{
			"repo_root":     res.RepoRoot,
			"snapshot_path": res.SnapshotPath,
			"stats":         res.Stats,
			"timings_ms": map[string]float64{
				"discover": float64(res.Timings.Discover.Microseconds()) / 1000.0,
				"parse":    float64(res.Timings.Parse.Microseconds()) / 1000.0,
				"build":    float64(res.Timings.Build.Microseconds()) / 1000.0,
				"resolve":  float64(res.Timings.Resolve.Microseconds()) / 1000.0,
				"finalize": float64(res.Timings.Finalize.Microseconds()) / 1000.0,
				"persist":  float64(res.Timings.Persist.Microseconds()) / 1000.0,
				"total":    float64(res.Timings.Total.Microseconds()) / 1000.0,
			},
			"errors": errStrings,
		})
		return 0
	}

	_, _ = fmt.Fprint(out, res.FormatSummary())
	return 0
}

func runServe(ctx context.Context, args []string, in io.Reader, out io.Writer, errOut io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(errOut)
	var cf commonFlags
	registerCommonFlags(fs, &cf, "json")
	if err := fs.Parse(reorderFlagsBeforeArgs(args, map[string]bool{"format": true, "config": true, "workers": true})); err != nil {
		return 2
	}

	target := "."
	if fs.NArg() > 0 {
		target = fs.Arg(0)
	}
	absRoot, err := filepath.Abs(target)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "resolve path: %v\n", err)
		return 1
	}
	cfg, err := loadRepoConfig(absRoot, cf)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "load config: %v\n", err)
		return 1
	}

	g, err := loadOrIndexGraph(ctx, absRoot, cfg, cf.force, cf.verbose, errOut)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "serve prepare index: %v\n", err)
		return 1
	}

	handler := mcp.NewHandler(g, mcp.FormatterConfig{
		RepoRoot:           absRoot,
		DefaultTokenBudget: cfg.TokenBudget,
	})
	srv := mcp.NewServer(handler, in, out, errOut)
	if err := srv.Serve(ctx); err != nil {
		_, _ = fmt.Fprintf(errOut, "mcp server error: %v\n", err)
		return 1
	}
	return 0
}

func runWatch(ctx context.Context, args []string, in io.Reader, out io.Writer, errOut io.Writer) int {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.SetOutput(errOut)
	var cf commonFlags
	var noMCP bool
	registerCommonFlags(fs, &cf, "text")
	fs.BoolVar(&noMCP, "no-mcp", false, "run file watcher only without stdio MCP server")
	if err := fs.Parse(reorderFlagsBeforeArgs(args, map[string]bool{"format": true, "config": true, "workers": true})); err != nil {
		return 2
	}

	target := "."
	if fs.NArg() > 0 {
		target = fs.Arg(0)
	}
	absRoot, err := filepath.Abs(target)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "resolve path: %v\n", err)
		return 1
	}
	cfg, err := loadRepoConfig(absRoot, cf)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "load config: %v\n", err)
		return 1
	}

	eng, err := watcher.NewIncrementalEngine(ctx, absRoot, cfg, pipeline.IndexOptions{Force: cf.force})
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "watch init failed: %v\n", err)
		return 1
	}
	defer func() { _ = eng.Close() }()

	watchOut := errOut
	if noMCP {
		watchOut = out
	}

	fw, err := watcher.NewFileWatcher(eng, cfg, watcher.WatchOptions{
		Debounce: cfg.DebounceDuration(),
		Writer:   watchOut,
		OnError: func(werr error) {
			_, _ = fmt.Fprintf(errOut, "watch error: %v\n", werr)
		},
	})
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "start watcher: %v\n", err)
		return 1
	}
	defer func() { _ = fw.Close() }()

	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	watchErrCh := make(chan error, 1)
	go func() {
		watchErrCh <- fw.Start(watchCtx)
	}()

	if cf.verbose {
		_, _ = fmt.Fprintf(errOut, "watching %s (%d directories)...\n", absRoot, fw.WatchedDirCount())
	}

	if !noMCP {
		handler := mcp.NewHandler(eng.Graph(), mcp.FormatterConfig{
			RepoRoot:           absRoot,
			DefaultTokenBudget: cfg.TokenBudget,
		})
		srv := mcp.NewServer(handler, in, out, errOut)
		if err := srv.Serve(watchCtx); err != nil {
			_, _ = fmt.Fprintf(errOut, "mcp server error: %v\n", err)
			cancel()
			<-watchErrCh
			return 1
		}
		cancel()
		<-watchErrCh
		return 0
	}

	<-watchCtx.Done()
	if err := <-watchErrCh; err != nil {
		_, _ = fmt.Fprintf(errOut, "watcher stopped with error: %v\n", err)
		return 1
	}
	return 0
}

func runQuery(ctx context.Context, args []string, out io.Writer, errOut io.Writer) int {
	fs := flag.NewFlagSet("query", flag.ContinueOnError)
	fs.SetOutput(errOut)
	var cf commonFlags
	var repoPath, rawArgsJSON, symbolArg, fileArg string
	var limitArg, depthArg int
	registerCommonFlags(fs, &cf, "json")
	fs.StringVar(&repoPath, "path", ".", "repository root path")
	fs.StringVar(&rawArgsJSON, "args", "", "JSON tool arguments object")
	fs.StringVar(&symbolArg, "symbol", "", "symbol name or QName shorthand")
	fs.StringVar(&fileArg, "file", "", "repo-relative file path shorthand")
	fs.IntVar(&limitArg, "limit", 0, "max results")
	fs.IntVar(&depthArg, "depth", 0, "max traversal depth (get_impact)")
	if err := fs.Parse(reorderFlagsBeforeArgs(args, map[string]bool{
		"format": true, "config": true, "workers": true, "path": true,
		"args": true, "symbol": true, "file": true, "limit": true, "depth": true,
	})); err != nil {
		return 2
	}
	normFmt, err := validateFormat(cf.format)
	if err != nil {
		_, _ = fmt.Fprintln(errOut, err)
		return 2
	}
	if fs.NArg() == 0 {
		_, _ = fmt.Fprintln(errOut, "query requires <tool> name (e.g. find_symbol, get_callers, repo_overview)")
		return 2
	}

	toolName := fs.Arg(0)
	if fs.NArg() > 1 && symbolArg == "" && rawArgsJSON == "" {
		arg1 := fs.Arg(1)
		switch toolName {
		case "file_outline":
			fileArg = arg1
		case "repo_overview":
			if repoPath == "." {
				repoPath = arg1
			}
		default:
			symbolArg = arg1
		}
	}

	absRoot, err := filepath.Abs(repoPath)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "resolve path: %v\n", err)
		return 1
	}
	cfg, err := loadRepoConfig(absRoot, cf)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "load config: %v\n", err)
		return 1
	}

	g, err := loadOrIndexGraph(ctx, absRoot, cfg, cf.force, cf.verbose, errOut)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "query load index: %v\n", err)
		return 1
	}

	payloadJSON := json.RawMessage(rawArgsJSON)
	if strings.TrimSpace(rawArgsJSON) == "" {
		argMap := make(map[string]any)
		switch toolName {
		case "find_symbol":
			if symbolArg != "" {
				argMap["name"] = symbolArg
			}
			if fileArg != "" {
				argMap["file"] = fileArg
			}
		case "search_code":
			if symbolArg != "" {
				argMap["name_pattern"] = symbolArg
			}
			if fileArg != "" {
				argMap["file_pattern"] = fileArg
			}
		case "file_outline":
			if fileArg != "" {
				argMap["file"] = fileArg
			}
		case "get_callers", "get_callees", "get_impact":
			if symbolArg != "" {
				argMap["symbol"] = symbolArg
			}
			if fileArg != "" {
				argMap["file"] = fileArg
			}
			if depthArg > 0 && toolName == "get_impact" {
				argMap["max_depth"] = depthArg
			}
		}
		if limitArg > 0 {
			argMap["limit"] = limitArg
		}
		encoded, _ := json.Marshal(argMap)
		payloadJSON = encoded
	}

	handler := mcp.NewHandler(g, mcp.FormatterConfig{
		RepoRoot:           absRoot,
		DefaultTokenBudget: cfg.TokenBudget,
	})
	respBytes, err := handler.HandleCall(ctx, toolName, payloadJSON)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "query %q failed: %v\n", toolName, err)
		if errors.Is(err, mcp.ErrInvalidArgs) || errors.Is(err, mcp.ErrUnknownTool) {
			return 2
		}
		return 1
	}

	if normFmt == "json" {
		var v any
		if json.Unmarshal(respBytes, &v) == nil {
			enc := json.NewEncoder(out)
			enc.SetIndent("", "  ")
			_ = enc.Encode(v)
			return 0
		}
		_, _ = fmt.Fprintf(out, "%s\n", respBytes)
		return 0
	}

	var parsed mcp.FormattedResponse
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		_, _ = fmt.Fprintf(out, "%s\n", respBytes)
		return 0
	}
	_, _ = fmt.Fprintf(out, "[%s] %s (returned %d/%d, confidence=%s, ~%d tokens)\n",
		parsed.Tool, parsed.Summary, parsed.Returned, parsed.Total,
		parsed.ConfidenceSummary.OverallLabel, parsed.Metadata.EstimatedTokens)
	prettyData, _ := json.MarshalIndent(parsed.Data, "", "  ")
	_, _ = fmt.Fprintf(out, "%s\n", prettyData)
	return 0
}

func runStats(args []string, out io.Writer, errOut io.Writer) int {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	fs.SetOutput(errOut)
	var cf commonFlags
	registerCommonFlags(fs, &cf, "text")
	if err := fs.Parse(reorderFlagsBeforeArgs(args, map[string]bool{"format": true, "config": true, "workers": true})); err != nil {
		return 2
	}
	normFmt, err := validateFormat(cf.format)
	if err != nil {
		_, _ = fmt.Fprintln(errOut, err)
		return 2
	}

	target := "."
	if fs.NArg() > 0 {
		target = fs.Arg(0)
	}
	absRoot, err := filepath.Abs(target)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "resolve path: %v\n", err)
		return 1
	}
	cfg, err := loadRepoConfig(absRoot, cf)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "load config: %v\n", err)
		return 1
	}

	storeDir := resolveStoreDir(absRoot, cfg)
	snapPath := filepath.Join(storeDir, storage.SnapshotFileName)
	if _, statErr := os.Stat(snapPath); statErr != nil {
		_, _ = fmt.Fprintf(errOut, "no index found at %s (run `gographctx index %s` first)\n", snapPath, target)
		return 1
	}

	start := time.Now()
	store, g, lstats, err := storage.LoadStore(
		storeDir,
		storage.WriteOptions{Compress: cfg.CompressSnapshots, Sync: cfg.SyncWrites},
		storage.WALOptions{Sync: cfg.SyncWrites, CompactThresholdBytes: cfg.WALCompactThresholdBytes},
		storage.LoadOptions{AllowTrailingPartialWAL: true},
	)
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "load index failed: %v\n", err)
		return 1
	}
	_ = store.Close()

	repoStats := g.Stats()
	if normFmt == "json" {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{
			"repo_root":     absRoot,
			"snapshot_path": snapPath,
			"load_ms":       float64(time.Since(start).Microseconds()) / 1000.0,
			"wal_frames":    lstats.WALFramesApplied,
			"snapshot_hdr":  lstats.SnapshotHeader,
			"graph_stats":   repoStats,
		})
		return 0
	}

	_, _ = fmt.Fprintf(out, "GoGraphCtx Index Stats (%s)\n", absRoot)
	_, _ = fmt.Fprintf(out, "  Snapshot : %s (load: %.1fms, wal frames: %d)\n",
		snapPath, float64(time.Since(start).Microseconds())/1000.0, lstats.WALFramesApplied)
	_, _ = fmt.Fprintf(out, "  Files    : %d\n", repoStats.FileCount)
	_, _ = fmt.Fprintf(out, "  Nodes    : %d\n", repoStats.NodeCount)
	_, _ = fmt.Fprintf(out, "  Edges    : %d (coverage: %.1f%%)\n", repoStats.EdgeCount, repoStats.ResolutionCoverage*100)
	_, _ = fmt.Fprintf(out, "  CallSites: %d\n", repoStats.CallSiteCount)
	if len(repoStats.NodesByLanguage) > 0 {
		langs := make([]string, 0, len(repoStats.NodesByLanguage))
		for l := range repoStats.NodesByLanguage {
			langs = append(langs, l)
		}
		slices.Sort(langs)
		var parts []string
		for _, l := range langs {
			parts = append(parts, fmt.Sprintf("%s=%d", l, repoStats.NodesByLanguage[l]))
		}
		_, _ = fmt.Fprintf(out, "  Languages: %s\n", strings.Join(parts, ", "))
	}
	return 0
}

// reorderFlagsBeforeArgs moves flags placed after positional arguments (e.g.
// `index ./repo --force --format json`) ahead of positional arguments so Go's
// stdlib flag.FlagSet parses both positions naturally.
func reorderFlagsBeforeArgs(args []string, valueFlags map[string]bool) []string {
	var flags, positionals []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positionals = append(positionals, args[i:]...)
			break
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if !strings.Contains(name, "=") && valueFlags[name] && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		positionals = append(positionals, a)
	}
	return append(flags, positionals...)
}

func printUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `GoGraphCtx — fast, confidence-aware code graph indexer & MCP server

Usage:
  gographctx <command> [flags] [args]

Commands:
  index <path>                 Index a repository to .ggctx/index.ggctx
  serve [path]                 Start MCP JSON-RPC server over stdio
  watch [path]                 Watch repository changes and serve/update live graph
  query <tool> [args]          Run an MCP tool query against the indexed graph
  stats <path>                 Display index statistics without tree-sitter overhead
  version                      Print gographctx and index format version

Common Flags:
  --config <file>              Path to .ggctxrc config file
  --force                      Force full re-index
  --workers <n>                Worker concurrency override
  --format <json|text>         Output format
  --verbose                    Verbose logging to stderr
`)
}
