package graph

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
)

// UID is a stable 128-bit identifier for a graph node, derived from the node's
// qualified name and signature hash rather than volatile line numbers.
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

// SignatureHash computes a stable hash of a function's parameter types and return type.
// Parameter names are intentionally ignored so renaming parameters preserves identity,
// while parameter count and types (or arity alone for untyped functions) distinguish signatures.
func SignatureHash(params []Param, returnType string) string {
	h := sha256.New()
	writeLenPrefixed(h, "sig:v1")

	var countBuf [4]byte
	binary.BigEndian.PutUint32(countBuf[:], uint32(len(params)))
	_, _ = h.Write(countBuf[:])

	for _, p := range params {
		writeLenPrefixed(h, strings.TrimSpace(p.Type))
	}
	writeLenPrefixed(h, strings.TrimSpace(returnType))

	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:16])
}

// GenerateUID computes a deterministic 128-bit UID from a qualified name and signature hash.
func GenerateUID(qualifiedName, signatureHash string) UID {
	h := sha256.New()
	writeLenPrefixed(h, "uid:v1")
	writeLenPrefixed(h, qualifiedName)
	writeLenPrefixed(h, signatureHash)

	var uid UID
	copy(uid[:], h.Sum(nil)[:16])
	return uid
}

// ComputeNodeUID computes the stable UID for a Node.
// Line numbers are intentionally excluded so moving code within a file preserves identity.
func ComputeNodeUID(n *Node) UID {
	if n == nil {
		return ZeroUID
	}

	qname := n.QName
	if qname == "" {
		if n.File != "" {
			qname = n.File + "::" + n.Name
		} else {
			qname = n.Name
		}
	}

	var sigHash string
	if n.Kind == KindFunction {
		sigHash = SignatureHash(n.Params, n.ReturnType)
	} else {
		sigHash = "kind:" + n.Kind.String()
	}

	return GenerateUID(qname, sigHash)
}

func writeLenPrefixed(h interface{ Write([]byte) (int, error) }, s string) {
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(s)))
	_, _ = h.Write(lenBuf[:])
	_, _ = h.Write([]byte(s))
}

