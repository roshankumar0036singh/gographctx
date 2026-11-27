package watcher

import (
	"cmp"
	"slices"
	"strings"
	"sync"

	"github.com/roshankumar0036singh/gographctx/internal/graph"
)

// DepTracker maintains a thread-safe reverse file dependency index derived from
// graph edges (IMPORTS, CALLS, LIKELY_CALLS, INHERITS, IMPLEMENTS, DECORATED_BY).
type DepTracker struct {
	mu           sync.RWMutex
	depsByTarget map[string]map[string]struct{} // targetFile -> set(dependentFile)
	targetsByDep map[string]map[string]struct{} // dependentFile -> set(targetFile)
}

// NewDepTracker constructs an empty DepTracker.
func NewDepTracker() *DepTracker {
	return &DepTracker{
		depsByTarget: make(map[string]map[string]struct{}),
		targetsByDep: make(map[string]map[string]struct{}),
	}
}

// RefreshFromView rebuilds the entire reverse dependency index from view.
func (dt *DepTracker) RefreshFromView(view *graph.ReadView) {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	dt.depsByTarget = make(map[string]map[string]struct{})
	dt.targetsByDep = make(map[string]map[string]struct{})
	if view == nil {
		return
	}

	for _, e := range view.AllEdges() {
		if !e.Kind.IsDependency() {
			continue
		}
		srcNode, okSrc := view.GetNode(e.Source)
		tgtNode, okTgt := view.GetNode(e.Target)
		if !okSrc || !okTgt || srcNode.File == "" || tgtNode.File == "" || srcNode.File == tgtNode.File {
			continue
		}
		dt.addPairLocked(tgtNode.File, srcNode.File)
	}
}

// UpdateFileOutgoing replaces all tracked outgoing dependencies originating from
// depFile using current edges in view.
func (dt *DepTracker) UpdateFileOutgoing(depFile string, view *graph.ReadView) {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	dt.clearDepOutgoingLocked(depFile)
	if view == nil || depFile == "" {
		return
	}

	for _, srcNode := range view.GetByFile(depFile) {
		for _, e := range view.GetOutgoingEdges(srcNode.UID) {
			if !e.Kind.IsDependency() {
				continue
			}
			tgtNode, ok := view.GetNode(e.Target)
			if !ok || tgtNode.File == "" || tgtNode.File == depFile {
				continue
			}
			dt.addPairLocked(tgtNode.File, depFile)
		}
	}
}

// RemoveFile removes all dependency entries associated with file (both as source and target).
func (dt *DepTracker) RemoveFile(file string) {
	dt.mu.Lock()
	defer dt.mu.Unlock()

	dt.clearDepOutgoingLocked(file)
	if deps, ok := dt.depsByTarget[file]; ok {
		for dep := range deps {
			if tgts, okT := dt.targetsByDep[dep]; okT {
				delete(tgts, file)
				if len(tgts) == 0 {
					delete(dt.targetsByDep, dep)
				}
			}
		}
		delete(dt.depsByTarget, file)
	}
}

// Dependents returns a deterministically sorted slice of repository-relative file
// paths that depend on symbols or imports in targetFile.
func (dt *DepTracker) Dependents(targetFile string) []string {
	dt.mu.RLock()
	defer dt.mu.RUnlock()

	deps := dt.depsByTarget[targetFile]
	if len(deps) == 0 {
		return nil
	}
	out := make([]string, 0, len(deps))
	for dep := range deps {
		if dep != "" && dep != targetFile {
			out = append(out, dep)
		}
	}
	slices.SortFunc(out, cmp.Compare[string])
	return out
}

// CallSiteDependents returns files containing unresolved or ambiguous CallSites
// whose lookup key or callee expression matches any symbol in symbolNames.
func (dt *DepTracker) CallSiteDependents(view *graph.ReadView, excludeFile string, symbolNames []string) []string {
	if view == nil || len(symbolNames) == 0 {
		return nil
	}
	wanted := make(map[string]struct{}, len(symbolNames))
	for _, s := range symbolNames {
		if s != "" {
			wanted[s] = struct{}{}
		}
	}
	if len(wanted) == 0 {
		return nil
	}

	seenFiles := make(map[string]struct{})
	for _, cs := range view.GetCallSites() {
		if cs.File == "" || cs.File == excludeFile {
			continue
		}
		if callSiteMatchesAnySymbol(cs, wanted) {
			seenFiles[cs.File] = struct{}{}
		}
	}

	if len(seenFiles) == 0 {
		return nil
	}
	out := make([]string, 0, len(seenFiles))
	for f := range seenFiles {
		out = append(out, f)
	}
	slices.SortFunc(out, cmp.Compare[string])
	return out
}

func callSiteMatchesAnySymbol(cs graph.CallSite, wanted map[string]struct{}) bool {
	if _, ok := wanted[cs.CalleeExpr]; ok {
		return true
	}
	if idx := strings.LastIndexByte(cs.CalleeExpr, '.'); idx >= 0 {
		if _, ok := wanted[cs.CalleeExpr[idx+1:]]; ok {
			return true
		}
	}
	if idx := strings.LastIndex(cs.CalleeExpr, "::"); idx >= 0 {
		if _, ok := wanted[cs.CalleeExpr[idx+2:]]; ok {
			return true
		}
	}
	if idx := strings.LastIndex(cs.LookupKey, "::"); idx >= 0 {
		if _, ok := wanted[cs.LookupKey[idx+2:]]; ok {
			return true
		}
	}
	return false
}

func (dt *DepTracker) addPairLocked(targetFile, depFile string) {
	deps, ok := dt.depsByTarget[targetFile]
	if !ok {
		deps = make(map[string]struct{})
		dt.depsByTarget[targetFile] = deps
	}
	deps[depFile] = struct{}{}

	tgts, ok := dt.targetsByDep[depFile]
	if !ok {
		tgts = make(map[string]struct{})
		dt.targetsByDep[depFile] = tgts
	}
	tgts[targetFile] = struct{}{}
}

func (dt *DepTracker) clearDepOutgoingLocked(depFile string) {
	tgts, ok := dt.targetsByDep[depFile]
	if !ok {
		return
	}
	for tgt := range tgts {
		if deps, okD := dt.depsByTarget[tgt]; okD {
			delete(deps, depFile)
			if len(deps) == 0 {
				delete(dt.depsByTarget, tgt)
			}
		}
	}
	delete(dt.targetsByDep, depFile)
}
