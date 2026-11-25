package storage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"strings"

	"github.com/roshankumar0036singh/gographctx/internal/graph"
)

var (
	// SnapshotMagic identifies a GoGraphCtx base snapshot file.
	SnapshotMagic = [8]byte{'G', 'G', 'C', 'T', 'X', 0, 0, 1}
	// WALMagic identifies a GoGraphCtx append-only write-ahead log.
	WALMagic = [8]byte{'G', 'G', 'C', 'W', 'A', 'L', 0, 1}
)

const (
	// FormatVersion is the binary schema version for snapshots and WAL logs.
	FormatVersion uint32 = 1

	// SnapshotHeaderSize is the fixed size in bytes of the snapshot header.
	SnapshotHeaderSize = 80
	// WALHeaderSize is the fixed size in bytes of the WAL file header.
	WALHeaderSize = 16

	// FlagZstd indicates the snapshot payload is zstd-compressed.
	FlagZstd uint32 = 1 << 0

	// WAL operation tags.
	WALOpReplaceFile uint8 = 1
	WALOpRemoveFile  uint8 = 2
)

var (
	ErrInvalidFormat    = errors.New("storage: invalid file format or magic")
	ErrVersionMismatch  = errors.New("storage: unsupported format version")
	ErrChecksumMismatch = errors.New("storage: checksum mismatch")
	ErrCorruptWAL       = errors.New("storage: corrupt WAL frame")
	ErrClosed           = errors.New("storage: store or WAL already closed")
)

var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

// SnapshotHeader describes the fixed 80-byte header at offset 0 of a .ggctx snapshot.
type SnapshotHeader struct {
	Magic         [8]byte
	Version       uint32
	Flags         uint32
	NodeCount     uint64
	EdgeCount     uint64
	CallSiteCount uint64
	PayloadSize   uint64
	Checksum      [32]byte
}

// MarshalBinary encodes the 80-byte SnapshotHeader in little-endian byte order.
func (h SnapshotHeader) MarshalBinary() [SnapshotHeaderSize]byte {
	var buf [SnapshotHeaderSize]byte
	copy(buf[0:8], h.Magic[:])
	binary.LittleEndian.PutUint32(buf[8:12], h.Version)
	binary.LittleEndian.PutUint32(buf[12:16], h.Flags)
	binary.LittleEndian.PutUint64(buf[16:24], h.NodeCount)
	binary.LittleEndian.PutUint64(buf[24:32], h.EdgeCount)
	binary.LittleEndian.PutUint64(buf[32:40], h.CallSiteCount)
	binary.LittleEndian.PutUint64(buf[40:48], h.PayloadSize)
	copy(buf[48:80], h.Checksum[:])
	return buf
}

// UnmarshalSnapshotHeader parses and validates the fixed 80-byte SnapshotHeader.
func UnmarshalSnapshotHeader(data []byte) (SnapshotHeader, error) {
	if len(data) < SnapshotHeaderSize {
		return SnapshotHeader{}, fmt.Errorf("%w: header truncated (%d < %d bytes)", ErrInvalidFormat, len(data), SnapshotHeaderSize)
	}
	var h SnapshotHeader
	copy(h.Magic[:], data[0:8])
	if h.Magic != SnapshotMagic {
		return SnapshotHeader{}, fmt.Errorf("%w: unexpected snapshot magic %q", ErrInvalidFormat, h.Magic)
	}
	h.Version = binary.LittleEndian.Uint32(data[8:12])
	if h.Version != FormatVersion {
		return SnapshotHeader{}, fmt.Errorf("%w: got %d, want %d", ErrVersionMismatch, h.Version, FormatVersion)
	}
	h.Flags = binary.LittleEndian.Uint32(data[12:16])
	h.NodeCount = binary.LittleEndian.Uint64(data[16:24])
	h.EdgeCount = binary.LittleEndian.Uint64(data[24:32])
	h.CallSiteCount = binary.LittleEndian.Uint64(data[32:40])
	h.PayloadSize = binary.LittleEndian.Uint64(data[40:48])
	copy(h.Checksum[:], data[48:80])
	return h, nil
}

// WALHeader describes the fixed 16-byte header at offset 0 of a .wal file.
type WALHeader struct {
	Magic    [8]byte
	Version  uint32
	Reserved uint32
}

// MarshalBinary encodes the 16-byte WALHeader in little-endian order.
func (h WALHeader) MarshalBinary() [WALHeaderSize]byte {
	var buf [WALHeaderSize]byte
	copy(buf[0:8], h.Magic[:])
	binary.LittleEndian.PutUint32(buf[8:12], h.Version)
	binary.LittleEndian.PutUint32(buf[12:16], h.Reserved)
	return buf
}

// UnmarshalWALHeader parses and validates the fixed 16-byte WALHeader.
func UnmarshalWALHeader(data []byte) (WALHeader, error) {
	if len(data) < WALHeaderSize {
		return WALHeader{}, fmt.Errorf("%w: WAL header truncated (%d < %d bytes)", ErrInvalidFormat, len(data), WALHeaderSize)
	}
	var h WALHeader
	copy(h.Magic[:], data[0:8])
	if h.Magic != WALMagic {
		return WALHeader{}, fmt.Errorf("%w: unexpected WAL magic %q", ErrInvalidFormat, h.Magic)
	}
	h.Version = binary.LittleEndian.Uint32(data[8:12])
	if h.Version != FormatVersion {
		return WALHeader{}, fmt.Errorf("%w: WAL version %d, want %d", ErrVersionMismatch, h.Version, FormatVersion)
	}
	h.Reserved = binary.LittleEndian.Uint32(data[12:16])
	return h, nil
}

// ---------- Shared Compact Binary Codec Helpers ----------

type stringTable struct {
	list  []string
	index map[string]uint32
}

func newStringTable() *stringTable {
	return &stringTable{
		list:  []string{""}, // 0 always maps to empty string
		index: map[string]uint32{"": 0},
	}
}

func (st *stringTable) intern(s string) uint32 {
	if id, ok := st.index[s]; ok {
		return id
	}
	cp := strings.Clone(s)
	id := uint32(len(st.list))
	st.list = append(st.list, cp)
	st.index[cp] = id
	return id
}

type binWriter struct {
	buf bytes.Buffer
}

func (w *binWriter) writeU8(v uint8) {
	_ = w.buf.WriteByte(v)
}

func (w *binWriter) writeU16(v uint16) {
	var b [2]byte
	binary.LittleEndian.PutUint16(b[:], v)
	_, _ = w.buf.Write(b[:])
}

func (w *binWriter) writeU32(v uint32) {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	_, _ = w.buf.Write(b[:])
}

func (w *binWriter) writeUvarint(v uint64) {
	var b [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(b[:], v)
	_, _ = w.buf.Write(b[:n])
}

func (w *binWriter) writeBytes(b []byte) {
	_, _ = w.buf.Write(b)
}

func (w *binWriter) writeString(s string) {
	w.writeUvarint(uint64(len(s)))
	_, _ = w.buf.WriteString(s)
}

type binReader struct {
	data []byte
	pos  int
}

func newBinReader(data []byte) *binReader {
	return &binReader{data: data}
}

func (r *binReader) remaining() int {
	return len(r.data) - r.pos
}

func (r *binReader) readU8() (uint8, error) {
	if r.pos >= len(r.data) {
		return 0, io.ErrUnexpectedEOF
	}
	v := r.data[r.pos]
	r.pos++
	return v, nil
}

func (r *binReader) readU16() (uint16, error) {
	if r.remaining() < 2 {
		return 0, io.ErrUnexpectedEOF
	}
	v := binary.LittleEndian.Uint16(r.data[r.pos : r.pos+2])
	r.pos += 2
	return v, nil
}

func (r *binReader) readU32() (uint32, error) {
	if r.remaining() < 4 {
		return 0, io.ErrUnexpectedEOF
	}
	v := binary.LittleEndian.Uint32(r.data[r.pos : r.pos+4])
	r.pos += 4
	return v, nil
}

func (r *binReader) readUvarint() (uint64, error) {
	v, n := binary.Uvarint(r.data[r.pos:])
	if n == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	if n < 0 {
		return 0, fmt.Errorf("%w: varint overflow", ErrInvalidFormat)
	}
	r.pos += n
	return v, nil
}

func (r *binReader) readExact(dst []byte) error {
	if r.remaining() < len(dst) {
		return io.ErrUnexpectedEOF
	}
	copy(dst, r.data[r.pos:r.pos+len(dst)])
	r.pos += len(dst)
	return nil
}

func (r *binReader) readString() (string, error) {
	n, err := r.readUvarint()
	if err != nil {
		return "", err
	}
	if n > uint64(r.remaining()) {
		return "", io.ErrUnexpectedEOF
	}
	s := string(r.data[r.pos : r.pos+int(n)])
	r.pos += int(n)
	return s, nil
}

func (r *binReader) checkCount(count, minBytes uint64, what string) error {
	if count == 0 {
		return nil
	}
	rem := uint64(r.remaining())
	if minBytes == 0 {
		minBytes = 1
	}
	if count > rem/minBytes {
		return fmt.Errorf("%w: %s count %d exceeds remaining %d bytes", ErrInvalidFormat, what, count, rem)
	}
	return nil
}

func (r *binReader) lookupTableString(tbl []string) (string, error) {
	idx, err := r.readUvarint()
	if err != nil {
		return "", err
	}
	if idx >= uint64(len(tbl)) {
		return "", fmt.Errorf("%w: string table index %d out of bounds (%d)", ErrInvalidFormat, idx, len(tbl))
	}
	return tbl[idx], nil
}

func writeNodeRecord(w *binWriter, n *graph.Node, st *stringTable) {
	w.writeBytes(n.UID[:])
	w.writeU8(uint8(n.Kind))
	w.writeU16(n.Occurrence)
	w.writeBytes(n.SignatureHash[:])

	w.writeString(n.Name)
	w.writeString(n.QName)

	w.writeUvarint(uint64(st.intern(n.File)))
	w.writeUvarint(uint64(st.intern(n.Language)))
	w.writeUvarint(uint64(st.intern(n.Type)))
	w.writeUvarint(uint64(st.intern(n.Receiver)))
	w.writeUvarint(uint64(st.intern(n.ReturnType)))

	w.writeU32(n.StartLine)
	w.writeU32(n.EndLine)
	w.writeU16(n.Complexity)
	if n.Exported {
		w.writeU8(1)
	} else {
		w.writeU8(0)
	}

	w.writeUvarint(uint64(len(n.Params)))
	for i := range n.Params {
		w.writeString(n.Params[i].Name)
		w.writeUvarint(uint64(st.intern(n.Params[i].Type)))
	}

	w.writeUvarint(uint64(len(n.Decorators)))
	for i := range n.Decorators {
		w.writeUvarint(uint64(st.intern(n.Decorators[i])))
	}

	w.writeUvarint(uint64(len(n.Bases)))
	for i := range n.Bases {
		w.writeUvarint(uint64(st.intern(n.Bases[i])))
	}

	w.writeString(n.Docstring)
}

func readNodeRecord(r *binReader, tbl []string) (graph.Node, error) {
	var n graph.Node
	if err := r.readExact(n.UID[:]); err != nil {
		return n, err
	}
	kind, err := r.readU8()
	if err != nil {
		return n, err
	}
	n.Kind = graph.NodeKind(kind)
	if n.Occurrence, err = r.readU16(); err != nil {
		return n, err
	}
	if err := r.readExact(n.SignatureHash[:]); err != nil {
		return n, err
	}

	if n.Name, err = r.readString(); err != nil {
		return n, err
	}
	if n.QName, err = r.readString(); err != nil {
		return n, err
	}
	if n.File, err = r.lookupTableString(tbl); err != nil {
		return n, err
	}
	if n.Language, err = r.lookupTableString(tbl); err != nil {
		return n, err
	}
	if n.Type, err = r.lookupTableString(tbl); err != nil {
		return n, err
	}
	if n.Receiver, err = r.lookupTableString(tbl); err != nil {
		return n, err
	}
	if n.ReturnType, err = r.lookupTableString(tbl); err != nil {
		return n, err
	}
	if n.StartLine, err = r.readU32(); err != nil {
		return n, err
	}
	if n.EndLine, err = r.readU32(); err != nil {
		return n, err
	}
	if n.Complexity, err = r.readU16(); err != nil {
		return n, err
	}
	exp, err := r.readU8()
	if err != nil {
		return n, err
	}
	n.Exported = exp != 0

	paramCount, err := r.readUvarint()
	if err != nil {
		return n, err
	}
	if paramCount > 0 {
		if err := r.checkCount(paramCount, 2, "param"); err != nil {
			return n, err
		}
		n.Params = make([]graph.Param, paramCount)
		for i := uint64(0); i < paramCount; i++ {
			if n.Params[i].Name, err = r.readString(); err != nil {
				return n, err
			}
			if n.Params[i].Type, err = r.lookupTableString(tbl); err != nil {
				return n, err
			}
		}
	}

	decCount, err := r.readUvarint()
	if err != nil {
		return n, err
	}
	if decCount > 0 {
		if err := r.checkCount(decCount, 1, "decorator"); err != nil {
			return n, err
		}
		n.Decorators = make([]string, decCount)
		for i := uint64(0); i < decCount; i++ {
			if n.Decorators[i], err = r.lookupTableString(tbl); err != nil {
				return n, err
			}
		}
	}

	baseCount, err := r.readUvarint()
	if err != nil {
		return n, err
	}
	if baseCount > 0 {
		if err := r.checkCount(baseCount, 1, "base"); err != nil {
			return n, err
		}
		n.Bases = make([]string, baseCount)
		for i := uint64(0); i < baseCount; i++ {
			if n.Bases[i], err = r.lookupTableString(tbl); err != nil {
				return n, err
			}
		}
	}

	if n.Docstring, err = r.readString(); err != nil {
		return n, err
	}
	return n, nil
}

func writeEdgeSites(w *binWriter, sites []graph.CallSiteRef, st *stringTable) {
	w.writeUvarint(uint64(len(sites)))
	for i := range sites {
		w.writeUvarint(uint64(sites[i].Line))
		w.writeU16(sites[i].Column)
		w.writeU8(uint8(sites[i].Tier))
		w.writeUvarint(uint64(st.intern(sites[i].Label)))
	}
}

func readEdgeSites(r *binReader, tbl []string) ([]graph.CallSiteRef, error) {
	cnt, err := r.readUvarint()
	if err != nil {
		return nil, err
	}
	if cnt == 0 {
		return nil, nil
	}
	if err := r.checkCount(cnt, 5, "edge site"); err != nil {
		return nil, err
	}
	sites := make([]graph.CallSiteRef, cnt)
	for i := uint64(0); i < cnt; i++ {
		line, err := r.readUvarint()
		if err != nil {
			return nil, err
		}
		col, err := r.readU16()
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
		sites[i] = graph.CallSiteRef{
			Line:   uint32(line),
			Column: col,
			Tier:   graph.ResolutionTier(tier),
			Label:  lbl,
		}
	}
	return sites, nil
}

func writeCallSiteRecord(w *binWriter, cs *graph.CallSite, st *stringTable) {
	w.writeBytes(cs.Caller[:])
	w.writeUvarint(uint64(st.intern(cs.File)))
	w.writeUvarint(uint64(cs.Line))
	w.writeU16(cs.Column)
	w.writeString(cs.CalleeExpr)
	w.writeUvarint(uint64(st.intern(cs.LookupKey)))
	w.writeU8(uint8(cs.Status))
	w.writeU8(uint8(cs.Tier))
	w.writeString(cs.Reason)

	w.writeUvarint(uint64(len(cs.Candidates)))
	for i := range cs.Candidates {
		w.writeBytes(cs.Candidates[i][:])
	}
}

func readCallSiteRecord(r *binReader, tbl []string) (graph.CallSite, error) {
	var cs graph.CallSite
	var err error
	if err = r.readExact(cs.Caller[:]); err != nil {
		return cs, err
	}
	if cs.File, err = r.lookupTableString(tbl); err != nil {
		return cs, err
	}
	line, err := r.readUvarint()
	if err != nil {
		return cs, err
	}
	cs.Line = uint32(line)
	if cs.Column, err = r.readU16(); err != nil {
		return cs, err
	}
	if cs.CalleeExpr, err = r.readString(); err != nil {
		return cs, err
	}
	if cs.LookupKey, err = r.lookupTableString(tbl); err != nil {
		return cs, err
	}
	st, err := r.readU8()
	if err != nil {
		return cs, err
	}
	cs.Status = graph.CallStatus(st)
	t, err := r.readU8()
	if err != nil {
		return cs, err
	}
	cs.Tier = graph.ResolutionTier(t)
	if cs.Reason, err = r.readString(); err != nil {
		return cs, err
	}
	candCount, err := r.readUvarint()
	if err != nil {
		return cs, err
	}
	if candCount > 0 {
		if err := r.checkCount(candCount, 16, "candidate"); err != nil {
			return cs, err
		}
		cs.Candidates = make([]graph.UID, candCount)
		for i := uint64(0); i < candCount; i++ {
			if err = r.readExact(cs.Candidates[i][:]); err != nil {
				return cs, err
			}
		}
	}
	return cs, nil
}
