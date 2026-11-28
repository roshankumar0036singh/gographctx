package mcp

import (
	"encoding/json"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/roshankumar0036singh/gographctx/internal/config"
	"github.com/roshankumar0036singh/gographctx/internal/graph"
	"github.com/roshankumar0036singh/gographctx/internal/storage"
)

// FormatterConfig controls token budgeting, path normalization, and metadata
// emitted by Formatter.
type FormatterConfig struct {
	RepoRoot           string         // Optional root path stripped from emitted file paths
	RepoName           string         // Display repository name in metadata
	DefaultTokenBudget int            // Defaults to config.DefaultTokenBudget (4000)
	ToolBudgets        map[string]int // Optional per-tool token budget overrides
	FixedTimestamp     string         // Optional deterministic RFC3339 timestamp for golden tests
}

// ConfidenceSummary summarizes tier certainty across edges or items in a response.
type ConfidenceSummary struct {
	Certain       int     `json:"certain"`
	Likely        int     `json:"likely"`
	Unresolved    int     `json:"unresolved"`
	OverallLabel  string  `json:"overall_label"`
	MinConfidence float32 `json:"min_confidence"`
	MaxConfidence float32 `json:"max_confidence"`
}

// ResponseMetadata carries index provenance and budget metrics on every MCP response.
type ResponseMetadata struct {
	Tool               string  `json:"tool"`
	IndexVersion       uint32  `json:"index_version"`
	RepoName           string  `json:"repo_name,omitempty"`
	Timestamp          string  `json:"timestamp"`
	ResolutionCoverage float64 `json:"resolution_coverage"`
	TokenBudget        int     `json:"token_budget"`
	EstimatedTokens    int     `json:"estimated_tokens"`
}

// FormattedResponse is the deterministic LLM-facing envelope emitted for tool calls.
type FormattedResponse struct {
	Tool              string            `json:"tool"`
	Summary           string            `json:"summary,omitempty"`
	Truncated         bool              `json:"truncated"`
	Total             int               `json:"total"`
	Returned          int               `json:"returned"`
	ConfidenceSummary ConfidenceSummary `json:"confidence_summary"`
	Metadata          ResponseMetadata  `json:"metadata"`
	Data              any               `json:"data"`
}

// ToolPayload supplies structured data alongside shrinkable slice rendering for
// token-budget enforcement.
type ToolPayload struct {
	Summary       string
	Total         int
	MaxItems      int
	PreTruncated  bool
	ItemTiers     []graph.ResolutionTier // Parallel to slice length when item-level confidence applies
	StaticTiers   []graph.ResolutionTier // Used when MaxItems == 0 (e.g. aggregate overview)
	BuildEnvelope func(limit int) (returned int, data any)
}

// Formatter serializes tool results deterministically within token budgets.
type Formatter struct {
	cfg FormatterConfig
}

// NewFormatter constructs a Formatter with sensible defaults.
func NewFormatter(cfg FormatterConfig) *Formatter {
	if cfg.DefaultTokenBudget <= 0 {
		cfg.DefaultTokenBudget = config.DefaultTokenBudget
	}
	if cfg.RepoName == "" && cfg.RepoRoot != "" {
		cfg.RepoName = filepath.Base(filepath.Clean(cfg.RepoRoot))
	}
	return &Formatter{cfg: cfg}
}

// EstimateTokens estimates token count using ~4 characters per token for code/JSON.
func EstimateTokens(text string) int {
	n := len(text)
	if n == 0 {
		return 0
	}
	return (n + 3) / 4
}

// BudgetFor returns the configured token budget for toolName.
func (f *Formatter) BudgetFor(toolName string) int {
	if f == nil {
		return config.DefaultTokenBudget
	}
	if len(f.cfg.ToolBudgets) > 0 {
		if b, ok := f.cfg.ToolBudgets[toolName]; ok && b > 0 {
			return b
		}
	}
	if f.cfg.DefaultTokenBudget > 0 {
		return f.cfg.DefaultTokenBudget
	}
	return config.DefaultTokenBudget
}

// StripRepoPath converts rawPath to a clean forward-slash repository-relative path.
func (f *Formatter) StripRepoPath(rawPath string) string {
	repoRoot := ""
	if f != nil {
		repoRoot = f.cfg.RepoRoot
	}
	return StripRepoPath(repoRoot, rawPath)
}

// StripRepoPath strips repoRoot from rawPath and normalizes separators to '/'.
func StripRepoPath(repoRoot, rawPath string) string {
	trimmed := strings.TrimSpace(rawPath)
	if trimmed == "" {
		return ""
	}
	slashPath := strings.ReplaceAll(trimmed, "\\", "/")
	if strings.TrimSpace(repoRoot) != "" {
		normRoot := strings.TrimSuffix(strings.ReplaceAll(filepath.ToSlash(filepath.Clean(repoRoot)), "\\", "/"), "/")
		if normRoot != "" && normRoot != "." {
			if strings.EqualFold(slashPath, normRoot) {
				return "."
			}
			prefix := normRoot + "/"
			if len(slashPath) > len(prefix) && strings.EqualFold(slashPath[:len(prefix)], prefix) {
				slashPath = slashPath[len(prefix):]
			}
		}
	}
	cleaned := strings.TrimPrefix(path.Clean(slashPath), "./")
	if cleaned == "." {
		return "."
	}
	return cleaned
}

// Format renders payload into compact deterministic JSON strictly honoring the tool's
// token budget and recording explicit truncation + confidence metadata.
func (f *Formatter) Format(toolName string, payload ToolPayload, coverage float64) ([]byte, error) {
	budget := f.BudgetFor(toolName)
	ts := f.cfg.FixedTimestamp
	if ts == "" {
		ts = time.Now().UTC().Format(time.RFC3339)
	}

	renderAt := func(limit int) (FormattedResponse, []byte, error) {
		returned, data := payload.BuildEnvelope(limit)
		tiers := payload.StaticTiers
		if len(payload.ItemTiers) > 0 {
			end := min(returned, len(payload.ItemTiers))
			tiers = payload.ItemTiers[:end]
		}
		total := payload.Total
		if total < returned {
			total = returned
		}
		truncated := payload.PreTruncated || returned < total

		resp := FormattedResponse{
			Tool:              toolName,
			Summary:           payload.Summary,
			Truncated:         truncated,
			Total:             total,
			Returned:          returned,
			ConfidenceSummary: summarizeConfidence(tiers),
			Metadata: ResponseMetadata{
				Tool:               toolName,
				IndexVersion:       storage.FormatVersion,
				RepoName:           f.cfg.RepoName,
				Timestamp:          ts,
				ResolutionCoverage: roundCoverage(coverage),
				TokenBudget:        budget,
			},
			Data: data,
		}

		// First pass measures tokens, second pass stamps exact EstimatedTokens into metadata.
		raw, err := json.Marshal(resp)
		if err != nil {
			return resp, nil, err
		}
		resp.Metadata.EstimatedTokens = EstimateTokens(string(raw))
		raw, err = json.Marshal(resp)
		if err != nil {
			return resp, nil, err
		}
		resp.Metadata.EstimatedTokens = EstimateTokens(string(raw))
		raw, err = json.Marshal(resp)
		return resp, raw, err
	}

	limit := payload.MaxItems
	resp, raw, err := renderAt(limit)
	if err != nil {
		return nil, err
	}
	if resp.Metadata.EstimatedTokens <= budget || limit <= 0 {
		return raw, nil
	}

	// Binary search largest returned count <= limit that fits inside budget.
	lo, hi := 0, limit
	bestRaw := raw
	for lo <= hi {
		mid := lo + (hi-lo)/2
		rMid, rawMid, err := renderAt(mid)
		if err != nil {
			return nil, err
		}
		if rMid.Metadata.EstimatedTokens <= budget {
			bestRaw = rawMid
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}

	// If even 0 items exceeded an extremely tiny budget, return the 0-item minimal envelope.
	if hi < 0 {
		_, zeroRaw, err := renderAt(0)
		if err != nil {
			return nil, err
		}
		return zeroRaw, nil
	}

	return bestRaw, nil
}

func summarizeConfidence(tiers []graph.ResolutionTier) ConfidenceSummary {
	if len(tiers) == 0 {
		return ConfidenceSummary{
			OverallLabel: "certain",
		}
	}
	cs := ConfidenceSummary{
		MinConfidence: 1.0,
		MaxConfidence: 0.0,
	}
	for _, t := range tiers {
		conf := t.Confidence()
		if conf < cs.MinConfidence {
			cs.MinConfidence = conf
		}
		if conf > cs.MaxConfidence {
			cs.MaxConfidence = conf
		}
		switch {
		case t.IsCertain():
			cs.Certain++
		case t == graph.TierTypeNarrowed:
			cs.Likely++
		default:
			cs.Unresolved++
		}
	}
	switch {
	case cs.Unresolved > 0 && cs.Certain == 0 && cs.Likely == 0:
		cs.OverallLabel = "unresolved"
	case cs.Likely > 0 || cs.Unresolved > 0:
		cs.OverallLabel = "mixed"
	default:
		cs.OverallLabel = "certain"
	}
	return cs
}

func roundCoverage(v float64) float64 {
	return float64(int(v*10000+0.5)) / 10000
}
