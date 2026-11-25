package storage

import (
	"bufio"
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

type recordReader interface {
	readU8() (uint8, error)
	readU16() (uint16, error)
	readU32() (uint32, error)
	readUvarint() (uint64, error)
	readExact(dst []byte) error
	readString() (string, error)
	lookupTableString(tbl []string) (string, error)
	checkCount(count, minBytes uint64, what string) error
	safePreallocCap(count uint64) int
}

const maxStreamPrealloc = 4096

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

func (r *binReader) safePreallocCap(count uint64) int {
	return int(count)
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

// streamReader decodes binary records sequentially from a bufio.Reader without buffering
// the entire uncompressed payload at once.
type streamReader struct {
	br       *bufio.Reader
	hasBound bool
	remBytes uint64
	strBuf   []byte
}

func newStreamReader(r io.Reader, bound uint64, hasBound bool) *streamReader {
	br, ok := r.(*bufio.Reader)
	if !ok {
		br = bufio.NewReaderSize(r, 256*1024)
	}
	return &streamReader{
		br:       br,
		hasBound: hasBound,
		remBytes: bound,
	}
}

func (s *streamReader) consume(n uint64) error {
	if s.hasBound {
		if n > s.remBytes {
			return io.ErrUnexpectedEOF
		}
		s.remBytes -= n
	}
	return nil
}

func (s *streamReader) ReadByte() (byte, error) {
	b, err := s.br.ReadByte()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return 0, io.ErrUnexpectedEOF
		}
		return 0, err
	}
	if err := s.consume(1); err != nil {
		return 0, err
	}
	return b, nil
}

func (s *streamReader) readU8() (uint8, error) {
	return s.ReadByte()
}

func (s *streamReader) readU16() (uint16, error) {
	var b [2]byte
	if err := s.readExact(b[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(b[:]), nil
}

func (s *streamReader) readU32() (uint32, error) {
	var b [4]byte
	if err := s.readExact(b[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(b[:]), nil
}

func (s *streamReader) readUvarint() (uint64, error) {
	var x uint64
	var shift uint
	for i := 0; i < binary.MaxVarintLen64; i++ {
		b, err := s.br.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return 0, io.ErrUnexpectedEOF
			}
			return 0, err
		}
		if s.hasBound {
			if s.remBytes == 0 {
				return 0, io.ErrUnexpectedEOF
			}
			s.remBytes--
		}
		if b < 0x80 {
			if i == binary.MaxVarintLen64-1 && b > 1 {
				return 0, fmt.Errorf("%w: varint overflow", ErrInvalidFormat)
			}
			return x | uint64(b)<<shift, nil
		}
		x |= uint64(b&0x7f) << shift
		shift += 7
	}
	return 0, fmt.Errorf("%w: varint overflow", ErrInvalidFormat)
}

func (s *streamReader) readExact(dst []byte) error {
	if err := s.consume(uint64(len(dst))); err != nil {
		return err
	}
	_, err := io.ReadFull(s.br, dst)
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

func (s *streamReader) readString() (string, error) {
	n, err := s.readUvarint()
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "", nil
	}
	if err := s.checkCount(n, 1, "string"); err != nil {
		return "", err
	}
	if n <= 1024*1024 || s.hasBound {
		ni := int(n)
		if cap(s.strBuf) < ni {
			s.strBuf = make([]byte, ni)
		} else {
			s.strBuf = s.strBuf[:ni]
		}
		if err := s.readExact(s.strBuf); err != nil {
			return "", err
		}
		return string(s.strBuf), nil
	}
	// Compressed unbounded stream: read in chunks so huge bogus varint fails on EOF without OOM.
	var sb strings.Builder
	sb.Grow(4096)
	var chunk [4096]byte
	rem := n
	for rem > 0 {
		take := min(rem, uint64(len(chunk)))
		if err := s.readExact(chunk[:take]); err != nil {
			return "", err
		}
		sb.Write(chunk[:take])
		rem -= take
	}
	return sb.String(), nil
}

func (s *streamReader) checkCount(count, minBytes uint64, what string) error {
	if count == 0 {
		return nil
	}
	if minBytes == 0 {
		minBytes = 1
	}
	if s.hasBound && count > s.remBytes/minBytes {
		return fmt.Errorf("%w: %s count %d exceeds remaining %d bytes", ErrInvalidFormat, what, count, s.remBytes)
	}
	// Even for unbounded compressed streams, reject blatantly absurd > 1e9 counts immediately.
	if count > 1<<30 {
		return fmt.Errorf("%w: %s count %d exceeds sanity limit", ErrInvalidFormat, what, count)
	}
	return nil
}

func (s *streamReader) safePreallocCap(count uint64) int {
	if s.hasBound {
		return int(count)
	}
	if count > maxStreamPrealloc {
		return maxStreamPrealloc
	}
	return int(count)
}

func (s *streamReader) lookupTableString(tbl []string) (string, error) {
	idx, err := s.readUvarint()
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

func readNodeRecord(r recordReader, tbl []string) (graph.Node, error) {
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
		n.Params = make([]graph.Param, 0, r.safePreallocCap(paramCount))
		for i := uint64(0); i < paramCount; i++ {
			var p graph.Param
			if p.Name, err = r.readString(); err != nil {
				return n, err
			}
			if p.Type, err = r.lookupTableString(tbl); err != nil {
				return n, err
			}
			n.Params = append(n.Params, p)
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
		n.Decorators = make([]string, 0, r.safePreallocCap(decCount))
		for i := uint64(0); i < decCount; i++ {
			dec, err := r.lookupTableString(tbl)
			if err != nil {
				return n, err
			}
			n.Decorators = append(n.Decorators, dec)
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
		n.Bases = make([]string, 0, r.safePreallocCap(baseCount))
		for i := uint64(0); i < baseCount; i++ {
			base, err := r.lookupTableString(tbl)
			if err != nil {
				return n, err
			}
			n.Bases = append(n.Bases, base)
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

func readEdgeSites(r recordReader, tbl []string) ([]graph.CallSiteRef, error) {
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
	sites := make([]graph.CallSiteRef, 0, r.safePreallocCap(cnt))
	for i := uint64(0); i < cnt; i++ {
		site, err := readSingleEdgeSite(r, tbl)
		if err != nil {
			return nil, err
		}
		sites = append(sites, site)
	}
	return sites, nil
}

func readDenseEdgeSites(r recordReader, tbl []string, dst *graph.DenseEdge) error {
	cnt, err := r.readUvarint()
	if err != nil {
		return err
	}
	if cnt == 0 {
		return nil
	}
	if err := r.checkCount(cnt, 5, "edge site"); err != nil {
		return err
	}
	site0, err := readSingleEdgeSite(r, tbl)
	if err != nil {
		return err
	}
	dst.HasPrimary = true
	dst.PrimarySite = site0
	if cnt == 1 {
		return nil
	}
	dst.Sites = make([]graph.CallSiteRef, 0, r.safePreallocCap(cnt-1))
	for i := uint64(1); i < cnt; i++ {
		site, err := readSingleEdgeSite(r, tbl)
		if err != nil {
			return err
		}
		dst.Sites = append(dst.Sites, site)
	}
	return nil
}

func readSingleEdgeSite(r recordReader, tbl []string) (graph.CallSiteRef, error) {
	line, err := r.readUvarint()
	if err != nil {
		return graph.CallSiteRef{}, err
	}
	col, err := r.readU16()
	if err != nil {
		return graph.CallSiteRef{}, err
	}
	tier, err := r.readU8()
	if err != nil {
		return graph.CallSiteRef{}, err
	}
	lbl, err := r.lookupTableString(tbl)
	if err != nil {
		return graph.CallSiteRef{}, err
	}
	return graph.CallSiteRef{
		Line:   uint32(line),
		Column: col,
		Tier:   graph.ResolutionTier(tier),
		Label:  lbl,
	}, nil
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

func readCallSiteRecord(r recordReader, tbl []string) (graph.CallSite, error) {
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
		cs.Candidates = make([]graph.UID, 0, r.safePreallocCap(candCount))
		for i := uint64(0); i < candCount; i++ {
			var u graph.UID
			if err = r.readExact(u[:]); err != nil {
				return cs, err
			}
			cs.Candidates = append(cs.Candidates, u)
		}
	}
	return cs, nil
}
