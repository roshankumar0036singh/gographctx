package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/roshankumar0036singh/gographctx/internal/graph"
)

func assertGraphsEqual(t *testing.T, got, want *graph.Graph) {
	t.Helper()
	if err := got.VerifyIndexConsistency(); err != nil {
		t.Fatalf("loaded graph index consistency failed: %v", err)
	}
	if got.NodeCount() != want.NodeCount() || got.EdgeCount() != want.EdgeCount() || got.CallSiteCount() != want.CallSiteCount() {
		t.Fatalf("graph counts mismatch: got (%d nodes, %d edges, %d sites), want (%d, %d, %d)",
			got.NodeCount(), got.EdgeCount(), got.CallSiteCount(),
			want.NodeCount(), want.EdgeCount(), want.CallSiteCount())
	}
	gotNodes := got.AllNodes()
	wantNodes := want.AllNodes()
	if !reflect.DeepEqual(gotNodes, wantNodes) {
		t.Fatalf("AllNodes mismatch after snapshot+WAL reload")
	}
	gotEdges := got.AllEdges()
	wantEdges := want.AllEdges()
	if !reflect.DeepEqual(gotEdges, wantEdges) {
		t.Fatalf("AllEdges mismatch after snapshot+WAL reload")
	}
	gotSites := got.GetCallSites()
	wantSites := want.GetCallSites()
	if !reflect.DeepEqual(gotSites, wantSites) {
		t.Fatalf("GetCallSites mismatch after snapshot+WAL reload")
	}
}

func TestSnapshotPlusNRandomWALUpdatesRoundTrip(t *testing.T) {
	for _, compress := range []bool{false, true} {
		name := "raw"
		if compress {
			name = "zstd"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			rng := rand.New(rand.NewSource(42))

			// Build initial multi-file graph with external target + 10 files
			expected := graph.New()
			ext := graph.Node{Kind: graph.KindExternal, Name: "Printf", QName: "fmt.Printf"}
			extUID, err := expected.AddNode(ext)
			if err != nil {
				t.Fatal(err)
			}

			makeFilePayload := func(fileIdx int, rev int) ([]graph.Node, []graph.Edge, []graph.CallSite) {
				filePath := fmt.Sprintf("pkg/f%02d.go", fileIdx)
				fNode := graph.Node{Kind: graph.KindFile, Name: fmt.Sprintf("f%02d.go", fileIdx), File: filePath, StartLine: 1, Language: "go"}
				fUID := graph.ComputeNodeUID(fNode)

				fn1 := graph.Node{
					Kind:          graph.KindFunction,
					Name:          "Run",
					QName:         fmt.Sprintf("pkg::F%02d::Run", fileIdx),
					File:          filePath,
					StartLine:     uint32(10 + rev),
					EndLine:       uint32(30 + rev),
					Language:      "go",
					Exported:      true,
					Complexity:    uint16(1 + rev%10),
					Params:        []graph.Param{{Name: "ctx", Type: "Context"}},
					SignatureHash: graph.ComputeSignatureHash([]graph.Param{{Name: "ctx", Type: "Context"}}, "error"),
				}
				fn1UID := graph.ComputeNodeUID(fn1)
				nodes := []graph.Node{fNode, fn1}
				edges := []graph.Edge{
					{Source: fUID, Target: fn1UID, Kind: graph.EdgeContains, Tier: graph.TierStructural},
					{
						Source: fn1UID,
						Target: extUID,
						Kind:   graph.EdgeCalls,
						Tier:   graph.TierQualifiedImport,
						Sites: []graph.CallSiteRef{
							{Line: uint32(12 + rev), Column: 4},
							{Line: uint32(18 + rev), Column: 8},
						},
					},
				}

				// Optionally include or remove a helper node on even revisions
				if rev%2 == 0 {
					fn2 := graph.Node{
						Kind:      graph.KindFunction,
						Name:      "Helper",
						QName:     fmt.Sprintf("pkg::F%02d::Helper", fileIdx),
						File:      filePath,
						StartLine: uint32(40 + rev),
						Language:  "go",
					}
					fn2UID := graph.ComputeNodeUID(fn2)
					nodes = append(nodes, fn2)
					edges = append(edges,
						graph.Edge{Source: fUID, Target: fn2UID, Kind: graph.EdgeContains, Tier: graph.TierStructural},
						graph.Edge{Source: fn1UID, Target: fn2UID, Kind: graph.EdgeCalls, Tier: graph.TierLocal, Sites: []graph.CallSiteRef{{Line: uint32(15 + rev)}}},
					)
				}

				sites := []graph.CallSite{{
					Caller:     fn1UID,
					File:       filePath,
					Line:       uint32(20 + rev),
					CalleeExpr: "missingHook()",
					LookupKey:  fmt.Sprintf("Hook_%d", fileIdx),
					Status:     graph.CallStatusUnresolved,
					Tier:       graph.TierUnresolved,
				}}
				return nodes, edges, sites
			}

			for f := 0; f < 10; f++ {
				nodes, edges, sites := makeFilePayload(f, 0)
				if _, err := expected.ReplaceFile(fmt.Sprintf("pkg/f%02d.go", f), nodes, edges, sites); err != nil {
					t.Fatal(err)
				}
			}

			store, err := OpenStore(dir, WriteOptions{Compress: compress}, WALOptions{CompactThresholdBytes: 1 << 30})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.SaveSnapshot(expected); err != nil {
				t.Fatal(err)
			}

			// Apply 40 randomized ReplaceFile / RemoveFile updates to both expected & WAL
			revs := make([]int, 12)
			for step := 0; step < 40; step++ {
				fIdx := rng.Intn(12)
				filePath := fmt.Sprintf("pkg/f%02d.go", fIdx)
				if rng.Intn(5) == 0 && len(expected.GetByFile(filePath)) > 0 {
					expected.RemoveFile(filePath)
					if _, err := store.AppendRemoveFile(filePath); err != nil {
						t.Fatal(err)
					}
				} else {
					revs[fIdx]++
					nodes, edges, sites := makeFilePayload(fIdx, revs[fIdx])
					if _, err := expected.ReplaceFile(filePath, nodes, edges, sites); err != nil {
						t.Fatal(err)
					}
					if _, err := store.AppendReplaceFile(filePath, nodes, edges, sites); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}

			// Reload from snapshot + WAL and verify exact equality
			reloadedStore, got, stats, err := LoadStore(dir, WriteOptions{Compress: compress}, WALOptions{}, LoadOptions{AllowTrailingPartialWAL: false})
			if err != nil {
				t.Fatalf("LoadStore failed: %v", err)
			}
			defer func() { _ = reloadedStore.Close() }()

			if !stats.HasSnapshot || stats.WALFramesApplied != 40 || stats.WALTruncatedTail {
				t.Fatalf("unexpected LoadStats: %+v", stats)
			}
			assertGraphsEqual(t, got, expected)
		})
	}
}

func TestLoadStoreTruncatesTornTailBeforeNewAppends(t *testing.T) {
	dir := t.TempDir()
	g := graph.New()
	f0 := graph.Node{Kind: graph.KindFile, Name: "a.go", File: "a.go", StartLine: 1}
	_, _ = g.AddNode(f0)

	store, err := OpenStore(dir, WriteOptions{Compress: true}, WALOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveSnapshot(g); err != nil {
		t.Fatal(err)
	}

	// Valid WAL frame 1
	f1 := graph.Node{Kind: graph.KindFile, Name: "b.go", File: "b.go", StartLine: 1}
	fn1 := graph.Node{Kind: graph.KindFunction, Name: "F1", QName: "pkg::F1", File: "b.go", StartLine: 5}
	uF1, uFn1 := graph.ComputeNodeUID(f1), graph.ComputeNodeUID(fn1)
	if _, err := store.AppendReplaceFile("b.go", []graph.Node{f1, fn1}, []graph.Edge{{Source: uF1, Target: uFn1, Kind: graph.EdgeContains, Tier: graph.TierStructural}}, nil); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	// Append torn trailing crash bytes directly to index.wal
	walData := mustReadFile(t, filepath.Join(dir, WALFileName))
	corrupted := append(bytes.Clone(walData), 0x20, 0x00, 0x00, 0x00, 0x01, 0xAA, 0xBB)
	mustWriteFile(t, filepath.Join(dir, WALFileName), corrupted)

	// LoadStore with AllowTrailingPartialWAL=true must recover frame 1 AND truncate torn tail
	store2, g2, stats, err := LoadStore(dir, WriteOptions{Compress: true}, WALOptions{}, LoadOptions{AllowTrailingPartialWAL: true})
	if err != nil {
		t.Fatalf("LoadStore crash recovery failed: %v", err)
	}
	if !stats.WALTruncatedTail || stats.WALFramesApplied != 1 {
		t.Fatalf("expected WALTruncatedTail=true & WALFramesApplied=1, got %+v", stats)
	}
	if !g2.HasNode(uFn1) {
		t.Fatal("missing node from frame 1 after recovery")
	}

	// Now append frame 2 after recovery — must NOT sit behind old torn bytes!
	f2 := graph.Node{Kind: graph.KindFile, Name: "c.go", File: "c.go", StartLine: 1}
	fn2 := graph.Node{Kind: graph.KindFunction, Name: "F2", QName: "pkg::F2", File: "c.go", StartLine: 5}
	uF2, uFn2 := graph.ComputeNodeUID(f2), graph.ComputeNodeUID(fn2)
	if _, err := store2.AppendReplaceFile("c.go", []graph.Node{f2, fn2}, []graph.Edge{{Source: uF2, Target: uFn2, Kind: graph.EdgeContains, Tier: graph.TierStructural}}, nil); err != nil {
		t.Fatal(err)
	}
	_ = store2.Close()

	// Third startup load in STRICT mode (AllowTrailingPartialWAL=false) must succeed with BOTH frames!
	store3, g3, stats3, err := LoadStore(dir, WriteOptions{}, WALOptions{}, LoadOptions{AllowTrailingPartialWAL: false})
	if err != nil {
		t.Fatalf("strict LoadStore after post-recovery append failed: %v", err)
	}
	_ = store3.Close()
	if stats3.WALFramesApplied != 2 || stats3.WALTruncatedTail {
		t.Fatalf("expected 2 clean frames after restart, got %+v", stats3)
	}
	if !g3.HasNode(uFn1) || !g3.HasNode(uFn2) {
		t.Fatal("missing node from frame 1 or frame 2 after post-recovery restart")
	}
}

func TestLoadStoreEmptyAndCompactAfterReplay(t *testing.T) {
	dir := t.TempDir()

	// Empty dir initializes empty Graph + clean WAL
	store, g, stats, err := LoadStore(dir, WriteOptions{}, WALOptions{CompactThresholdBytes: 32}, LoadOptions{})
	if err != nil {
		t.Fatalf("LoadStore empty dir failed: %v", err)
	}
	if stats.HasSnapshot || g.NodeCount() != 0 {
		t.Fatalf("expected empty graph without snapshot, got %+v", stats)
	}
	f := graph.Node{Kind: graph.KindFile, Name: "m.go", File: "m.go", StartLine: 1}
	_, _ = store.AppendReplaceFile("m.go", []graph.Node{f}, nil, nil)
	_ = store.Close()

	// Reload with CompactAfterReplay=true compacts WAL into base snapshot
	store2, g2, stats2, err := LoadStore(dir, WriteOptions{Compress: true}, WALOptions{CompactThresholdBytes: 32}, LoadOptions{CompactAfterReplay: true})
	if err != nil {
		t.Fatalf("LoadStore CompactAfterReplay failed: %v", err)
	}
	defer func() { _ = store2.Close() }()
	if stats2.WALFramesApplied != 1 || g2.NodeCount() != 1 || store2.wal.Size() != WALHeaderSize {
		t.Fatalf("CompactAfterReplay did not reset WAL cleanly: stats=%+v walSize=%d", stats2, store2.wal.Size())
	}
}

func TestReaderCorruptionAndEdgeErrors(t *testing.T) {
	dir := t.TempDir()
	g, _ := buildSampleGraph(t, false)
	snapPath := filepath.Join(dir, "sample.ggctx")
	if _, err := WriteSnapshot(snapPath, g, WriteOptions{Compress: false}); err != nil {
		t.Fatal(err)
	}
	rawSnap := mustReadFile(t, snapPath)

	// Unsupported flags
	badFlags := bytes.Clone(rawSnap)
	binary.LittleEndian.PutUint32(badFlags[12:16], 1<<5)
	badFlagsPath := filepath.Join(dir, "bad_flags.ggctx")
	mustWriteFile(t, badFlagsPath, badFlags)
	if _, _, err := ReadSnapshot(badFlagsPath); !errors.Is(err, ErrInvalidFormat) {
		t.Fatalf("expected ErrInvalidFormat for unsupported flags, got %v", err)
	}

	// Trailing extra bytes past declared PayloadSize
	extraBytesPath := filepath.Join(dir, "extra.ggctx")
	mustWriteFile(t, extraBytesPath, append(bytes.Clone(rawSnap), 0x01, 0x02))
	if _, _, err := ReadSnapshot(extraBytesPath); !errors.Is(err, ErrInvalidFormat) {
		t.Fatalf("expected ErrInvalidFormat for size mismatch, got %v", err)
	}

	// Empty paths & missing files
	if _, _, err := ReadSnapshot(""); !errors.Is(err, ErrInvalidFormat) {
		t.Fatalf("expected ErrInvalidFormat on empty ReadSnapshot path, got %v", err)
	}
	if _, err := ReplayWAL("", g, true); !errors.Is(err, ErrInvalidFormat) {
		t.Fatalf("expected ErrInvalidFormat on empty ReplayWAL path, got %v", err)
	}
	if _, err := ReplayWAL(filepath.Join(dir, "a.wal"), nil, true); !errors.Is(err, ErrInvalidFormat) {
		t.Fatalf("expected ErrInvalidFormat on nil Graph, got %v", err)
	}
	if _, _, _, err := LoadStore("", WriteOptions{}, WALOptions{}, LoadOptions{}); !errors.Is(err, ErrInvalidFormat) {
		t.Fatalf("expected ErrInvalidFormat on empty dir, got %v", err)
	}

	// Zstd snapshot bit-flip returns ErrChecksumMismatch even if zstd decode errors
	zstdPath := filepath.Join(dir, "sample_zstd.ggctx")
	if _, err := WriteSnapshot(zstdPath, g, WriteOptions{Compress: true}); err != nil {
		t.Fatal(err)
	}
	zBytes := mustReadFile(t, zstdPath)
	zCorrupt := bytes.Clone(zBytes)
	zCorrupt[SnapshotHeaderSize+2] ^= 0xFF
	zCorruptPath := filepath.Join(dir, "corrupt_zstd.ggctx")
	mustWriteFile(t, zCorruptPath, zCorrupt)
	if _, _, err := ReadSnapshot(zCorruptPath); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("expected ErrChecksumMismatch on corrupted zstd payload, got %v", err)
	}

	// Corrupted WAL CRC in middle vs allowTrailingPartial
	walPath := filepath.Join(dir, "bad_crc.wal")
	wal, err := OpenWAL(walPath, WALOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = wal.AppendRemoveFile("pkg/service.go")
	_ = wal.Close()
	walRaw := mustReadFile(t, walPath)
	walRaw[len(walRaw)-1] ^= 0xFF
	mustWriteFile(t, walPath, walRaw)

	if _, err := ReplayWAL(walPath, g, false); !errors.Is(err, ErrCorruptWAL) {
		t.Fatalf("expected ErrCorruptWAL on strict CRC failure, got %v", err)
	}
	res, err := ReplayWAL(walPath, g, true)
	if err != nil || !res.TruncatedTail || res.FramesApplied != 0 {
		t.Fatalf("expected clean trailing CRC stop, got (%+v, %v)", res, err)
	}

	// Checksum-valid snapshot containing invalid payload (empty string table) returns ErrInvalidFormat
	var badPayload []byte
	badPayload = append(badPayload, 0) // strCount = 0 -> invalid
	sum := sha256.Sum256(badPayload)
	hdr := (SnapshotHeader{
		Magic:       SnapshotMagic,
		Version:     FormatVersion,
		PayloadSize: uint64(len(badPayload)),
		Checksum:    sum,
	}).MarshalBinary()
	badPayloadPath := filepath.Join(dir, "bad_payload.ggctx")
	mustWriteFile(t, badPayloadPath, append(hdr[:], badPayload...))
	if _, _, err := ReadSnapshot(badPayloadPath); !errors.Is(err, ErrInvalidFormat) {
		t.Fatalf("expected ErrInvalidFormat on invalid decoded payload, got %v", err)
	}

	_ = crc32.Size
}

func FuzzSnapshotAndWALDecode(f *testing.F) {
	// Seed corpus with minimal valid raw snapshot payload & valid WAL frame
	dir := f.TempDir()
	g := graph.New()
	_, _ = g.AddNode(graph.Node{Kind: graph.KindFile, Name: "a.go", File: "a.go", StartLine: 1})
	snapPath := filepath.Join(dir, "seed.ggctx")
	_, _ = WriteSnapshot(snapPath, g, WriteOptions{Compress: false})
	if raw, err := os.ReadFile(snapPath); err == nil {
		f.Add(raw)
	}
	f.Add([]byte("GGCTX\x00\x00\x01garbage"))
	f.Add([]byte("GGCWAL\x00\x01"))

	f.Fuzz(func(t *testing.T, data []byte) {
		fDir := t.TempDir()
		sPath := filepath.Join(fDir, "fuzz.ggctx")
		wPath := filepath.Join(fDir, "fuzz.wal")
		if err := os.WriteFile(sPath, data, 0o644); err == nil {
			_, _, _ = ReadSnapshot(sPath)
		}
		if err := os.WriteFile(wPath, data, 0o644); err == nil {
			_, _ = ReplayWAL(wPath, graph.New(), true)
			_, _ = ReplayWAL(wPath, graph.New(), false)
		}
	})
}
