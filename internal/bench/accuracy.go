package bench

import (
	"fmt"
	"strings"

	"github.com/roshankumar0036singh/gographctx/internal/graph"
)

// CallEdgeTruth represents one ground-truth intra-repo call edge extracted from a compiler oracle (e.g. SCIP).
type CallEdgeTruth struct {
	CallerFile string `json:"caller_file"`
	CallerName string `json:"caller_name"`
	CalleeFile string `json:"callee_file"`
	CalleeName string `json:"callee_name"`
	Line       uint32 `json:"line,omitempty"`
}

// AccuracyReport summarizes GoGraphCtx call resolution precision, recall, and strong-tier share.
type AccuracyReport struct {
	Corpus            string   `json:"corpus"`
	GroundTruthEdges  int      `json:"ground_truth_edges"`
	PredictedCertain  int      `json:"predicted_certain"`
	PredictedLikely   int      `json:"predicted_likely"`
	MatchedCertain    int      `json:"matched_certain"`
	MatchedWithLikely int      `json:"matched_with_likely"`
	PrecisionCertain  float64  `json:"precision_certain"`
	PrecisionOverall  float64  `json:"precision_overall"`
	Recall            float64  `json:"recall"`
	StrongTierRatio   float64  `json:"strong_tier_ratio"`
	FalsePositives    []string `json:"false_positives,omitempty"`
	FalseNegatives    []string `json:"false_negatives,omitempty"`
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
	g.View(func(v *graph.ReadView) {
		for _, e := range v.AllEdges() {
			if e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeLikelyCalls {
				continue
			}
			src, okS := v.GetNode(e.Source)
			tgt, okT := v.GetNode(e.Target)
			if !okS || !okT || src.File == "" || tgt.File == "" || tgt.Kind == graph.KindExternal {
				continue
			}
			k := edgeMatchKey(src.File, src.Name, tgt.File, tgt.Name)
			if e.Kind == graph.EdgeCalls {
				certainSet[k] = struct{}{}
			} else {
				likelySet[k] = struct{}{}
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

	for k := range truthSet {
		_, cOK := certainSet[k]
		_, lOK := likelySet[k]
		if !cOK && !lOK && len(rep.FalseNegatives) < 20 {
			rep.FalseNegatives = append(rep.FalseNegatives, k)
		}
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
