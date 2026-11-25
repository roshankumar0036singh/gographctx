package graph

import (
	"fmt"
	"runtime"
	"sync"
	"testing"
)

// BenchmarkBuild150kNodes600kEdges constructs 10,000 files * 15 symbols = 150,000 nodes
// and 600,000 edges using parallel FileFragment generation + MergeFragments, then reports
// live heap bytes to verify the <200 MB RSS/Heap target.
func BenchmarkBuild150kNodes600kEdges(b *testing.B) {
	const (
		numFiles       = 10000
		symsPerFile    = 15
		edgesPerSymbol = 4
	)

	// Pre-compute UIDs deterministically once for benchmark fixture assembly.
	type fileSpec struct {
		file    string
		fileUID UID
		fnUIDs  [symsPerFile - 1]UID
	}
	specs := make([]fileSpec, numFiles)
	for f := 0; f < numFiles; f++ {
		path := fmt.Sprintf("pkg/mod_%02d/file_%04d.go", f%50, f)
		specs[f].file = path
		specs[f].fileUID = GenerateNodeUID(KindFile, path, 0)
		for s := 0; s < symsPerFile-1; s++ {
			qname := fmt.Sprintf("pkg/mod_%02d::F_%d_%d", f%50, f, s)
			specs[f].fnUIDs[s] = GenerateNodeUID(KindFunction, qname, 0)
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)
		b.StartTimer()

		frags := make([]FileFragment, numFiles)
		workers := runtime.NumCPU()
		var wg sync.WaitGroup
		chunk := (numFiles + workers - 1) / workers
		for w := 0; w < workers; w++ {
			start := w * chunk
			end := start + chunk
			if end > numFiles {
				end = numFiles
			}
			if start >= end {
				continue
			}
			wg.Add(1)
			go func(lo, hi int) {
				defer wg.Done()
				for f := lo; f < hi; f++ {
					sp := &specs[f]
					nodes := make([]Node, 0, symsPerFile)
					nodes = append(nodes, Node{
						UID:       sp.fileUID,
						Kind:      KindFile,
						Name:      fmt.Sprintf("file_%04d.go", f),
						File:      sp.file,
						StartLine: 1,
						Language:  "go",
					})
					edges := make([]Edge, 0, symsPerFile*edgesPerSymbol)
					for s := 0; s < symsPerFile-1; s++ {
						uid := sp.fnUIDs[s]
						nodes = append(nodes, Node{
							UID:       uid,
							Kind:      KindFunction,
							Name:      fmt.Sprintf("Fn_%d", s),
							QName:     fmt.Sprintf("pkg/mod_%02d::F_%d_%d", f%50, f, s),
							File:      sp.file,
							StartLine: uint32(10 + s*10),
							EndLine:   uint32(18 + s*10),
							Language:  "go",
						})
						edges = append(edges, Edge{
							Source: sp.fileUID,
							Target: uid,
							Kind:   EdgeContains,
							Tier:   TierStructural,
						})
						for e := 0; e < edgesPerSymbol-1; e++ {
							tgtFile := (f + e + 1) % numFiles
							tgtSym := (s + e) % (symsPerFile - 1)
							edges = append(edges, Edge{
								Source: uid,
								Target: specs[tgtFile].fnUIDs[tgtSym],
								Kind:   EdgeCalls,
								Tier:   TierQualifiedImport,
								Sites:  []CallSiteRef{{Line: uint32(12 + s*10 + e)}},
							})
						}
					}
					// Top up to 60 edges per file (add IMPORTS edges from file to neighbor files)
					for k := len(edges); k < symsPerFile*edgesPerSymbol; k++ {
						tgtFile := (f + k + 1) % numFiles
						if tgtFile == f {
							tgtFile = (f + 1) % numFiles
						}
						edges = append(edges, Edge{
							Source: sp.fileUID,
							Target: specs[tgtFile].fnUIDs[0],
							Kind:   EdgeImports,
							Tier:   TierStructural,
						})
					}
					frags[f] = FileFragment{File: sp.file, Nodes: nodes, Edges: edges}
				}
			}(start, end)
		}
		wg.Wait()

		g := &Graph{}
		if err := g.MergeFragments(frags); err != nil {
			b.Fatalf("MergeFragments failed: %v", err)
		}
		b.StopTimer()
		frags = nil // release temporary input fragments before measuring live graph heap

		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)

		var heapBytes uint64
		if after.HeapAlloc > before.HeapAlloc {
			heapBytes = after.HeapAlloc - before.HeapAlloc
		} else {
			heapBytes = after.HeapAlloc
		}
		b.ReportMetric(float64(heapBytes)/(1024*1024), "heap-MB")
		b.ReportMetric(float64(g.NodeCount()), "nodes")
		b.ReportMetric(float64(g.EdgeCount()), "edges")
		runtime.KeepAlive(g)
		b.StartTimer()
	}
}

func BenchmarkRemoveHubNode(b *testing.B) {
	for _, inDegree := range []int{3000, 30000} {
		b.Run(fmt.Sprintf("InDegree=%d", inDegree), func(b *testing.B) {
			buildHub := func() (*Graph, UID) {
				g := NewWithCapacity(inDegree+1, inDegree)
				hubUID, _ := g.AddNode(Node{
					Kind:      KindFunction,
					Name:      "Log",
					QName:     "log::Log",
					File:      "log.go",
					StartLine: 10,
				})
				for c := 0; c < inDegree; c++ {
					callerUID, _ := g.AddNode(Node{
						Kind:      KindFunction,
						Name:      fmt.Sprintf("Caller_%d", c),
						QName:     fmt.Sprintf("pkg::Caller_%d", c),
						File:      fmt.Sprintf("f_%d.go", c/10),
						StartLine: uint32(10 + c%10),
					})
					_ = g.AddEdge(Edge{
						Source: callerUID,
						Target: hubUID,
						Kind:   EdgeCalls,
						Tier:   TierQualifiedImport,
						Sites:  []CallSiteRef{{Line: 15}},
					})
				}
				return g, hubUID
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				g, hubUID := buildHub()
				b.StartTimer()
				if err := g.RemoveNode(hubUID); err != nil {
					b.Fatalf("RemoveNode(hub) failed: %v", err)
				}
			}
		})
	}
}
