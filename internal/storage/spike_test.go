package storage

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/roshankumar0036singh/gographctx/internal/graph"
)

func buildSpikeGraph150k600k(tb testing.TB) (*graph.Graph, []graph.Node, []graph.Edge) {
	tb.Helper()
	const (
		numFiles       = 10000
		symsPerFile    = 15
		edgesPerSymbol = 4
	)

	type fileSpec struct {
		file    string
		fileUID graph.UID
		fnUIDs  [symsPerFile - 1]graph.UID
	}
	specs := make([]fileSpec, numFiles)
	for f := 0; f < numFiles; f++ {
		path := fmt.Sprintf("pkg/mod_%02d/file_%04d.go", f%50, f)
		specs[f].file = path
		specs[f].fileUID = graph.GenerateNodeUID(graph.KindFile, path, 0)
		for s := 0; s < symsPerFile-1; s++ {
			qname := fmt.Sprintf("pkg/mod_%02d::F_%d_%d", f%50, f, s)
			specs[f].fnUIDs[s] = graph.GenerateNodeUID(graph.KindFunction, qname, 0)
		}
	}

	frags := make([]graph.FileFragment, numFiles)
	for f := 0; f < numFiles; f++ {
		sp := &specs[f]
		nodes := make([]graph.Node, 0, symsPerFile)
		nodes = append(nodes, graph.Node{
			UID:       sp.fileUID,
			Kind:      graph.KindFile,
			Name:      fmt.Sprintf("file_%04d.go", f),
			File:      sp.file,
			StartLine: 1,
			Language:  "go",
		})
		edges := make([]graph.Edge, 0, symsPerFile*edgesPerSymbol)
		for s := 0; s < symsPerFile-1; s++ {
			uid := sp.fnUIDs[s]
			nodes = append(nodes, graph.Node{
				UID:       uid,
				Kind:      graph.KindFunction,
				Name:      fmt.Sprintf("Fn_%d", s),
				QName:     fmt.Sprintf("pkg/mod_%02d::F_%d_%d", f%50, f, s),
				File:      sp.file,
				StartLine: uint32(10 + s*10),
				EndLine:   uint32(18 + s*10),
				Language:  "go",
			})
			edges = append(edges, graph.Edge{
				Source: sp.fileUID,
				Target: uid,
				Kind:   graph.EdgeContains,
				Tier:   graph.TierStructural,
			})
			for e := 0; e < edgesPerSymbol-1; e++ {
				tgtFile := (f + e + 1) % numFiles
				tgtSym := (s + e) % (symsPerFile - 1)
				edges = append(edges, graph.Edge{
					Source: uid,
					Target: specs[tgtFile].fnUIDs[tgtSym],
					Kind:   graph.EdgeCalls,
					Tier:   graph.TierQualifiedImport,
					Sites:  []graph.CallSiteRef{{Line: uint32(12 + s*10 + e)}},
				})
			}
		}
		for k := len(edges); k < symsPerFile*edgesPerSymbol; k++ {
			tgtFile := (f + k + 1) % numFiles
			if tgtFile == f {
				tgtFile = (f + 1) % numFiles
			}
			edges = append(edges, graph.Edge{
				Source: sp.fileUID,
				Target: specs[tgtFile].fnUIDs[0],
				Kind:   graph.EdgeImports,
				Tier:   graph.TierStructural,
			})
		}
		frags[f] = graph.FileFragment{File: sp.file, Nodes: nodes, Edges: edges}
	}

	g := graph.NewWithCapacity(150000, 600000)
	if err := g.MergeFragments(frags); err != nil {
		tb.Fatalf("MergeFragments failed: %v", err)
	}
	return g, frags[0].Nodes, frags[0].Edges
}

// BenchmarkPersistenceSpike150kNodes600kEdges compares SQLite (WAL mode) against
// Custom Snapshot+WAL (raw and zstd) at 150,000 nodes and 600,000 edges across:
//   - full write latency & disk size
//   - startup load latency into *graph.Graph
//   - incremental single-file (15 nodes, 60 edges) update latency
func BenchmarkPersistenceSpike150kNodes600kEdges(b *testing.B) {
	g, sampleNodes, sampleEdges := buildSpikeGraph150k600k(b)
	allNodes := g.AllNodes()
	allEdges := g.AllEdges()

	b.Run("Snapshot_Raw", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			dir := b.TempDir()
			snapPath := filepath.Join(dir, "index.ggctx")
			walPath := filepath.Join(dir, "index.wal")

			t0 := time.Now()
			st, err := WriteSnapshot(snapPath, g, WriteOptions{Compress: false, Sync: false})
			if err != nil {
				b.Fatal(err)
			}
			writeMs := float64(time.Since(t0).Microseconds()) / 1000.0

			t1 := time.Now()
			loaded, _, err := ReadSnapshotDecode(snapPath)
			if err != nil {
				b.Fatal(err)
			}
			loadMs := float64(time.Since(t1).Microseconds()) / 1000.0
			_ = loaded

			wal, err := OpenWAL(walPath, WALOptions{Sync: false})
			if err != nil {
				b.Fatal(err)
			}
			t2 := time.Now()
			if _, err := wal.AppendReplaceFile(sampleNodes[0].File, sampleNodes, sampleEdges, nil); err != nil {
				b.Fatal(err)
			}
			incUs := float64(time.Since(t2).Nanoseconds()) / 1000.0
			_ = wal.Close()

			b.ReportMetric(writeMs, "write-ms")
			b.ReportMetric(loadMs, "load-ms")
			b.ReportMetric(incUs, "inc-write-us")
			b.ReportMetric(float64(st.TotalBytes)/(1024*1024), "disk-MB")
		}
	})

	b.Run("Snapshot_Zstd", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			dir := b.TempDir()
			snapPath := filepath.Join(dir, "index.ggctx")
			walPath := filepath.Join(dir, "index.wal")

			t0 := time.Now()
			st, err := WriteSnapshot(snapPath, g, WriteOptions{Compress: true, Sync: false})
			if err != nil {
				b.Fatal(err)
			}
			writeMs := float64(time.Since(t0).Microseconds()) / 1000.0

			t1 := time.Now()
			loaded, _, err := ReadSnapshotDecode(snapPath)
			if err != nil {
				b.Fatal(err)
			}
			loadMs := float64(time.Since(t1).Microseconds()) / 1000.0
			_ = loaded

			wal, err := OpenWAL(walPath, WALOptions{Sync: false})
			if err != nil {
				b.Fatal(err)
			}
			t2 := time.Now()
			if _, err := wal.AppendReplaceFile(sampleNodes[0].File, sampleNodes, sampleEdges, nil); err != nil {
				b.Fatal(err)
			}
			incUs := float64(time.Since(t2).Nanoseconds()) / 1000.0
			_ = wal.Close()

			b.ReportMetric(writeMs, "write-ms")
			b.ReportMetric(loadMs, "load-ms")
			b.ReportMetric(incUs, "inc-write-us")
			b.ReportMetric(float64(st.TotalBytes)/(1024*1024), "disk-MB")
		}
	})

	b.Run("SQLite_WAL", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			dir := b.TempDir()
			dbPath := filepath.Join(dir, "graph.db")

			db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
			if err != nil {
				b.Fatal(err)
			}
			_, err = db.Exec(`
				CREATE TABLE nodes (
					uid BLOB PRIMARY KEY,
					kind INTEGER NOT NULL,
					name TEXT NOT NULL,
					qname TEXT NOT NULL,
					file TEXT NOT NULL,
					start_line INTEGER NOT NULL,
					end_line INTEGER NOT NULL,
					language TEXT NOT NULL
				);
				CREATE TABLE edges (
					source BLOB NOT NULL,
					target BLOB NOT NULL,
					kind INTEGER NOT NULL,
					tier INTEGER NOT NULL,
					line INTEGER NOT NULL,
					PRIMARY KEY (source, target, kind)
				);
				CREATE INDEX idx_nodes_file ON nodes(file);
			`)
			if err != nil {
				b.Fatal(err)
			}

			t0 := time.Now()
			tx, err := db.Begin()
			if err != nil {
				b.Fatal(err)
			}
			nStmt, err := tx.Prepare(`INSERT INTO nodes(uid, kind, name, qname, file, start_line, end_line, language) VALUES(?, ?, ?, ?, ?, ?, ?, ?)`)
			if err != nil {
				b.Fatal(err)
			}
			for j := range allNodes {
				n := &allNodes[j]
				if _, err := nStmt.Exec(n.UID[:], int(n.Kind), n.Name, n.QName, n.File, n.StartLine, n.EndLine, n.Language); err != nil {
					b.Fatal(err)
				}
			}
			_ = nStmt.Close()

			eStmt, err := tx.Prepare(`INSERT INTO edges(source, target, kind, tier, line) VALUES(?, ?, ?, ?, ?)`)
			if err != nil {
				b.Fatal(err)
			}
			for j := range allEdges {
				e := &allEdges[j]
				var line uint32
				if len(e.Sites) > 0 {
					line = e.Sites[0].Line
				}
				if _, err := eStmt.Exec(e.Source[:], e.Target[:], int(e.Kind), int(e.Tier), line); err != nil {
					b.Fatal(err)
				}
			}
			_ = eStmt.Close()
			if err := tx.Commit(); err != nil {
				b.Fatal(err)
			}
			writeMs := float64(time.Since(t0).Microseconds()) / 1000.0

			// Startup load: read all rows back into a fresh Graph
			t1 := time.Now()
			loadedNodes := make([]graph.Node, 0, len(allNodes))
			rows, err := db.Query(`SELECT uid, kind, name, qname, file, start_line, end_line, language FROM nodes`)
			if err != nil {
				b.Fatal(err)
			}
			for rows.Next() {
				var uidBytes []byte
				var kind int
				var n graph.Node
				if err := rows.Scan(&uidBytes, &kind, &n.Name, &n.QName, &n.File, &n.StartLine, &n.EndLine, &n.Language); err != nil {
					b.Fatal(err)
				}
				copy(n.UID[:], uidBytes)
				n.Kind = graph.NodeKind(kind)
				loadedNodes = append(loadedNodes, n)
			}
			_ = rows.Close()

			loadedEdges := make([]graph.Edge, 0, len(allEdges))
			erows, err := db.Query(`SELECT source, target, kind, tier, line FROM edges`)
			if err != nil {
				b.Fatal(err)
			}
			for erows.Next() {
				var srcB, tgtB []byte
				var kind, tier int
				var line uint32
				if err := erows.Scan(&srcB, &tgtB, &kind, &tier, &line); err != nil {
					b.Fatal(err)
				}
				var e graph.Edge
				copy(e.Source[:], srcB)
				copy(e.Target[:], tgtB)
				e.Kind = graph.EdgeKind(kind)
				e.Tier = graph.ResolutionTier(tier)
				if line > 0 {
					e.Sites = []graph.CallSiteRef{{Line: line}}
				}
				loadedEdges = append(loadedEdges, e)
			}
			_ = erows.Close()

			loaded := graph.NewWithCapacity(len(loadedNodes), len(loadedEdges))
			if err := loaded.BulkLoad(loadedNodes, loadedEdges, nil); err != nil {
				b.Fatal(err)
			}
			loadMs := float64(time.Since(t1).Microseconds()) / 1000.0

			// Incremental file replacement transaction
			t2 := time.Now()
			itx, err := db.Begin()
			if err != nil {
				b.Fatal(err)
			}
			targetFile := sampleNodes[0].File
			if _, err := itx.Exec(`DELETE FROM edges WHERE source IN (SELECT uid FROM nodes WHERE file = ?)`, targetFile); err != nil {
				b.Fatal(err)
			}
			if _, err := itx.Exec(`DELETE FROM nodes WHERE file = ?`, targetFile); err != nil {
				b.Fatal(err)
			}
			inStmt, err := itx.Prepare(`INSERT INTO nodes(uid, kind, name, qname, file, start_line, end_line, language) VALUES(?, ?, ?, ?, ?, ?, ?, ?)`)
			if err != nil {
				b.Fatal(err)
			}
			for j := range sampleNodes {
				n := &sampleNodes[j]
				if _, err := inStmt.Exec(n.UID[:], int(n.Kind), n.Name, n.QName, n.File, n.StartLine, n.EndLine, n.Language); err != nil {
					b.Fatal(err)
				}
			}
			_ = inStmt.Close()
			ieStmt, err := itx.Prepare(`INSERT OR REPLACE INTO edges(source, target, kind, tier, line) VALUES(?, ?, ?, ?, ?)`)
			if err != nil {
				b.Fatal(err)
			}
			for j := range sampleEdges {
				e := &sampleEdges[j]
				var line uint32
				if len(e.Sites) > 0 {
					line = e.Sites[0].Line
				}
				if _, err := ieStmt.Exec(e.Source[:], e.Target[:], int(e.Kind), int(e.Tier), line); err != nil {
					b.Fatal(err)
				}
			}
			_ = ieStmt.Close()
			if err := itx.Commit(); err != nil {
				b.Fatal(err)
			}
			incUs := float64(time.Since(t2).Nanoseconds()) / 1000.0

			_ = db.Close()
			var totalDisk int64
			for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
				if fi, err := os.Stat(p); err == nil {
					totalDisk += fi.Size()
				}
			}

			b.ReportMetric(writeMs, "write-ms")
			b.ReportMetric(loadMs, "load-ms")
			b.ReportMetric(incUs, "inc-write-us")
			b.ReportMetric(float64(totalDisk)/(1024*1024), "disk-MB")
			_ = binary.LittleEndian
		}
	})
}
