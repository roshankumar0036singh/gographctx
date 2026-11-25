package storage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"

	"github.com/roshankumar0036singh/gographctx/internal/graph"
)

func buildSampleGraph(t *testing.T, reverseInsert bool) (*graph.Graph, map[string]graph.UID) {
	t.Helper()
	g := graph.New()

	sig := graph.ComputeSignatureHash([]graph.Param{{Name: "ctx", Type: "Context"}, {Name: "id", Type: "int64"}}, "error")
	nodes := []graph.Node{
		{
			Kind:      graph.KindFile,
			Name:      "service.go",
			File:      "pkg/service.go",
			StartLine: 1,
			Language:  "go",
		},
		{
			Kind:          graph.KindFunction,
			Name:          "Save",
			QName:         "pkg::Service::Save",
			Occurrence:    0,
			SignatureHash: sig,
			File:          "pkg/service.go",
			StartLine:     12,
			EndLine:       40,
			Language:      "go",
			Receiver:      "*Service",
			Exported:      true,
			Params:        []graph.Param{{Name: "ctx", Type: "Context"}, {Name: "id", Type: "int64"}},
			ReturnType:    "error",
			Decorators:    []string{"@trace"},
			Docstring:     "Save persists a record.",
			Complexity:    7,
			Bases:         []string{"BaseService"},
		},
		{
			Kind:       graph.KindFunction,
			Name:       "init",
			QName:      "pkg::init",
			Occurrence: 1,
			File:       "pkg/service.go",
			StartLine:  5,
			EndLine:    10,
			Language:   "go",
		},
		{
			Kind:  graph.KindExternal,
			Name:  "Errorf",
			QName: "fmt.Errorf",
		},
	}

	if reverseInsert {
		for i := len(nodes) - 1; i >= 0; i-- {
			if _, err := g.AddNode(nodes[i]); err != nil {
				t.Fatalf("AddNode failed: %v", err)
			}
		}
	} else {
		for _, n := range nodes {
			if _, err := g.AddNode(n); err != nil {
				t.Fatalf("AddNode failed: %v", err)
			}
		}
	}

	uFile := graph.ComputeNodeUID(nodes[0])
	uSave := graph.ComputeNodeUID(nodes[1])
	uInit := graph.ComputeNodeUID(nodes[2])
	uExt := graph.ComputeNodeUID(nodes[3])

	edges := []graph.Edge{
		{Source: uFile, Target: uSave, Kind: graph.EdgeContains, Tier: graph.TierStructural},
		{Source: uFile, Target: uInit, Kind: graph.EdgeContains, Tier: graph.TierStructural},
		{
			Source: uSave,
			Target: uExt,
			Kind:   graph.EdgeCalls,
			Tier:   graph.TierQualifiedImport,
			Label:  "qualified import",
			Sites: []graph.CallSiteRef{
				{Line: 20, Column: 8, Tier: graph.TierQualifiedImport, Label: "err check"},
				{Line: 34, Column: 10, Tier: graph.TierQualifiedImport, Label: "wrap"},
			},
		},
	}
	for _, e := range edges {
		if err := g.AddEdge(e); err != nil {
			t.Fatalf("AddEdge failed: %v", err)
		}
	}

	if err := g.AddCallSite(graph.CallSite{
		Caller:     uSave,
		File:       "pkg/service.go",
		Line:       28,
		Column:     12,
		CalleeExpr: "repo.Flush()",
		LookupKey:  "Flush",
		Candidates: []graph.UID{uInit, uExt},
		Status:     graph.CallStatusAmbiguous,
		Tier:       graph.TierTypeNarrowed,
		Reason:     "interface has 2 implementations",
	}); err != nil {
		t.Fatalf("AddCallSite failed: %v", err)
	}

	return g, map[string]graph.UID{
		"file": uFile,
		"save": uSave,
		"init": uInit,
		"ext":  uExt,
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile(%q) failed: %v", path, err)
	}
	return b
}

func mustWriteFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("os.WriteFile(%q) failed: %v", path, err)
	}
}

func TestSnapshotDeterminismAndRoundTrip(t *testing.T) {
	dir := t.TempDir()
	g1, uids := buildSampleGraph(t, false)
	g2, _ := buildSampleGraph(t, true) // opposite insertion order

	for _, compress := range []bool{false, true} {
		suffix := "raw"
		if compress {
			suffix = "zstd"
		}
		p1 := filepath.Join(dir, "s1_"+suffix+".ggctx")
		p2 := filepath.Join(dir, "s2_"+suffix+".ggctx")

		st1, err := WriteSnapshot(p1, g1, WriteOptions{Compress: compress, Sync: true})
		if err != nil {
			t.Fatalf("WriteSnapshot(%s) failed: %v", suffix, err)
		}
		st2, err := WriteSnapshot(p2, g2, WriteOptions{Compress: compress, Sync: false})
		if err != nil {
			t.Fatalf("WriteSnapshot(%s, reversed) failed: %v", suffix, err)
		}

		if st1.Checksum != st2.Checksum {
			t.Fatalf("%s snapshots not deterministic: checksums differ", suffix)
		}
		b1 := mustReadFile(t, p1)
		b2 := mustReadFile(t, p2)
		if !bytes.Equal(b1, b2) {
			t.Fatalf("%s snapshots not byte-for-byte deterministic", suffix)
		}

		loaded, hdr, err := ReadSnapshotDecode(p1)
		if err != nil {
			t.Fatalf("ReadSnapshotDecode(%s) failed: %v", suffix, err)
		}
		if err := loaded.VerifyIndexConsistency(); err != nil {
			t.Fatalf("loaded graph index consistency failed: %v", err)
		}
		if hdr.NodeCount != 4 || hdr.EdgeCount != 3 || hdr.CallSiteCount != 1 {
			t.Fatalf("unexpected decoded header counts: %+v", hdr)
		}

		gotSave, ok := loaded.GetNode(uids["save"])
		if !ok || gotSave.Complexity != 7 || !gotSave.Exported || gotSave.Docstring != "Save persists a record." || len(gotSave.Params) != 2 || gotSave.SignatureHash.IsZero() {
			t.Fatalf("decoded node fidelity mismatch: %+v", gotSave)
		}
		gotExt, ok := loaded.GetNode(uids["ext"])
		if !ok || gotExt.Kind != graph.KindExternal || gotExt.File != "" {
			t.Fatalf("KindExternal node polluted or missing: %+v", gotExt)
		}
		gotEdge, ok := loaded.GetEdge(uids["save"], uids["ext"], graph.EdgeCalls)
		if !ok || len(gotEdge.Sites) != 2 || gotEdge.Sites[1].Line != 34 || gotEdge.Tier != graph.TierQualifiedImport {
			t.Fatalf("decoded multi-site edge mismatch: %+v", gotEdge)
		}
		sites := loaded.GetCallSites()
		if len(sites) != 1 || sites[0].Status != graph.CallStatusAmbiguous || len(sites[0].Candidates) != 2 {
			t.Fatalf("decoded CallSites mismatch: %+v", sites)
		}
	}
}

func TestSnapshotCorruptionAndAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.ggctx")
	g, _ := buildSampleGraph(t, false)

	if _, err := WriteSnapshot("", g, WriteOptions{}); !errors.Is(err, ErrInvalidFormat) {
		t.Fatalf("expected ErrInvalidFormat on empty path, got %v", err)
	}

	if _, err := WriteSnapshot(path, g, WriteOptions{Sync: true}); err != nil {
		t.Fatalf("initial WriteSnapshot failed: %v", err)
	}
	origBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Corrupt 1 payload byte -> SHA-256 mismatch
	corruptPath := filepath.Join(dir, "corrupt_payload.ggctx")
	corrupt := bytes.Clone(origBytes)
	corrupt[len(corrupt)-1] ^= 0xFF
	mustWriteFile(t, corruptPath, corrupt)
	if _, _, err := ReadSnapshotDecode(corruptPath); !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("expected ErrChecksumMismatch, got %v", err)
	}

	// Bad magic -> ErrInvalidFormat
	badMagic := bytes.Clone(origBytes)
	badMagic[0] = 'X'
	badMagicPath := filepath.Join(dir, "bad_magic.ggctx")
	mustWriteFile(t, badMagicPath, badMagic)
	if _, _, err := ReadSnapshotDecode(badMagicPath); !errors.Is(err, ErrInvalidFormat) {
		t.Fatalf("expected ErrInvalidFormat for bad magic, got %v", err)
	}

	// Version mismatch -> ErrVersionMismatch
	badVer := bytes.Clone(origBytes)
	binary.LittleEndian.PutUint32(badVer[8:12], 99)
	badVerPath := filepath.Join(dir, "bad_ver.ggctx")
	mustWriteFile(t, badVerPath, badVer)
	if _, _, err := ReadSnapshotDecode(badVerPath); !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("expected ErrVersionMismatch, got %v", err)
	}

	// Truncated header -> ErrInvalidFormat
	truncPath := filepath.Join(dir, "trunc.ggctx")
	mustWriteFile(t, truncPath, origBytes[:20])
	if _, _, err := ReadSnapshotDecode(truncPath); !errors.Is(err, ErrInvalidFormat) {
		t.Fatalf("expected ErrInvalidFormat for truncated header, got %v", err)
	}

	// Verify no leftover temp files after WriteSnapshot
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("os.ReadDir(%q) failed: %v", dir, err)
	}
	for _, e := range entries {
		if len(e.Name()) > 10 && e.Name()[:10] == ".ggctx-tmp" {
			t.Fatalf("leftover temp file found: %s", e.Name())
		}
	}
}

func TestWALAppendReplayCrashRecoveryAndCompaction(t *testing.T) {
	dir := t.TempDir()
	g, uids := buildSampleGraph(t, false)

	store, err := OpenStore(dir, WriteOptions{Compress: true, Sync: true}, WALOptions{
		Sync:                  false,
		CompactThresholdBytes: 128,
	})
	if err != nil {
		t.Fatalf("OpenStore failed: %v", err)
	}
	defer func() { _ = store.Close() }()

	if _, err := store.SaveSnapshot(g); err != nil {
		t.Fatalf("SaveSnapshot failed: %v", err)
	}

	// Incremental ReplaceFile delta written to WAL
	newFileNode := graph.Node{Kind: graph.KindFile, Name: "extra.go", File: "pkg/extra.go", StartLine: 1, Language: "go"}
	newFn := graph.Node{Kind: graph.KindFunction, Name: "Helper", QName: "pkg::Helper", File: "pkg/extra.go", StartLine: 5, Language: "go"}
	uExtra := graph.ComputeNodeUID(newFileNode)
	uHelper := graph.ComputeNodeUID(newFn)
	extraEdges := []graph.Edge{
		{Source: uExtra, Target: uHelper, Kind: graph.EdgeContains, Tier: graph.TierStructural},
		{Source: uHelper, Target: uids["save"], Kind: graph.EdgeCalls, Tier: graph.TierSamePackage, Sites: []graph.CallSiteRef{{Line: 9}}},
	}
	if _, err := store.AppendReplaceFile("pkg/extra.go", []graph.Node{newFileNode, newFn}, extraEdges, nil); err != nil {
		t.Fatalf("AppendReplaceFile failed: %v", err)
	}

	// Another WAL delta removing pkg/extra.go? First test replay with extra.go present, then add remove frame.
	if !store.NeedsCompaction() {
		t.Fatal("expected NeedsCompaction() == true above 128-byte threshold")
	}

	// Simulate crash by appending a partial/torn WAL frame at the end of index.wal
	_ = store.Close()
	walBytes, err := os.ReadFile(store.WALPath())
	if err != nil {
		t.Fatal(err)
	}
	torn := append(bytes.Clone(walBytes), 50, 0, 0, 0, 1, 2, 3) // declares len=50, ends after 3 bytes
	tornPath := filepath.Join(dir, "torn.wal")
	mustWriteFile(t, tornPath, torn)

	// Strict replay fails with ErrCorruptWAL; crash-safe replay recovers valid frame
	gStrict, _, err := ReadSnapshotDecode(store.SnapshotPath())
	if err != nil {
		t.Fatalf("ReadSnapshotDecode failed: %v", err)
	}
	if _, err := ReplayWALDecode(tornPath, gStrict, false); !errors.Is(err, ErrCorruptWAL) {
		t.Fatalf("expected ErrCorruptWAL on strict replay of torn WAL, got %v", err)
	}

	gRecovered, _, err := ReadSnapshotDecode(store.SnapshotPath())
	if err != nil {
		t.Fatalf("ReadSnapshotDecode failed: %v", err)
	}
	applied, err := ReplayWALDecode(tornPath, gRecovered, true)
	if err != nil || applied != 1 {
		t.Fatalf("crash-safe WAL replay = (%d, %v), want (1, nil)", applied, err)
	}
	if !gRecovered.HasNode(uHelper) || !gRecovered.HasEdge(uHelper, uids["save"], graph.EdgeCalls) {
		t.Fatal("recovered graph missing delta from valid WAL frame")
	}

	// Re-open store, apply Replace + Remove to live graph & compact cleanly
	store2, err := OpenStore(dir, WriteOptions{Compress: true}, WALOptions{CompactThresholdBytes: 1 << 20})
	if err != nil {
		t.Fatalf("re-OpenStore failed: %v", err)
	}
	defer func() { _ = store2.Close() }()

	_, _ = g.ReplaceFile("pkg/extra.go", []graph.Node{newFileNode, newFn}, extraEdges, nil)
	if _, err := store2.AppendRemoveFile("pkg/service.go"); err != nil {
		t.Fatalf("AppendRemoveFile failed: %v", err)
	}
	g.RemoveFile("pkg/service.go")

	if _, err := store2.Compact(g); err != nil {
		t.Fatalf("Compact failed: %v", err)
	}
	if store2.wal.Size() != WALHeaderSize {
		t.Fatalf("expected reset WAL size %d after compaction, got %d", WALHeaderSize, store2.wal.Size())
	}

	loadedAfterCompact, _, err := ReadSnapshotDecode(store2.SnapshotPath())
	if err != nil {
		t.Fatalf("ReadSnapshotDecode after Compact failed: %v", err)
	}
	if loadedAfterCompact.HasNode(uids["save"]) || !loadedAfterCompact.HasNode(uHelper) {
		t.Fatal("compacted snapshot did not reflect latest state")
	}

	// Closed store/WAL rejects operations cleanly
	_ = store2.Close()
	if _, err := store2.AppendRemoveFile("x.go"); !errors.Is(err, ErrClosed) {
		t.Fatalf("expected ErrClosed on closed Store, got %v", err)
	}
}

func TestCorruptCountsAndWALOverflowGuards(t *testing.T) {
	dir := t.TempDir()
	hdr := (WALHeader{Magic: WALMagic, Version: FormatVersion}).MarshalBinary()

	// 1. Huge frameLen (0xffffffff) must not overflow 32-bit math
	hugeLenPath := filepath.Join(dir, "huge_len.wal")
	hugeData := append(bytes.Clone(hdr[:]), 0xff, 0xff, 0xff, 0xff, 1, 2, 3, 4)
	mustWriteFile(t, hugeLenPath, hugeData)
	if _, err := ReplayWALDecode(hugeLenPath, graph.New(), false); !errors.Is(err, ErrCorruptWAL) {
		t.Fatalf("expected ErrCorruptWAL on huge frameLen, got %v", err)
	}
	if n, err := ReplayWALDecode(hugeLenPath, graph.New(), true); err != nil || n != 0 {
		t.Fatalf("expected clean partial stop on huge frameLen, got (%d, %v)", n, err)
	}

	// 2. Zero-length WAL frame: strict returns ErrCorruptWAL; crash-safe stops cleanly
	zeroLenPath := filepath.Join(dir, "zero_len.wal")
	zeroData := append(bytes.Clone(hdr[:]), 0, 0, 0, 0)
	mustWriteFile(t, zeroLenPath, zeroData)
	if _, err := ReplayWALDecode(zeroLenPath, graph.New(), false); !errors.Is(err, ErrCorruptWAL) {
		t.Fatalf("expected ErrCorruptWAL on strict zero-length frame, got %v", err)
	}
	if n, err := ReplayWALDecode(zeroLenPath, graph.New(), true); err != nil || n != 0 {
		t.Fatalf("expected clean crash-safe stop on trailing zero-length frame, got (%d, %v)", n, err)
	}

	// 3. Adversarial huge nodeCount in CRC-valid WAL frame must fail via checkCount before allocating
	var body binWriter
	body.writeU8(WALOpReplaceFile)
	body.writeUvarint(1) // strCount = 1
	body.writeString("")
	body.writeString("evil.go")
	body.writeUvarint(1 << 40) // nc = 1 trillion nodes inside tiny body!
	rawBody := body.buf.Bytes()

	var frame bytes.Buffer
	_, _ = frame.Write(hdr[:])
	var u32 [4]byte
	binary.LittleEndian.PutUint32(u32[:], uint32(len(rawBody)))
	_, _ = frame.Write(u32[:])
	_, _ = frame.Write(rawBody)
	binary.LittleEndian.PutUint32(u32[:], crc32.Checksum(rawBody, crc32cTable))
	_, _ = frame.Write(u32[:])

	evilWAL := filepath.Join(dir, "evil_count.wal")
	mustWriteFile(t, evilWAL, frame.Bytes())
	if _, err := ReplayWALDecode(evilWAL, graph.New(), false); !errors.Is(err, ErrCorruptWAL) || !errors.Is(err, ErrInvalidFormat) {
		t.Fatalf("expected ErrCorruptWAL/ErrInvalidFormat on bogus huge node count, got %v", err)
	}

	// 4. Adversarial huge snapshot nodeCount must fail before allocating
	var tinyPayload binWriter
	tinyPayload.writeUvarint(1)
	tinyPayload.writeString("")
	if _, err := decodeSnapshotPayload(tinyPayload.buf.Bytes(), 1<<40, 0, 0); !errors.Is(err, ErrInvalidFormat) {
		t.Fatalf("expected ErrInvalidFormat on bogus huge snapshot nodeCount, got %v", err)
	}
}
