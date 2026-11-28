package mcp_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/roshankumar0036singh/gographctx/internal/graph"
	"github.com/roshankumar0036singh/gographctx/internal/mcp"
)

func TestFormatterTokenBudgetTruncationPathStrippingAndDeterminism(t *testing.T) {
	items := make([]mcp.SymbolSummary, 60)
	tiers := make([]graph.ResolutionTier, 60)
	for i := 0; i < 60; i++ {
		items[i] = mcp.SymbolSummary{
			UID:       fmt.Sprintf("%032x", i+1),
			Kind:      "Function",
			Name:      fmt.Sprintf("HandleRequest_%02d", i),
			QName:     fmt.Sprintf("pkg/service::HandleRequest_%02d", i),
			File:      mcp.StripRepoPath("/home/dev/acme-repo", fmt.Sprintf("/home/dev/acme-repo/internal/service/handler_%02d.go", i)),
			StartLine: uint32(10 + i),
			Language:  "go",
			Exported:  true,
		}
		if i%2 == 0 {
			tiers[i] = graph.TierQualifiedImport
		} else {
			tiers[i] = graph.TierTypeNarrowed
		}
	}

	f := mcp.NewFormatter(mcp.FormatterConfig{
		RepoRoot:           "/home/dev/acme-repo",
		RepoName:           "acme-repo",
		DefaultTokenBudget: 4000,
		ToolBudgets:        map[string]int{"find_symbol": 350}, // strict tight budget
		FixedTimestamp:     "2026-11-28T00:00:00Z",
	})

	payload := mcp.ToolPayload{
		Summary:   "Found 60 matching symbol(s)",
		Total:     len(items),
		MaxItems:  len(items),
		ItemTiers: tiers,
		BuildEnvelope: func(limit int) (int, any) {
			return limit, map[string]any{"symbols": items[:limit]}
		},
	}

	raw1, err := f.Format("find_symbol", payload, 0.91234)
	if err != nil {
		t.Fatalf("Format failed: %v", err)
	}
	raw2, err := f.Format("find_symbol", payload, 0.91234)
	if err != nil {
		t.Fatalf("Format second run failed: %v", err)
	}
	if !bytes.Equal(raw1, raw2) {
		t.Fatalf("expected deterministic byte-identical formatted output")
	}

	if est := mcp.EstimateTokens(string(raw1)); est > 350 {
		t.Fatalf("response exceeded tool token budget 350: got %d tokens\n%s", est, raw1)
	}

	var env mcp.FormattedResponse
	if err := json.Unmarshal(raw1, &env); err != nil {
		t.Fatalf("unmarshal FormattedResponse: %v", err)
	}
	if !env.Truncated || env.Total != 60 || env.Returned == 0 || env.Returned >= 60 {
		t.Fatalf("expected partial truncation metadata, got truncated=%v total=%d returned=%d",
			env.Truncated, env.Total, env.Returned)
	}
	if env.ConfidenceSummary.OverallLabel != "mixed" || env.ConfidenceSummary.Certain == 0 || env.ConfidenceSummary.Likely == 0 {
		t.Fatalf("unexpected ConfidenceSummary: %+v", env.ConfidenceSummary)
	}
	if env.Metadata.TokenBudget != 350 || env.Metadata.EstimatedTokens > 350 || env.Metadata.ResolutionCoverage != 0.9123 {
		t.Fatalf("unexpected Metadata: %+v", env.Metadata)
	}
	if bytes.Contains(raw1, []byte("/home/dev/acme-repo")) {
		t.Fatalf("formatted output leaked absolute repoRoot path: %s", raw1)
	}
	if got := mcp.StripRepoPath(`C:\Users\dev\myrepo`, `C:\Users\dev\myrepo\pkg\a.go`); got != "pkg/a.go" {
		t.Fatalf("StripRepoPath Windows path = %q, want pkg/a.go", got)
	}

	// Verify unresolved-only tiers classify OverallLabel as "unresolved".
	unresPayload := mcp.ToolPayload{
		Summary:   "Unresolved only",
		Total:     1,
		MaxItems:  1,
		ItemTiers: []graph.ResolutionTier{graph.TierUnresolved},
		BuildEnvelope: func(limit int) (int, any) {
			return limit, map[string]any{"items": items[:limit]}
		},
	}
	unresRaw, err := f.Format("find_symbol", unresPayload, 0)
	if err != nil {
		t.Fatalf("Format unresolved failed: %v", err)
	}
	var unresEnv mcp.FormattedResponse
	if err := json.Unmarshal(unresRaw, &unresEnv); err != nil {
		t.Fatalf("unmarshal unresolved FormattedResponse: %v", err)
	}
	if unresEnv.ConfidenceSummary.OverallLabel != "unresolved" || unresEnv.ConfidenceSummary.Unresolved != 1 || unresEnv.ConfidenceSummary.Certain != 0 {
		t.Fatalf("unexpected unresolved ConfidenceSummary: %+v", unresEnv.ConfidenceSummary)
	}
}
