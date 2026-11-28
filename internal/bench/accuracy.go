package bench

import (
	"fmt"
	"strings"

	"github.com/roshankumar0036singh/gographctx/internal/graph"
)

// CallEdgeTruth represents one ground-truth intra-repo call edge extracted from a compiler/analyzer oracle (e.g. SCIP or Jedi).
type CallEdgeTruth struct {
	CallerFile string `json:"caller_file"`
	CallerName string `json:"caller_name"`
	CalleeFile string `json:"callee_file"`
	CalleeName string `json:"callee_name"`
	Category   string `json:"category,omitempty"` // optional: var_or_module, decorator, constructor, plain, self_cls
	Line       uint32 `json:"line,omitempty"`
}

// CategoryAccuracy captures precision/recall for one call-pattern bucket.
type CategoryAccuracy struct {
	Total   int     `json:"total"`
	Matched int     `json:"matched"`
	Recall  float64 `json:"recall"`
}

// AccuracyReport summarizes GoGraphCtx call resolution precision, recall, library-vs-test split, and strong-tier share.
type AccuracyReport struct {
	Corpus              string                      `json:"corpus"`
	GroundTruthEdges    int                         `json:"ground_truth_edges"`
	PredictedCertain    int                         `json:"predicted_certain"`
	PredictedLikely     int                         `json:"predicted_likely"`
	MatchedCertain      int                         `json:"matched_certain"`
	MatchedWithLikely   int                         `json:"matched_with_likely"`
	UnresolvedCallSites int                         `json:"unresolved_call_sites"`
	PrecisionCertain    float64                     `json:"precision_certain"`
	PrecisionOverall    float64                     `json:"precision_overall"`
	Recall              float64                     `json:"recall"`
	LibraryTruthEdges   int                         `json:"library_truth_edges,omitempty"`
	LibraryRecall       float64                     `json:"library_recall,omitempty"`
	TestTruthEdges      int                         `json:"test_truth_edges,omitempty"`
	TestRecall          float64                     `json:"test_recall,omitempty"`
	ByCategory          map[string]CategoryAccuracy `json:"by_category,omitempty"`
	StrongTierRatio     float64                     `json:"strong_tier_ratio"`
	FalsePositives      []string                    `json:"false_positives,omitempty"`
	FalseNegatives      []string                    `json:"false_negatives,omitempty"`
}

// EvaluateGraphAccuracy compares g's EdgeCalls (certain) and EdgeLikelyCalls (likely) edges
// against compiler-oracle ground truth edges.
func EvaluateGraphAccuracy(corpus string, g *graph.Graph, truth []CallEdgeTruth) AccuracyReport {
	rep := AccuracyReport{
		Corpus:           corpus,
		GroundTruthEdges: len(truth),
	}

	truthSet := make(map[string]CallEdgeTruth, len(truth))
	for _, t := range truth {
		truthSet[edgeMatchKey(t.CallerFile, t.CallerName, t.CalleeFile, t.CalleeName)] = t
	}

	certainSet := make(map[string]struct{})
	likelySet := make(map[string]struct{})
	var predKeysForEdge = func(src, tgt graph.Node) []string {
		callerNames := []string{src.Name}
		if src.Kind == graph.KindModule {
			callerNames = append(callerNames, "<module>", "<package_init>")
		}
		calleeNames := []string{tgt.Name}
		if tgt.Name == "__init__" || tgt.Name == "__new__" || tgt.Name == "constructor" {
			parts := strings.Split(tgt.QName, "::")
			if len(parts) >= 2 {
				calleeNames = append(calleeNames, parts[len(parts)-2])
			}
		}
		out := make([]string, 0, len(callerNames)*len(calleeNames))
		for _, cn := range callerNames {
			for _, tn := range calleeNames {
				out = append(out, edgeMatchKey(src.File, cn, tgt.File, tn))
			}
		}
		return out
	}

	g.View(func(v *graph.ReadView) {
		rep.UnresolvedCallSites = v.CallSiteCount()
		for _, e := range v.AllEdges() {
			if e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeLikelyCalls {
				continue
			}
			src, okS := v.GetNode(e.Source)
			tgt, okT := v.GetNode(e.Target)
			if !okS || !okT || src.File == "" || tgt.File == "" || tgt.Kind == graph.KindExternal {
				continue
			}
			keys := predKeysForEdge(src, tgt)
			canonical := keys[0]
			for _, candidate := range keys {
				if _, ok := truthSet[candidate]; ok {
					canonical = candidate
					break
				}
			}
			if e.Kind == graph.EdgeCalls {
				certainSet[canonical] = struct{}{}
			} else {
				likelySet[canonical] = struct{}{}
			}
		}
	})

	rep.PredictedCertain = len(certainSet)
	rep.PredictedLikely = len(likelySet)

	for k := range certainSet {
		if _, ok := truthSet[k]; ok {
			rep.MatchedCertain++
		} else if len(rep.FalsePositives) < 20 {
			rep.FalsePositives = append(rep.FalsePositives, k)
		}
	}

	unionMatched := rep.MatchedCertain
	for k := range likelySet {
		if _, ok := truthSet[k]; ok {
			if _, alreadyCertain := certainSet[k]; !alreadyCertain {
				unionMatched++
			}
		}
	}
	rep.MatchedWithLikely = unionMatched

	var libMatched, testMatched int
	catCounts := make(map[string]*CategoryAccuracy)
	for k, t := range truthSet {
		_, cOK := certainSet[k]
		_, lOK := likelySet[k]
		hit := cOK || lOK
		if !hit && len(rep.FalseNegatives) < 20 {
			rep.FalseNegatives = append(rep.FalseNegatives, k)
		}
		if isTestOrExampleFile(t.CallerFile) {
			rep.TestTruthEdges++
			if hit {
				testMatched++
			}
		} else {
			rep.LibraryTruthEdges++
			if hit {
				libMatched++
			}
		}
		if t.Category != "" {
			ca := catCounts[t.Category]
			if ca == nil {
				ca = &CategoryAccuracy{}
				catCounts[t.Category] = ca
			}
			ca.Total++
			if hit {
				ca.Matched++
			}
		}
	}
	if len(catCounts) > 0 {
		rep.ByCategory = make(map[string]CategoryAccuracy, len(catCounts))
		for k, v := range catCounts {
			if v.Total > 0 {
				v.Recall = float64(v.Matched) / float64(v.Total)
			}
			rep.ByCategory[k] = *v
		}
	}
	if rep.LibraryTruthEdges > 0 {
		rep.LibraryRecall = float64(libMatched) / float64(rep.LibraryTruthEdges)
	}
	if rep.TestTruthEdges > 0 {
		rep.TestRecall = float64(testMatched) / float64(rep.TestTruthEdges)
	}

	if rep.PredictedCertain > 0 {
		rep.PrecisionCertain = float64(rep.MatchedCertain) / float64(rep.PredictedCertain)
	}
	if totalPred := rep.PredictedCertain + rep.PredictedLikely; totalPred > 0 {
		rep.PrecisionOverall = float64(rep.MatchedWithLikely) / float64(totalPred)
		rep.StrongTierRatio = float64(rep.PredictedCertain) / float64(totalPred)
	}
	if rep.GroundTruthEdges > 0 {
		rep.Recall = float64(rep.MatchedWithLikely) / float64(rep.GroundTruthEdges)
	}
	return rep
}

func isTestOrExampleFile(rel string) bool {
	s := strings.ReplaceAll(strings.ToLower(rel), "\\", "/")
	return strings.HasPrefix(s, "tests/") || strings.HasPrefix(s, "test/") ||
		strings.HasPrefix(s, "examples/") || strings.Contains(s, "/tests/") ||
		strings.HasSuffix(s, "_test.go") || strings.HasPrefix(s, "test_")
}

// CheckAccuracyGates verifies accuracy targets and returns any breach descriptions.
func CheckAccuracyGates(rep AccuracyReport, minPrecision, minStrongTier, minRecall float64) []string {
	var breaches []string
	if rep.GroundTruthEdges == 0 {
		return []string{"accuracy: zero ground truth edges provided"}
	}
	if rep.PrecisionCertain < minPrecision {
		breaches = append(breaches, fmt.Sprintf("precision %.3f < %.3f", rep.PrecisionCertain, minPrecision))
	}
	if rep.StrongTierRatio < minStrongTier {
		breaches = append(breaches, fmt.Sprintf("strong-tier ratio %.3f < %.3f", rep.StrongTierRatio, minStrongTier))
	}
	if rep.Recall < minRecall {
		breaches = append(breaches, fmt.Sprintf("recall %.3f < %.3f", rep.Recall, minRecall))
	}
	return breaches
}

func edgeMatchKey(callerFile, callerName, calleeFile, calleeName string) string {
	cf := strings.TrimPrefix(strings.ReplaceAll(callerFile, "\\", "/"), "./")
	tf := strings.TrimPrefix(strings.ReplaceAll(calleeFile, "\\", "/"), "./")
	cn := normalizeShortSymbol(callerName)
	tn := normalizeShortSymbol(calleeName)
	return fmt.Sprintf("%s:%s->%s:%s", cf, cn, tf, tn)
}

func normalizeShortSymbol(name string) string {
	s := strings.TrimSpace(name)
	if idx := strings.LastIndex(s, "::"); idx >= 0 {
		s = s[idx+2:]
	}
	if idx := strings.LastIndexByte(s, '.'); idx >= 0 {
		s = s[idx+1:]
	}
	return s
}
