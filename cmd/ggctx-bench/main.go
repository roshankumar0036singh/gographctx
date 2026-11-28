package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/roshankumar0036singh/gographctx/internal/bench"
	"github.com/roshankumar0036singh/gographctx/internal/config"
	"github.com/roshankumar0036singh/gographctx/internal/pipeline"
)

func main() {
	os.Exit(runBenchCLI(context.Background(), os.Args[1:]))
}

func runBenchCLI(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("ggctx-bench", flag.ContinueOnError)
	corpus := fs.String("corpus", "flask-synth", "corpus to benchmark: flask-synth, go-1k, mixed, custom")
	repoPath := fs.String("repo", "", "custom repository path when -corpus=custom (or existing Flask checkout)")
	scipFile := fs.String("scip-file", "", "optional pre-generated .scip file for accuracy evaluation")
	cliBin := fs.String("cli-bin", "auto", "path to gographctx CLI binary ('auto' builds a stripped binary)")
	toolsFlag := fs.String("tools", "all", "comma-separated comparators: all or subset of gographctx,cgc,scip,ctags,ast-grep,serena,ripgrep")
	outJSON := fs.String("out-json", "", "path to write JSON benchmark report")
	outMD := fs.String("out-md", "", "path to write Markdown benchmark report")
	baselineJSON := fs.String("baseline-json", "", "optional previous FullBenchReport JSON for delta comparison")
	maxRegression := fs.Float64("max-regression", 0.20, "maximum allowed fractional regression vs baseline (e.g. 0.20 = 20%)")
	assertSLA := fs.Bool("assert-sla", false, "exit non-zero if GoGraphCtx SLA targets or accuracy/regression gates fail")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	workDir, err := os.MkdirTemp("", "ggctx-bench-work-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create temp work dir: %v\n", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(workDir) }()

	resolvedCLI := *cliBin
	if resolvedCLI == "auto" {
		built, berr := buildStrippedBinary(ctx, workDir)
		if berr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not auto-build gographctx binary: %v\n", berr)
			resolvedCLI = ""
		} else {
			resolvedCLI = built
		}
	}

	// Run GoGraphCtx core SLA benchmark suite.
	flaskOverride := ""
	if *corpus == "custom" && *repoPath != "" {
		flaskOverride = *repoPath
	}
	perfRes, err := bench.RunPerfSuite(ctx, workDir, resolvedCLI, flaskOverride)
	if err != nil {
		fmt.Fprintf(os.Stderr, "perf suite failed: %v\n", err)
		return 1
	}

	// Determine comparator target directory.
	compDir := filepath.Join(workDir, "flask-83")
	switch *corpus {
	case "go-1k":
		compDir = filepath.Join(workDir, "go-1k")
	case "mixed":
		compDir = filepath.Join(workDir, "mixed")
		if err := bench.GenerateMixedPolyglotCorpus(compDir); err != nil {
			fmt.Fprintf(os.Stderr, "generate mixed corpus: %v\n", err)
			return 1
		}
	case "custom":
		if strings.TrimSpace(*repoPath) == "" {
			fmt.Fprintln(os.Stderr, "--repo is required when --corpus=custom")
			return 2
		}
		absRepo, aerr := filepath.Abs(*repoPath)
		if aerr != nil {
			fmt.Fprintf(os.Stderr, "resolve repo path: %v\n", aerr)
			return 1
		}
		compDir = absRepo
	}

	enabled := parseEnabledTools(*toolsFlag)
	compResults, genSCIP := RunAllComparators(ctx, compDir, *corpus, resolvedCLI, enabled, perfRes)

	scipTarget := *scipFile
	if scipTarget == "" {
		scipTarget = genSCIP
	}

	var accReport *bench.AccuracyReport
	var accBreaches []string
	if scipTarget != "" {
		scipIdx, serr := bench.ParseSCIPFile(scipTarget)
		if serr != nil {
			fmt.Fprintf(os.Stderr, "warning: parse scip file %s: %v\n", scipTarget, serr)
		} else {
			truth := bench.ExtractCallEdgesFromSCIP(scipIdx)
			idxRes, ierr := pipeline.IndexRepository(ctx, compDir, config.DefaultConfig(), pipeline.IndexOptions{Force: true})
			if ierr != nil {
				fmt.Fprintf(os.Stderr, "accuracy re-index failed: %v\n", ierr)
				return 1
			}
			r := bench.EvaluateGraphAccuracy(*corpus, idxRes.Graph, truth)
			accReport = &r
			accBreaches = bench.CheckAccuracyGates(r, 0.90, 0.85, 0.75)
		}
	}

	targets := bench.DefaultSLATargets()
	rep := FullBenchReport{
		GeneratedAt:      time.Now().UTC().Format(time.RFC3339),
		GoVersion:        runtime.Version(),
		OSArch:           runtime.GOOS + "/" + runtime.GOARCH,
		NumCPU:           runtime.NumCPU(),
		Corpus:           *corpus,
		Performance:      perfRes,
		Targets:          targets,
		SLABreaches:      bench.CheckSLAs(perfRes, targets),
		Accuracy:         accReport,
		AccuracyBreaches: accBreaches,
		Comparators:      compResults,
	}

	if *baselineJSON != "" {
		data, rerr := os.ReadFile(*baselineJSON)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "read baseline json: %v\n", rerr)
			return 1
		}
		var prev FullBenchReport
		if uerr := json.Unmarshal(data, &prev); uerr != nil {
			fmt.Fprintf(os.Stderr, "unmarshal baseline json: %v\n", uerr)
			return 1
		}
		rep.BaselineDeltas, rep.RegressionBreaches = CompareBaseline(rep, prev, *maxRegression)
	}

	md := RenderMarkdownReport(rep)
	fmt.Println(md)

	if *outJSON != "" {
		if err := writeReportFile(*outJSON, mustMarshalJSON(rep)); err != nil {
			fmt.Fprintf(os.Stderr, "write json report: %v\n", err)
			return 1
		}
	}
	if *outMD != "" {
		if err := writeReportFile(*outMD, []byte(md)); err != nil {
			fmt.Fprintf(os.Stderr, "write markdown report: %v\n", err)
			return 1
		}
	}

	if *assertSLA && (len(rep.SLABreaches) > 0 || len(rep.AccuracyBreaches) > 0 || len(rep.RegressionBreaches) > 0) {
		fmt.Fprintf(os.Stderr, "benchmark gates failed: sla=%v accuracy=%v regression=%v\n",
			rep.SLABreaches, rep.AccuracyBreaches, rep.RegressionBreaches)
		return 1
	}
	return 0
}

func buildStrippedBinary(ctx context.Context, outDir string) (string, error) {
	binName := "gographctx"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	outPath := filepath.Join(outDir, binName)
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-ldflags=-s -w", "-o", outPath, "./cmd/gographctx")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return outPath, nil
}

func parseEnabledTools(raw string) map[string]bool {
	all := []string{"gographctx", "cgc", "scip", "ctags", "ast-grep", "serena", "ripgrep"}
	out := make(map[string]bool, len(all))
	trimmed := strings.TrimSpace(strings.ToLower(raw))
	if trimmed == "" || trimmed == "all" {
		for _, t := range all {
			out[t] = true
		}
		return out
	}
	for _, tok := range strings.Split(trimmed, ",") {
		k := strings.TrimSpace(tok)
		if k != "" {
			out[k] = true
		}
	}
	return out
}

func mustMarshalJSON(v any) []byte {
	b, _ := json.MarshalIndent(v, "", "  ")
	return append(b, '\n')
}

func writeReportFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
