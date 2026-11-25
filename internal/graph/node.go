package graph

import (
	"fmt"
	"slices"
	"strings"
)

// NodeKind identifies the category of a code entity in the graph.
type NodeKind uint8

const (
	KindUnknown NodeKind = iota
	KindFile
	KindModule
	KindFunction
	KindClass
	KindStruct
	KindInterface
	KindEnum
	KindVariable
	KindParameter
	KindProperty
	KindTypeAlias
	KindDecorator
	KindExternal
	numNodeKinds
)

var nodeKindNames = [numNodeKinds]string{
	KindUnknown:   "Unknown",
	KindFile:      "File",
	KindModule:    "Module",
	KindFunction:  "Function",
	KindClass:     "Class",
	KindStruct:    "Struct",
	KindInterface: "Interface",
	KindEnum:      "Enum",
	KindVariable:  "Variable",
	KindParameter: "Parameter",
	KindProperty:  "Property",
	KindTypeAlias: "TypeAlias",
	KindDecorator: "Decorator",
	KindExternal:  "External",
}

// AllNodeKinds returns all defined valid NodeKind values in canonical order.
func AllNodeKinds() []NodeKind {
	out := make([]NodeKind, 0, int(numNodeKinds)-1)
	for kind := KindFile; kind < numNodeKinds; kind++ {
		out = append(out, kind)
	}
	return out
}

// IsValid reports whether k is one of the defined valid node kinds.
func (k NodeKind) IsValid() bool {
	return k >= KindFile && k < numNodeKinds
}

// String returns the canonical human-readable name of the NodeKind.
func (k NodeKind) String() string {
	if k < numNodeKinds {
		return nodeKindNames[k]
	}
	return "Unknown"
}

// MarshalText implements encoding.TextMarshaler.
func (k NodeKind) MarshalText() ([]byte, error) {
	if !k.IsValid() {
		return nil, fmt.Errorf("%w: invalid node kind %d", ErrInvalidNode, k)
	}
	return []byte(k.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (k *NodeKind) UnmarshalText(text []byte) error {
	parsed, err := ParseNodeKind(string(text))
	if err != nil {
		return err
	}
	*k = parsed
	return nil
}

// ParseNodeKind converts a string into a NodeKind (case-insensitive).
func ParseNodeKind(s string) (NodeKind, error) {
	trimmed := strings.TrimSpace(s)
	for kind := KindFile; kind < numNodeKinds; kind++ {
		if strings.EqualFold(nodeKindNames[kind], trimmed) {
			return kind, nil
		}
	}
	if strings.EqualFold(trimmed, "type_alias") {
		return KindTypeAlias, nil
	}
	return KindUnknown, fmt.Errorf("%w: unknown node kind %q", ErrInvalidNode, s)
}

// Param represents a function or method parameter.
type Param struct {
	Name string `json:"name"`
	Type string `json:"type,omitempty"`
}

// Node represents a symbol or structural entity in the code graph.
type Node struct {
	UID           UID      `json:"uid"`
	Kind          NodeKind `json:"kind"`
	Name          string   `json:"name"`
	QName         string   `json:"qualified_name"`
	Occurrence    uint16   `json:"occurrence,omitempty"`
	SignatureHash SigHash  `json:"signature_hash,omitempty"`
	File          string   `json:"file,omitempty"`
	StartLine     uint32   `json:"start_line,omitempty"`
	EndLine       uint32   `json:"end_line,omitempty"`
	Language      string   `json:"language,omitempty"`
	Type          string   `json:"type,omitempty"`
	Receiver      string   `json:"receiver,omitempty"`
	Exported      bool     `json:"exported,omitempty"`
	Params        []Param  `json:"params,omitempty"`
	ReturnType    string   `json:"return_type,omitempty"`
	Decorators    []string `json:"decorators,omitempty"`
	Docstring     string   `json:"docstring,omitempty"`
	Complexity    uint16   `json:"complexity,omitempty"`
	Bases         []string `json:"bases,omitempty"`
}

// Validate checks that the node satisfies structural invariants.
// Non-File nodes must supply an explicit non-empty QName.
func (n Node) Validate() error {
	if !n.Kind.IsValid() {
		return fmt.Errorf("%w: unknown node kind %d", ErrInvalidNode, n.Kind)
	}
	if strings.TrimSpace(n.Name) == "" {
		return fmt.Errorf("%w: empty node name", ErrInvalidNode)
	}
	if n.Kind == KindFile {
		if strings.TrimSpace(n.QName) == "" && strings.TrimSpace(n.File) == "" {
			return fmt.Errorf("%w: file node requires file path or qualified name", ErrInvalidNode)
		}
	} else if strings.TrimSpace(n.QName) == "" {
		return fmt.Errorf("%w: %s node %q requires qualified_name", ErrInvalidNode, n.Kind, n.Name)
	}
	if n.EndLine != 0 && n.EndLine < n.StartLine {
		return fmt.Errorf("%w: end_line (%d) < start_line (%d)", ErrInvalidNode, n.EndLine, n.StartLine)
	}
	return nil
}

// Clone returns a deep copy of the Node.
func (n Node) Clone() Node {
	cp := n
	cp.Params = slices.Clone(n.Params)
	cp.Decorators = slices.Clone(n.Decorators)
	cp.Bases = slices.Clone(n.Bases)
	return cp
}
