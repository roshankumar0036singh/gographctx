package storage

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/roshankumar0036singh/gographctx/internal/graph"
)

// WALReplayResult summarizes a WAL replay pass.
type WALReplayResult struct {
	FramesApplied   int
	LastValidOffset int64
	BytesReplayed   int64
	TruncatedTail   bool
}

// LoadOptions configures startup reconstruction in LoadStore.
type LoadOptions struct {
	AllowTrailingPartialWAL bool // Truncate and recover cleanly if a crash left a torn trailing WAL frame
	CompactAfterReplay      bool // Compact WAL into a fresh base snapshot if WAL exceeds threshold
}

// LoadStats reports startup snapshot + WAL reconstruction metrics.
type LoadStats struct {
	HasSnapshot      bool
	SnapshotHeader   SnapshotHeader
	WALFramesApplied int
	WALTruncatedTail bool
	WALBytesReplayed int64
	DurationMs       float64
}

// ReadSnapshot validates header, checksum, and flags, then streams nodes, dense edges,
// and call sites directly into a pre-sized Graph.
func ReadSnapshot(path string) (*graph.Graph, SnapshotHeader, error) {
	if strings.TrimSpace(path) == "" {
		return nil, SnapshotHeader{}, fmt.Errorf("%w: empty snapshot path", ErrInvalidFormat)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, SnapshotHeader{}, err
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		return nil, SnapshotHeader{}, err
	}
	if fi.Size() < SnapshotHeaderSize {
		return nil, SnapshotHeader{}, fmt.Errorf("%w: snapshot truncated (%d < %d bytes)", ErrInvalidFormat, fi.Size(), SnapshotHeaderSize)
	}

	br := bufio.NewReaderSize(f, 256*1024)
	var hdrBuf [SnapshotHeaderSize]byte
	if _, err := io.ReadFull(br, hdrBuf[:]); err != nil {
		return nil, SnapshotHeader{}, fmt.Errorf("%w: read snapshot header: %w", ErrInvalidFormat, err)
	}
	hdr, err := UnmarshalSnapshotHeader(hdrBuf[:])
	if err != nil {
		return nil, SnapshotHeader{}, err
	}
	if hdr.Flags&^FlagZstd != 0 {
		return nil, SnapshotHeader{}, fmt.Errorf("%w: unsupported snapshot flags %#x", ErrInvalidFormat, hdr.Flags)
	}
	if hdr.PayloadSize > math.MaxInt64 {
		return nil, SnapshotHeader{}, fmt.Errorf("%w: payload size %d exceeds max int64", ErrInvalidFormat, hdr.PayloadSize)
	}

	expectedEnd := uint64(SnapshotHeaderSize) + hdr.PayloadSize
	if uint64(fi.Size()) != expectedEnd {
		return nil, SnapshotHeader{}, fmt.Errorf("%w: snapshot size %d != expected %d", ErrInvalidFormat, fi.Size(), expectedEnd)
	}

	lr := io.LimitReader(br, int64(hdr.PayloadSize))
	hasher := sha256.New()
	tee := io.TeeReader(lr, hasher)

	var sr *streamReader
	var zdec *zstd.Decoder
	if hdr.Flags&FlagZstd != 0 {
		zdec, err = zstd.NewReader(tee)
		if err != nil {
			return nil, SnapshotHeader{}, fmt.Errorf("create zstd decoder: %w", err)
		}
		defer zdec.Close()
		sr = newStreamReader(zdec, 0, false)
	} else {
		sr = newStreamReader(tee, hdr.PayloadSize, true)
	}

	g, decErr := decodeSnapshotFromReader(sr, hdr.NodeCount, hdr.EdgeCount, hdr.CallSiteCount)
	if decErr != nil {
		// Drain remaining bounded payload to distinguish checksum corruption from structural errors.
		if _, err := io.Copy(io.Discard, lr); err == nil {
			var gotSum [32]byte
			copy(gotSum[:], hasher.Sum(nil))
			if gotSum != hdr.Checksum {
				return nil, SnapshotHeader{}, fmt.Errorf("%w: snapshot SHA-256 mismatch", ErrChecksumMismatch)
			}
		}
		return nil, SnapshotHeader{}, decErr
	}

	// Ensure compressed/uncompressed payload stream reached clean EOF without trailing garbage.
	if zdec != nil {
		var extra [1]byte
		n, err := sr.br.Read(extra[:])
		if n > 0 || (err != nil && !errors.Is(err, io.EOF)) {
			// Check SHA-256 first in case bit-flip corrupted the compressed stream.
			_, _ = io.Copy(io.Discard, lr)
			var gotSum [32]byte
			copy(gotSum[:], hasher.Sum(nil))
			if gotSum != hdr.Checksum {
				return nil, SnapshotHeader{}, fmt.Errorf("%w: snapshot SHA-256 mismatch", ErrChecksumMismatch)
			}
			return nil, SnapshotHeader{}, fmt.Errorf("%w: unexpected trailing data or zstd stream error: %v", ErrInvalidFormat, err)
		}
	}

	// Drain any remaining zstd epilogue bytes so SHA-256 hashes the complete stored payload.
	if _, err := io.Copy(io.Discard, lr); err != nil {
		return nil, SnapshotHeader{}, fmt.Errorf("%w: drain snapshot payload: %w", ErrInvalidFormat, err)
	}
	var gotSum [32]byte
	copy(gotSum[:], hasher.Sum(nil))
	if gotSum != hdr.Checksum {
		return nil, SnapshotHeader{}, fmt.Errorf("%w: snapshot SHA-256 mismatch", ErrChecksumMismatch)
	}
	if hdr.Flags&FlagZstd == 0 && sr.remBytes != 0 {
		return nil, SnapshotHeader{}, fmt.Errorf("%w: %d trailing unread bytes in snapshot payload", ErrInvalidFormat, sr.remBytes)
	}

	return g, hdr, nil
}

func decodeSnapshotFromReader(r recordReader, nodeCount, edgeCount, siteCount uint64) (*graph.Graph, error) {
	strCount, err := r.readUvarint()
	if err != nil {
		return nil, err
	}
	if strCount == 0 {
		return nil, fmt.Errorf("%w: empty string table", ErrInvalidFormat)
	}
	if err := r.checkCount(strCount, 1, "string table"); err != nil {
		return nil, err
	}
	tbl := make([]string, 0, r.safePreallocCap(strCount))
	for i := uint64(0); i < strCount; i++ {
		s, err := r.readString()
		if err != nil {
			return nil, err
		}
		tbl = append(tbl, s)
	}

	if err := r.checkCount(nodeCount, 48, "snapshot node"); err != nil {
		return nil, err
	}
	nodes := make([]graph.Node, 0, r.safePreallocCap(nodeCount))
	for i := uint64(0); i < nodeCount; i++ {
		n, err := readNodeRecord(r, tbl)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}

	if err := r.checkCount(edgeCount, 6, "snapshot edge"); err != nil {
		return nil, err
	}
	edges := make([]graph.DenseEdge, 0, r.safePreallocCap(edgeCount))
	for i := uint64(0); i < edgeCount; i++ {
		srcIdx, err := r.readUvarint()
		if err != nil {
			return nil, err
		}
		tgtIdx, err := r.readUvarint()
		if err != nil {
			return nil, err
		}
		if srcIdx >= nodeCount || tgtIdx >= nodeCount {
			return nil, fmt.Errorf("%w: edge node index out of range (%d, %d >= %d)", ErrInvalidFormat, srcIdx, tgtIdx, nodeCount)
		}
		kind, err := r.readU8()
		if err != nil {
			return nil, err
		}
		tier, err := r.readU8()
		if err != nil {
			return nil, err
		}
		lbl, err := r.lookupTableString(tbl)
		if err != nil {
			return nil, err
		}
		de := graph.DenseEdge{
			SourceIdx: uint32(srcIdx),
			TargetIdx: uint32(tgtIdx),
			Kind:      graph.EdgeKind(kind),
			Tier:      graph.ResolutionTier(tier),
			Label:     lbl,
		}
		if err := readDenseEdgeSites(r, tbl, &de); err != nil {
			return nil, err
		}
		edges = append(edges, de)
	}

	if err := r.checkCount(siteCount, 26, "snapshot call site"); err != nil {
		return nil, err
	}
	sites := make([]graph.CallSite, 0, r.safePreallocCap(siteCount))
	for i := uint64(0); i < siteCount; i++ {
		cs, err := readCallSiteRecord(r, tbl)
		if err != nil {
			return nil, err
		}
		sites = append(sites, cs)
	}

	g := &graph.Graph{}
	if err := g.LoadDense(nodes, edges, sites); err != nil {
		return nil, err
	}
	return g, nil
}

// ReplayWAL streams CRC-32C-verified WAL frames sequentially onto g.
// When allowTrailingPartial is true, torn/zeroed trailing incomplete frames stop replay
// cleanly and set result.TruncatedTail = true; otherwise ErrCorruptWAL is returned.
func ReplayWAL(path string, g *graph.Graph, allowTrailingPartial bool) (WALReplayResult, error) {
	if strings.TrimSpace(path) == "" {
		return WALReplayResult{}, fmt.Errorf("%w: empty WAL path", ErrInvalidFormat)
	}
	if g == nil {
		return WALReplayResult{}, fmt.Errorf("%w: nil target graph", ErrInvalidFormat)
	}

	f, err := os.Open(path)
	if err != nil {
		return WALReplayResult{}, err
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		return WALReplayResult{}, err
	}
	fileSize := fi.Size()

	br := bufio.NewReaderSize(f, 64*1024)
	var hdrBuf [WALHeaderSize]byte
	hdrN, err := io.ReadFull(br, hdrBuf[:])
	if err != nil {
		if allowTrailingPartial && (errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)) && isPartialWALMagic(hdrBuf[:hdrN]) {
			return WALReplayResult{
				LastValidOffset: 0,
				TruncatedTail:   true,
			}, nil
		}
		return WALReplayResult{}, fmt.Errorf("%w: WAL header truncated: %w", ErrInvalidFormat, err)
	}
	if _, err := UnmarshalWALHeader(hdrBuf[:]); err != nil {
		return WALReplayResult{}, err
	}

	res := WALReplayResult{
		LastValidOffset: WALHeaderSize,
	}
	scratch := make([]byte, 0, 4096)

	for {
		var lenBuf [4]byte
		n, err := io.ReadFull(br, lenBuf[:])
		if err != nil {
			if errors.Is(err, io.EOF) && n == 0 {
				break
			}
			if (errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)) && allowTrailingPartial {
				res.TruncatedTail = true
				break
			}
			return res, fmt.Errorf("%w: truncated frame length at offset %d", ErrCorruptWAL, res.LastValidOffset)
		}

		frameLen := binary.LittleEndian.Uint32(lenBuf[:])
		if frameLen == 0 {
			if allowTrailingPartial {
				res.TruncatedTail = true
				break
			}
			return res, fmt.Errorf("%w: zero-length WAL frame at offset %d", ErrCorruptWAL, res.LastValidOffset)
		}

		rem := uint64(fileSize - (res.LastValidOffset + 4))
		need := uint64(frameLen) + 4
		if need > rem {
			if allowTrailingPartial {
				res.TruncatedTail = true
				break
			}
			return res, fmt.Errorf("%w: truncated frame body at offset %d (need %d, have %d)", ErrCorruptWAL, res.LastValidOffset, need, rem)
		}

		if cap(scratch) < int(need) {
			scratch = make([]byte, need)
		} else {
			scratch = scratch[:need]
		}
		if _, err := io.ReadFull(br, scratch); err != nil {
			if (errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)) && allowTrailingPartial {
				res.TruncatedTail = true
				break
			}
			return res, fmt.Errorf("%w: read frame body at offset %d: %w", ErrCorruptWAL, res.LastValidOffset, err)
		}

		body := scratch[:frameLen]
		gotCRC := binary.LittleEndian.Uint32(scratch[frameLen:need])
		if wantCRC := crc32.Checksum(body, crc32cTable); gotCRC != wantCRC {
			if allowTrailingPartial {
				res.TruncatedTail = true
				break
			}
			return res, fmt.Errorf("%w: CRC mismatch at offset %d (got %#x, want %#x)", ErrCorruptWAL, res.LastValidOffset, gotCRC, wantCRC)
		}

		if err := applyWALFrame(body, g); err != nil {
			return res, err
		}

		frameBytes := int64(4) + int64(need)
		res.LastValidOffset += frameBytes
		res.BytesReplayed += frameBytes
		res.FramesApplied++
	}

	return res, nil
}

// LoadStore coordinates startup recovery for a Store directory:
//  1. Loads index.ggctx (if present) into a new Graph,
//  2. Replays index.wal (if present) sequentially,
//  3. Truncates any torn trailing WAL bytes back to LastValidOffset before opening OpenWAL,
//  4. Optionally compacts the WAL if it exceeds the configured threshold.
func LoadStore(dir string, snapOpts WriteOptions, walOpts WALOptions, loadOpts LoadOptions) (*Store, *graph.Graph, LoadStats, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil, LoadStats{}, fmt.Errorf("%w: empty store directory", ErrInvalidFormat)
	}

	start := time.Now()
	var stats LoadStats

	snapPath := filepath.Join(dir, SnapshotFileName)
	walPath := filepath.Join(dir, WALFileName)

	var g *graph.Graph
	if _, err := os.Stat(snapPath); err == nil {
		var hdr SnapshotHeader
		g, hdr, err = ReadSnapshot(snapPath)
		if err != nil {
			return nil, nil, LoadStats{}, err
		}
		stats.HasSnapshot = true
		stats.SnapshotHeader = hdr
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, LoadStats{}, err
	} else {
		g = graph.New()
	}

	if fi, err := os.Stat(walPath); err == nil && fi.Size() > 0 {
		wres, err := ReplayWAL(walPath, g, loadOpts.AllowTrailingPartialWAL)
		if err != nil {
			return nil, nil, LoadStats{}, err
		}
		stats.WALFramesApplied = wres.FramesApplied
		stats.WALTruncatedTail = wres.TruncatedTail
		stats.WALBytesReplayed = wres.BytesReplayed

		// Truncate away torn crash tail before OpenWAL appends future frames.
		if wres.TruncatedTail {
			if err := os.Truncate(walPath, wres.LastValidOffset); err != nil {
				return nil, nil, LoadStats{}, fmt.Errorf("truncate torn WAL tail at %d: %w", wres.LastValidOffset, err)
			}
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, LoadStats{}, err
	}

	store, err := OpenStore(dir, snapOpts, walOpts)
	if err != nil {
		return nil, nil, LoadStats{}, err
	}

	if loadOpts.CompactAfterReplay && store.NeedsCompaction() {
		if _, err := store.Compact(g); err != nil {
			_ = store.Close()
			return nil, nil, LoadStats{}, err
		}
	}

	stats.DurationMs = float64(time.Since(start).Microseconds()) / 1000.0
	return store, g, stats, nil
}

func isPartialWALMagic(partial []byte) bool {
	if len(partial) <= len(WALMagic) {
		return bytes.Equal(partial, WALMagic[:len(partial)])
	}
	return bytes.Equal(partial[:len(WALMagic)], WALMagic[:])
}

func applyWALFrame(body []byte, g *graph.Graph) error {
	r := newBinReader(body)
	op, err := r.readU8()
	if err != nil {
		return fmt.Errorf("%w: %w", ErrCorruptWAL, err)
	}
	switch op {
	case WALOpRemoveFile:
		file, err := r.readString()
		if err != nil {
			return fmt.Errorf("%w: %w", ErrCorruptWAL, err)
		}
		g.RemoveFile(file)
		return nil
	case WALOpReplaceFile:
		strCount, err := r.readUvarint()
		if err != nil {
			return fmt.Errorf("%w: %w", ErrCorruptWAL, err)
		}
		if strCount == 0 {
			return fmt.Errorf("%w: empty WAL string table", ErrCorruptWAL)
		}
		if err := r.checkCount(strCount, 1, "WAL string table"); err != nil {
			return fmt.Errorf("%w: %w", ErrCorruptWAL, err)
		}
		tbl := make([]string, strCount)
		for i := uint64(0); i < strCount; i++ {
			if tbl[i], err = r.readString(); err != nil {
				return fmt.Errorf("%w: %w", ErrCorruptWAL, err)
			}
		}
		file, err := r.readString()
		if err != nil {
			return fmt.Errorf("%w: %w", ErrCorruptWAL, err)
		}
		nc, err := r.readUvarint()
		if err != nil {
			return fmt.Errorf("%w: %w", ErrCorruptWAL, err)
		}
		if err := r.checkCount(nc, 48, "WAL node"); err != nil {
			return fmt.Errorf("%w: %w", ErrCorruptWAL, err)
		}
		nodes := make([]graph.Node, nc)
		for i := uint64(0); i < nc; i++ {
			if nodes[i], err = readNodeRecord(r, tbl); err != nil {
				return fmt.Errorf("%w: %w", ErrCorruptWAL, err)
			}
		}
		ec, err := r.readUvarint()
		if err != nil {
			return fmt.Errorf("%w: %w", ErrCorruptWAL, err)
		}
		if err := r.checkCount(ec, 36, "WAL edge"); err != nil {
			return fmt.Errorf("%w: %w", ErrCorruptWAL, err)
		}
		edges := make([]graph.Edge, ec)
		for i := uint64(0); i < ec; i++ {
			e := &edges[i]
			if err := r.readExact(e.Source[:]); err != nil {
				return fmt.Errorf("%w: %w", ErrCorruptWAL, err)
			}
			if err := r.readExact(e.Target[:]); err != nil {
				return fmt.Errorf("%w: %w", ErrCorruptWAL, err)
			}
			k, err := r.readU8()
			if err != nil {
				return fmt.Errorf("%w: %w", ErrCorruptWAL, err)
			}
			e.Kind = graph.EdgeKind(k)
			t, err := r.readU8()
			if err != nil {
				return fmt.Errorf("%w: %w", ErrCorruptWAL, err)
			}
			e.Tier = graph.ResolutionTier(t)
			if e.Label, err = r.lookupTableString(tbl); err != nil {
				return fmt.Errorf("%w: %w", ErrCorruptWAL, err)
			}
			if e.Sites, err = readEdgeSites(r, tbl); err != nil {
				return fmt.Errorf("%w: %w", ErrCorruptWAL, err)
			}
		}
		sc, err := r.readUvarint()
		if err != nil {
			return fmt.Errorf("%w: %w", ErrCorruptWAL, err)
		}
		if err := r.checkCount(sc, 26, "WAL call site"); err != nil {
			return fmt.Errorf("%w: %w", ErrCorruptWAL, err)
		}
		sites := make([]graph.CallSite, sc)
		for i := uint64(0); i < sc; i++ {
			if sites[i], err = readCallSiteRecord(r, tbl); err != nil {
				return fmt.Errorf("%w: %w", ErrCorruptWAL, err)
			}
		}
		if _, err := g.ReplaceFile(file, nodes, edges, sites); err != nil {
			return err
		}
		return nil
	default:
		return fmt.Errorf("%w: unknown WAL op %d", ErrCorruptWAL, op)
	}
}
