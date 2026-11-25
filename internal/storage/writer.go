package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
	"github.com/roshankumar0036singh/gographctx/internal/graph"
)

const (
	// SnapshotFileName is the default base snapshot filename within a Store directory.
	SnapshotFileName = "index.ggctx"
	// WALFileName is the default append-only WAL filename within a Store directory.
	WALFileName = "index.wal"
	// DefaultWALCompactBytes triggers compaction suggestions once the WAL exceeds 4 MiB.
	DefaultWALCompactBytes int64 = 4 << 20
)

// WriteOptions configures base snapshot serialization.
type WriteOptions struct {
	Compress bool // Compress snapshot payload with zstd
	Sync     bool // Fsync temp file and parent directory (best-effort on dir) before rename
}

// SnapshotStats describes the outcome of writing a base snapshot.
type SnapshotStats struct {
	Path              string
	TotalBytes        int64
	RawPayloadBytes   int64
	StoredPayloadSize int64
	Compressed        bool
	NodeCount         int
	EdgeCount         int
	CallSiteCount     int
	Checksum          [32]byte
}

// WriteSnapshot atomically serializes the graph to path using write-to-temp,
// optional fsync, and os.Rename. Output is deterministic for equal graphs.
func WriteSnapshot(path string, g *graph.Graph, opts WriteOptions) (SnapshotStats, error) {
	if strings.TrimSpace(path) == "" {
		return SnapshotStats{}, fmt.Errorf("%w: empty snapshot path", ErrInvalidFormat)
	}
	if g == nil {
		g = graph.New()
	}

	var (
		nodes []graph.Node
		edges []graph.Edge
		sites []graph.CallSite
	)
	g.View(func(v *graph.ReadView) {
		nodes = v.AllNodes()
		edges = v.AllEdges()
		sites = v.GetCallSites()
	})

	payload, err := encodeSnapshotPayload(nodes, edges, sites)
	if err != nil {
		return SnapshotStats{}, err
	}
	rawLen := int64(len(payload))

	var flags uint32
	if opts.Compress {
		enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
		if err != nil {
			return SnapshotStats{}, fmt.Errorf("create zstd encoder: %w", err)
		}
		payload = enc.EncodeAll(payload, make([]byte, 0, len(payload)/2))
		_ = enc.Close()
		flags |= FlagZstd
	}

	sum := sha256.Sum256(payload)
	header := SnapshotHeader{
		Magic:         SnapshotMagic,
		Version:       FormatVersion,
		Flags:         flags,
		NodeCount:     uint64(len(nodes)),
		EdgeCount:     uint64(len(edges)),
		CallSiteCount: uint64(len(sites)),
		PayloadSize:   uint64(len(payload)),
		Checksum:      sum,
	}
	hdrBytes := header.MarshalBinary()

	if err := atomicWriteFile(path, hdrBytes[:], payload, opts.Sync); err != nil {
		return SnapshotStats{}, err
	}

	return SnapshotStats{
		Path:              path,
		TotalBytes:        int64(SnapshotHeaderSize + len(payload)),
		RawPayloadBytes:   rawLen,
		StoredPayloadSize: int64(len(payload)),
		Compressed:        opts.Compress,
		NodeCount:         len(nodes),
		EdgeCount:         len(edges),
		CallSiteCount:     len(sites),
		Checksum:          sum,
	}, nil
}

func encodeSnapshotPayload(nodes []graph.Node, edges []graph.Edge, sites []graph.CallSite) ([]byte, error) {
	st := newStringTable()
	uidToSnapIdx := make(map[graph.UID]uint64, len(nodes))

	var body binWriter
	// Reserve rough capacity to avoid reallocs during bulk write.
	body.buf.Grow(len(nodes)*64 + len(edges)*14 + len(sites)*48)

	// 1. Encode nodes and build UID -> 0-based dense index map.
	for i := range nodes {
		uidToSnapIdx[nodes[i].UID] = uint64(i)
		writeNodeRecord(&body, &nodes[i], st)
	}

	// 2. Encode edges using dense uvarint node indices.
	for i := range edges {
		e := &edges[i]
		srcIdx, ok := uidToSnapIdx[e.Source]
		if !ok {
			return nil, fmt.Errorf("%w: edge source UID %s missing from snapshot nodes", ErrInvalidFormat, e.Source)
		}
		tgtIdx, ok := uidToSnapIdx[e.Target]
		if !ok {
			return nil, fmt.Errorf("%w: edge target UID %s missing from snapshot nodes", ErrInvalidFormat, e.Target)
		}
		body.writeUvarint(srcIdx)
		body.writeUvarint(tgtIdx)
		body.writeU8(uint8(e.Kind))
		body.writeU8(uint8(e.Tier))
		body.writeUvarint(uint64(st.intern(e.Label)))
		writeEdgeSites(&body, e.Sites, st)
	}

	// 3. Encode CallSites.
	for i := range sites {
		writeCallSiteRecord(&body, &sites[i], st)
	}

	// Assemble final payload: [String Table] || [Nodes/Edges/CallSites body].
	var full binWriter
	full.buf.Grow(len(st.list)*20 + body.buf.Len())
	full.writeUvarint(uint64(len(st.list)))
	for _, s := range st.list {
		full.writeString(s)
	}
	full.writeBytes(body.buf.Bytes())
	return full.buf.Bytes(), nil
}

func atomicWriteFile(path string, prefix, payload []byte, doSync bool) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create storage directory: %w", err)
	}

	tmpFile, err := os.CreateTemp(dir, ".ggctx-tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if len(prefix) > 0 {
		if err := writeAll(tmpFile, prefix); err != nil {
			_ = tmpFile.Close()
			return fmt.Errorf("write header: %w", err)
		}
	}
	if len(payload) > 0 {
		if err := writeAll(tmpFile, payload); err != nil {
			_ = tmpFile.Close()
			return fmt.Errorf("write payload: %w", err)
		}
	}
	if doSync {
		if err := tmpFile.Sync(); err != nil {
			_ = tmpFile.Close()
			return fmt.Errorf("sync temp file: %w", err)
		}
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("atomic rename to %q: %w", path, err)
	}
	cleanup = false

	if doSync {
		if df, err := os.Open(dir); err == nil {
			_ = df.Sync()
			_ = df.Close()
		}
	}
	return nil
}

func writeAll(f *os.File, data []byte) error {
	for len(data) > 0 {
		n, err := f.Write(data)
		if err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

// ---------- Append-Only Incremental WAL ----------

// WALOptions configures append-only WAL writes and compaction threshold.
type WALOptions struct {
	Sync                  bool  // Call fsync after every WAL frame write
	CompactThresholdBytes int64 // Defaults to DefaultWALCompactBytes when <= 0
}

// WALWriter appends O(changed file) CRC-32C-framed records to an index.wal log.
type WALWriter struct {
	mu     sync.Mutex
	path   string
	file   *os.File
	size   int64
	opts   WALOptions
	closed bool
}

// OpenWAL opens an existing WAL (validating its header) or creates a new WAL with header.
func OpenWAL(path string, opts WALOptions) (*WALWriter, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("%w: empty WAL path", ErrInvalidFormat)
	}
	if opts.CompactThresholdBytes <= 0 {
		opts.CompactThresholdBytes = DefaultWALCompactBytes
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create WAL dir: %w", err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open WAL %q: %w", path, err)
	}

	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat WAL %q: %w", path, err)
	}

	size := info.Size()
	if size == 0 {
		hdr := (WALHeader{Magic: WALMagic, Version: FormatVersion}).MarshalBinary()
		if err := writeAll(f, hdr[:]); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("write WAL header: %w", err)
		}
		if opts.Sync {
			if err := f.Sync(); err != nil {
				_ = f.Close()
				return nil, fmt.Errorf("sync WAL header: %w", err)
			}
		}
		size = WALHeaderSize
	} else {
		var hdrBuf [WALHeaderSize]byte
		if _, err := f.ReadAt(hdrBuf[:], 0); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("%w: read WAL header: %w", ErrInvalidFormat, err)
		}
		if _, err := UnmarshalWALHeader(hdrBuf[:]); err != nil {
			_ = f.Close()
			return nil, err
		}
	}

	return &WALWriter{
		path: path,
		file: f,
		size: size,
		opts: opts,
	}, nil
}

// AppendReplaceFile writes an O(file) delta frame representing a ReplaceFile transaction.
func (w *WALWriter) AppendReplaceFile(file string, nodes []graph.Node, edges []graph.Edge, sites []graph.CallSite) (int, error) {
	trimmed := strings.TrimSpace(file)
	if trimmed == "" {
		return 0, fmt.Errorf("%w: empty file path in WAL replace", ErrInvalidFormat)
	}

	st := newStringTable()
	var body binWriter
	body.buf.Grow(len(nodes)*64 + len(edges)*42 + len(sites)*48)
	body.writeString(trimmed)

	body.writeUvarint(uint64(len(nodes)))
	for i := range nodes {
		writeNodeRecord(&body, &nodes[i], st)
	}

	body.writeUvarint(uint64(len(edges)))
	for i := range edges {
		e := &edges[i]
		body.writeBytes(e.Source[:])
		body.writeBytes(e.Target[:])
		body.writeU8(uint8(e.Kind))
		body.writeU8(uint8(e.Tier))
		body.writeUvarint(uint64(st.intern(e.Label)))
		writeEdgeSites(&body, e.Sites, st)
	}

	body.writeUvarint(uint64(len(sites)))
	for i := range sites {
		writeCallSiteRecord(&body, &sites[i], st)
	}

	// Frame payload carries op + local string table + body so each frame is self-contained.
	var framedBody binWriter
	framedBody.buf.Grow(len(st.list)*16 + body.buf.Len() + 4)
	framedBody.writeU8(WALOpReplaceFile)
	framedBody.writeUvarint(uint64(len(st.list)))
	for _, s := range st.list {
		framedBody.writeString(s)
	}
	framedBody.writeBytes(body.buf.Bytes())

	return w.appendRawFrame(framedBody.buf.Bytes())
}

// AppendRemoveFile writes a compact tombstone frame removing all symbols for file.
func (w *WALWriter) AppendRemoveFile(file string) (int, error) {
	trimmed := strings.TrimSpace(file)
	if trimmed == "" {
		return 0, fmt.Errorf("%w: empty file path in WAL remove", ErrInvalidFormat)
	}

	var body binWriter
	body.writeU8(WALOpRemoveFile)
	body.writeString(trimmed)
	return w.appendRawFrame(body.buf.Bytes())
}

func (w *WALWriter) appendRawFrame(body []byte) (int, error) {
	// Frame layout: [FrameLen uint32][Body...][CRC32C uint32]
	frameLen := uint32(len(body))
	crc := crc32.Checksum(body, crc32cTable)

	var frame bytes.Buffer
	frame.Grow(4 + len(body) + 4)
	var u32 [4]byte
	binary.LittleEndian.PutUint32(u32[:], frameLen)
	_, _ = frame.Write(u32[:])
	_, _ = frame.Write(body)
	binary.LittleEndian.PutUint32(u32[:], crc)
	_, _ = frame.Write(u32[:])
	out := frame.Bytes()

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return 0, ErrClosed
	}
	if err := writeAll(w.file, out); err != nil {
		return 0, fmt.Errorf("append WAL frame: %w", err)
	}
	if w.opts.Sync {
		if err := w.file.Sync(); err != nil {
			return 0, fmt.Errorf("sync WAL frame: %w", err)
		}
	}
	w.size += int64(len(out))
	return len(out), nil
}

// Size returns the current WAL byte size on disk.
func (w *WALWriter) Size() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.size
}

// NeedsCompaction reports whether the WAL size has reached CompactThresholdBytes.
func (w *WALWriter) NeedsCompaction() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.size >= w.opts.CompactThresholdBytes
}

// Close closes the underlying WAL file handle.
func (w *WALWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	return w.file.Close()
}

// ---------- Store Coordinator (Snapshot + WAL + Compaction) ----------

// Store coordinates base snapshots (index.ggctx) and incremental WAL logs (index.wal) in a directory.
type Store struct {
	mu       sync.Mutex
	dir      string
	snapPath string
	walPath  string
	snapOpts WriteOptions
	walOpts  WALOptions
	wal      *WALWriter
	closed   bool
}

// OpenStore opens or creates a Snapshot+WAL storage directory.
func OpenStore(dir string, snapOpts WriteOptions, walOpts WALOptions) (*Store, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("%w: empty store directory", ErrInvalidFormat)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create store dir: %w", err)
	}
	snapPath := filepath.Join(dir, SnapshotFileName)
	walPath := filepath.Join(dir, WALFileName)

	wal, err := OpenWAL(walPath, walOpts)
	if err != nil {
		return nil, err
	}
	return &Store{
		dir:      dir,
		snapPath: snapPath,
		walPath:  walPath,
		snapOpts: snapOpts,
		walOpts:  walOpts,
		wal:      wal,
	}, nil
}

// SnapshotPath returns the path to the base snapshot file.
func (s *Store) SnapshotPath() string { return s.snapPath }

// WALPath returns the path to the WAL file.
func (s *Store) WALPath() string { return s.walPath }

// SaveSnapshot writes a fresh base snapshot of g without clearing the WAL.
func (s *Store) SaveSnapshot(g *graph.Graph) (SnapshotStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return SnapshotStats{}, ErrClosed
	}
	return WriteSnapshot(s.snapPath, g, s.snapOpts)
}

// AppendReplaceFile appends an incremental file replacement delta to the WAL.
func (s *Store) AppendReplaceFile(file string, nodes []graph.Node, edges []graph.Edge, sites []graph.CallSite) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, ErrClosed
	}
	return s.wal.AppendReplaceFile(file, nodes, edges, sites)
}

// AppendRemoveFile appends a file deletion tombstone to the WAL.
func (s *Store) AppendRemoveFile(file string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, ErrClosed
	}
	return s.wal.AppendRemoveFile(file)
}

// NeedsCompaction reports whether the active WAL exceeds the compaction threshold.
func (s *Store) NeedsCompaction() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.wal == nil {
		return false
	}
	return s.wal.NeedsCompaction()
}

// Compact atomically writes a fresh base snapshot from g, then atomically resets the WAL
// to an empty header so subsequent restarts avoid replaying compacted history.
func (s *Store) Compact(g *graph.Graph) (SnapshotStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return SnapshotStats{}, ErrClosed
	}

	stats, err := WriteSnapshot(s.snapPath, g, s.snapOpts)
	if err != nil {
		return SnapshotStats{}, err
	}

	if s.wal != nil {
		if err := s.wal.Close(); err != nil {
			return SnapshotStats{}, fmt.Errorf("close WAL before compaction swap: %w", err)
		}
		s.wal = nil
	}

	freshHdr := (WALHeader{Magic: WALMagic, Version: FormatVersion}).MarshalBinary()
	if err := atomicWriteFile(s.walPath, freshHdr[:], nil, s.walOpts.Sync); err != nil {
		return SnapshotStats{}, fmt.Errorf("reset WAL after snapshot: %w", err)
	}

	wal, err := OpenWAL(s.walPath, s.walOpts)
	if err != nil {
		return SnapshotStats{}, err
	}
	s.wal = wal
	return stats, nil
}

// Close closes the Store and its WAL writer.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.wal != nil {
		return s.wal.Close()
	}
	return nil
}

// ReadSnapshotDecode verifies checksum and decodes a base snapshot into a Graph.
// Phase 4 extends streaming load and full WAL replay in reader.go.
func ReadSnapshotDecode(path string) (*graph.Graph, SnapshotHeader, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, SnapshotHeader{}, err
	}
	hdr, err := UnmarshalSnapshotHeader(raw)
	if err != nil {
		return nil, SnapshotHeader{}, err
	}
	expectedEnd := uint64(SnapshotHeaderSize) + hdr.PayloadSize
	if uint64(len(raw)) != expectedEnd {
		return nil, SnapshotHeader{}, fmt.Errorf("%w: snapshot size %d != expected %d", ErrInvalidFormat, len(raw), expectedEnd)
	}
	payload := raw[SnapshotHeaderSize:]
	sum := sha256.Sum256(payload)
	if sum != hdr.Checksum {
		return nil, SnapshotHeader{}, fmt.Errorf("%w: snapshot SHA-256 mismatch", ErrChecksumMismatch)
	}

	if hdr.Flags&FlagZstd != 0 {
		dec, err := zstd.NewReader(nil)
		if err != nil {
			return nil, SnapshotHeader{}, err
		}
		payload, err = dec.DecodeAll(payload, nil)
		dec.Close()
		if err != nil {
			return nil, SnapshotHeader{}, fmt.Errorf("%w: zstd decode failed: %w", ErrInvalidFormat, err)
		}
	}

	g, err := decodeSnapshotPayload(payload, hdr.NodeCount, hdr.EdgeCount, hdr.CallSiteCount)
	if err != nil {
		return nil, SnapshotHeader{}, err
	}
	return g, hdr, nil
}

func decodeSnapshotPayload(payload []byte, nodeCount, edgeCount, siteCount uint64) (*graph.Graph, error) {
	r := newBinReader(payload)

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
	tbl := make([]string, strCount)
	for i := uint64(0); i < strCount; i++ {
		if tbl[i], err = r.readString(); err != nil {
			return nil, err
		}
	}

	if err := r.checkCount(nodeCount, 48, "snapshot node"); err != nil {
		return nil, err
	}
	nodes := make([]graph.Node, nodeCount)
	for i := uint64(0); i < nodeCount; i++ {
		if nodes[i], err = readNodeRecord(r, tbl); err != nil {
			return nil, err
		}
	}

	if err := r.checkCount(edgeCount, 6, "snapshot edge"); err != nil {
		return nil, err
	}
	edges := make([]graph.Edge, edgeCount)
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
		sites, err := readEdgeSites(r, tbl)
		if err != nil {
			return nil, err
		}
		edges[i] = graph.Edge{
			Source: nodes[srcIdx].UID,
			Target: nodes[tgtIdx].UID,
			Kind:   graph.EdgeKind(kind),
			Tier:   graph.ResolutionTier(tier),
			Label:  lbl,
			Sites:  sites,
		}
	}

	if err := r.checkCount(siteCount, 26, "snapshot call site"); err != nil {
		return nil, err
	}
	sites := make([]graph.CallSite, siteCount)
	for i := uint64(0); i < siteCount; i++ {
		if sites[i], err = readCallSiteRecord(r, tbl); err != nil {
			return nil, err
		}
	}

	g := graph.NewWithCapacity(int(nodeCount), int(edgeCount))
	if err := g.BulkLoad(nodes, edges, sites); err != nil {
		return nil, err
	}
	return g, nil
}

// ReplayWALDecode replays CRC-32C-verified WAL frames onto g.
// Stops cleanly at a truncated trailing frame if allowTrailingPartial is true,
// or returns ErrCorruptWAL otherwise.
func ReplayWALDecode(path string, g *graph.Graph, allowTrailingPartial bool) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	if _, err := UnmarshalWALHeader(raw); err != nil {
		return 0, err
	}

	pos := WALHeaderSize
	applied := 0
	for pos < len(raw) {
		if len(raw)-pos < 4 {
			if allowTrailingPartial {
				break
			}
			return applied, fmt.Errorf("%w: truncated frame length at offset %d", ErrCorruptWAL, pos)
		}
		frameLen := binary.LittleEndian.Uint32(raw[pos : pos+4])
		if frameLen == 0 {
			return applied, fmt.Errorf("%w: zero-length WAL frame at offset %d", ErrCorruptWAL, pos)
		}
		rem := uint64(len(raw) - (pos + 4))
		if uint64(frameLen)+4 > rem {
			if allowTrailingPartial {
				break
			}
			return applied, fmt.Errorf("%w: truncated frame body at offset %d", ErrCorruptWAL, pos)
		}

		frameStart := pos + 4
		crcStart := frameStart + int(frameLen)
		frameEnd := crcStart + 4

		body := raw[frameStart:crcStart]
		gotCRC := binary.LittleEndian.Uint32(raw[crcStart:frameEnd])
		if wantCRC := crc32.Checksum(body, crc32cTable); gotCRC != wantCRC {
			if allowTrailingPartial {
				break
			}
			return applied, fmt.Errorf("%w: CRC mismatch at offset %d (got %#x, want %#x)", ErrCorruptWAL, pos, gotCRC, wantCRC)
		}

		if err := applyWALFrame(body, g); err != nil {
			return applied, err
		}
		applied++
		pos = frameEnd
	}
	return applied, nil
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
