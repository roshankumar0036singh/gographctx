package bench

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/roshankumar0036singh/gographctx/internal/config"
	"github.com/roshankumar0036singh/gographctx/internal/pipeline"
)

// encodeSCIPTestIndex serializes a minimal valid binary SCIP protobuf Index for unit testing
// ParseSCIPBytes and ExtractCallEdgesFromSCIP without external scip-python binaries.
func encodeSCIPTestIndex(docs []SCIPDocument) []byte {
	var indexBytes []byte
	for _, doc := range docs {
		var docBytes []byte
		docBytes = appendProtoString(docBytes, 1, doc.RelativePath)
		for _, occ := range doc.Occurrences {
			var occBytes []byte
			if len(occ.Range) > 0 {
				var packed []byte
				for _, r := range occ.Range {
					packed = binary.AppendUvarint(packed, uint64(uint32(r)))
				}
				docBytesEnc := appendProtoBytes(nil, 1, packed)
				occBytes = append(occBytes, docBytesEnc...)
			}
			occBytes = appendProtoString(occBytes, 2, occ.Symbol)
			if occ.SymbolRoles != 0 {
				occBytes = appendProtoVarint(occBytes, 3, uint64(uint32(occ.SymbolRoles)))
			}
			if len(occ.EnclosingRange) > 0 {
				var packed []byte
				for _, r := range occ.EnclosingRange {
					packed = binary.AppendUvarint(packed, uint64(uint32(r)))
				}
				occBytes = appendProtoBytes(occBytes, 7, packed)
			}
			docBytes = appendProtoBytes(docBytes, 2, occBytes)
		}
		if doc.Language != "" {
			docBytes = appendProtoString(docBytes, 4, doc.Language)
		}
		indexBytes = appendProtoBytes(indexBytes, 2, docBytes)
	}
	return indexBytes
}

func appendProtoVarint(dst []byte, fieldNum int, v uint64) []byte {
	dst = binary.AppendUvarint(dst, uint64(fieldNum<<3))
	return binary.AppendUvarint(dst, v)
}

func appendProtoBytes(dst []byte, fieldNum int, payload []byte) []byte {
	dst = binary.AppendUvarint(dst, uint64((fieldNum<<3)|2))
	dst = binary.AppendUvarint(dst, uint64(len(payload)))
	return append(dst, payload...)
}

func appendProtoString(dst []byte, fieldNum int, s string) []byte {
	return appendProtoBytes(dst, fieldNum, []byte(s))
}

func TestSCIPDecodeAndAccuracyEvaluation(t *testing.T) {
	// 1. Build Flask-like Python repo and index it with GoGraphCtx.
	repoDir := t.TempDir()
	if err := GenerateFlaskScalePythonCorpus(repoDir, 12); err != nil {
		t.Fatalf("GenerateFlaskScalePythonCorpus: %v", err)
	}
	idxRes, err := pipeline.IndexRepository(context.Background(), repoDir, config.DefaultConfig(), pipeline.IndexOptions{Force: true})
	if err != nil {
		t.Fatalf("IndexRepository: %v", err)
	}

	// 2. Construct synthetic SCIP binary index mirroring Flask core helper & app_core call chains.
	symClean := "scip-python pypi flask-bench 1.0 `app.helpers`/clean_input()."
	symSign := "scip-python pypi flask-bench 1.0 `app.helpers`/sign_token()."
	symAudit := "scip-python pypi flask-bench 1.0 `app.helpers`/audit_event()."
	symDispatch := "scip-python pypi flask-bench 1.0 `app.app_core`/FlaskApp#dispatch()."

	scipBytes := encodeSCIPTestIndex([]SCIPDocument{
		{
			RelativePath: "app/helpers.py",
			Language:     "python",
			Occurrences: []SCIPOccurrence{
				{Range: []int32{0, 4, 0, 15}, EnclosingRange: []int32{0, 0, 2, 0}, Symbol: symClean, SymbolRoles: SCIPRoleDefinition},
				{Range: []int32{3, 4, 3, 14}, EnclosingRange: []int32{3, 0, 5, 0}, Symbol: symSign, SymbolRoles: SCIPRoleDefinition},
				{Range: []int32{6, 4, 6, 15}, EnclosingRange: []int32{6, 0, 9, 0}, Symbol: symAudit, SymbolRoles: SCIPRoleDefinition},
				{Range: []int32{7, 14, 7, 25}, Symbol: symClean, SymbolRoles: 0},
				{Range: []int32{8, 11, 8, 21}, Symbol: symSign, SymbolRoles: 0},
			},
		},
		{
			RelativePath: "app/app_core.py",
			Language:     "python",
			Occurrences: []SCIPOccurrence{
				{Range: []int32{11, 8, 11, 16}, EnclosingRange: []int32{11, 4, 14, 0}, Symbol: symDispatch, SymbolRoles: SCIPRoleDefinition},
				{Range: []int32{13, 15, 13, 26}, Symbol: symAudit, SymbolRoles: 0},
			},
		},
	})

	scipPath := filepath.Join(repoDir, "index.scip")
	if err := os.WriteFile(scipPath, scipBytes, 0o600); err != nil {
		t.Fatalf("WriteFile scip: %v", err)
	}
	parsedSCIP, err := ParseSCIPFile(scipPath)
	if err != nil {
		t.Fatalf("ParseSCIPFile: %v", err)
	}
	truth := ExtractCallEdgesFromSCIP(parsedSCIP)
	if len(truth) != 3 {
		t.Fatalf("expected 3 extracted SCIP truth edges, got %d (%+v)", len(truth), truth)
	}

	// Evaluate GoGraphCtx accuracy against the SCIP oracle.
	// Filter truth check on the covered core subset: evaluate against full graph.
	rep := EvaluateGraphAccuracy("flask-core", idxRes.Graph, truth)
	if rep.MatchedCertain < 3 {
		t.Fatalf("expected all 3 oracle call edges matched by certain tiers, got %+v", rep)
	}
	if rep.StrongTierRatio < 0.85 {
		t.Fatalf("expected strong-tier ratio >= 0.85 on flask corpus, got %.3f", rep.StrongTierRatio)
	}
	if rep.Recall < 0.99 {
		t.Fatalf("expected 1.00 oracle recall on covered edges, got %.3f (%+v)", rep.Recall, rep)
	}
}
