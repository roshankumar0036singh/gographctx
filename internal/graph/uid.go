package graph

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
)

// UID is a stable 128-bit identifier for a graph node, derived from (Kind, QName, Occurrence).
// Line numbers and parameter type annotations are excluded so routine edits preserve identity.
type UID [16]byte

// ZeroUID represents an unset or invalid UID.
var ZeroUID UID

// IsZero reports whether the UID is the zero value.
func (u UID) IsZero() bool {
	return u == ZeroUID
}

// String returns the 32-character lowercase hex representation of the UID.
func (u UID) String() string {
	return hex.EncodeToString(u[:])
}

// Short returns the first 8 hex characters of the UID for compact logging and metadata.
func (u UID) Short() string {
	return hex.EncodeToString(u[:4])
}

// Compare returns an integer comparing two UIDs lexicographically.
func (u UID) Compare(other UID) int {
	return bytes.Compare(u[:], other[:])
}

// MarshalText implements encoding.TextMarshaler.
func (u UID) MarshalText() ([]byte, error) {
	return []byte(u.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (u *UID) UnmarshalText(text []byte) error {
	parsed, err := ParseUID(string(text))
	if err != nil {
		return err
	}
	*u = parsed
	return nil
}

// ParseUID parses a 32-character hex string into a UID.
func ParseUID(s string) (UID, error) {
	if len(s) != hex.EncodedLen(len(ZeroUID)) {
		return ZeroUID, fmt.Errorf("invalid UID length %d: expected %d hex characters", len(s), hex.EncodedLen(len(ZeroUID)))
	}
	var uid UID
	if _, err := hex.Decode(uid[:], []byte(s)); err != nil {
		return ZeroUID, fmt.Errorf("invalid UID hex: %w", err)
	}
	return uid, nil
}

// SigHash is a 128-bit digest of a symbol's signature (parameter count/types and return type),
// stored separately from UID to detect API signature changes across edits.
type SigHash [16]byte

// ZeroSigHash represents an unset or non-applicable signature hash.
var ZeroSigHash SigHash

// IsZero reports whether the SigHash is the zero value.
func (s SigHash) IsZero() bool {
	return s == ZeroSigHash
}

// String returns the 32-character lowercase hex representation of the signature hash.
func (s SigHash) String() string {
	return hex.EncodeToString(s[:])
}

// MarshalText implements encoding.TextMarshaler.
func (s SigHash) MarshalText() ([]byte, error) {
	return []byte(s.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (s *SigHash) UnmarshalText(text []byte) error {
	if len(text) != hex.EncodedLen(len(ZeroSigHash)) {
		return fmt.Errorf("invalid signature hash length %d: expected %d hex characters", len(text), hex.EncodedLen(len(ZeroSigHash)))
	}
	var out SigHash
	if _, err := hex.Decode(out[:], text); err != nil {
		return fmt.Errorf("invalid signature hash hex: %w", err)
	}
	*s = out
	return nil
}

// ComputeSignatureHash computes a stable digest of a function's parameter arity/types and return type.
// Parameter names are ignored so renaming parameters preserves the signature digest, while adding
// type annotations or changing parameter count/types updates the digest for API-change detection.
func ComputeSignatureHash(params []Param, returnType string) SigHash {
	h := sha256.New()
	writeLenPrefixed(h, "sig:v2")

	var countBuf [4]byte
	binary.BigEndian.PutUint32(countBuf[:], uint32(len(params)))
	_, _ = h.Write(countBuf[:])

	for _, p := range params {
		writeLenPrefixed(h, strings.TrimSpace(p.Type))
	}
	writeLenPrefixed(h, strings.TrimSpace(returnType))

	var out SigHash
	copy(out[:], h.Sum(nil)[:16])
	return out
}

// GenerateNodeUID computes a deterministic 128-bit UID from node kind, qualified name,
// and same-QName sibling occurrence index.
func GenerateNodeUID(kind NodeKind, qualifiedName string, occurrence uint16) UID {
	h := sha256.New()
	writeLenPrefixed(h, "uid:v2")

	var meta [3]byte
	meta[0] = byte(kind)
	binary.BigEndian.PutUint16(meta[1:3], occurrence)
	_, _ = h.Write(meta[:])

	writeLenPrefixed(h, qualifiedName)

	var uid UID
	copy(uid[:], h.Sum(nil)[:16])
	return uid
}

// ComputeNodeUID computes the stable UID for a Node using (Kind, QName, Occurrence).
// Only KindFile nodes fall back to n.File when n.QName is empty; all other node kinds
// require an explicit QName.
func ComputeNodeUID(n Node) UID {
	qname := strings.TrimSpace(n.QName)
	if qname == "" && n.Kind == KindFile {
		qname = strings.TrimSpace(n.File)
	}
	if !n.Kind.IsValid() || qname == "" {
		return ZeroUID
	}
	return GenerateNodeUID(n.Kind, qname, n.Occurrence)
}

func writeLenPrefixed(h interface{ Write([]byte) (int, error) }, s string) {
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(s)))
	_, _ = h.Write(lenBuf[:])
	_, _ = h.Write([]byte(s))
}
