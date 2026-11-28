package bench

import (
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// SCIPRoleDefinition is bit 0 of Occurrence.symbol_roles in scip.proto.
const SCIPRoleDefinition int32 = 1 << 0

// SCIPOccurrence represents a decoded SCIP Occurrence message.
type SCIPOccurrence struct {
	Range          []int32
	Symbol         string
	SymbolRoles    int32
	EnclosingRange []int32
}

// SCIPSymbolInfo represents minimal fields from scip.SymbolInformation.
type SCIPSymbolInfo struct {
	Symbol string
	Kind   int32
}

// SCIPDocument represents a decoded SCIP Document message.
type SCIPDocument struct {
	RelativePath string
	Language     string
	Occurrences  []SCIPOccurrence
	Symbols      []SCIPSymbolInfo
}

// SCIPIndex represents a decoded Sourcegraph SCIP Index protobuf message.
type SCIPIndex struct {
	Documents       []SCIPDocument
	ExternalSymbols []SCIPSymbolInfo
}

// ParseSCIPFile reads and decodes a binary .scip file without requiring external protobuf runtime deps.
func ParseSCIPFile(path string) (SCIPIndex, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SCIPIndex{}, err
	}
	return ParseSCIPBytes(data)
}

// ParseSCIPBytes decodes binary SCIP Index wire bytes per Sourcegraph's scip.proto schema.
func ParseSCIPBytes(data []byte) (SCIPIndex, error) {
	var idx SCIPIndex
	pos := 0
	for pos < len(data) {
		fieldNum, wireType, n := consumeTag(data[pos:])
		if n <= 0 {
			return idx, fmt.Errorf("scip: invalid tag at offset %d", pos)
		}
		pos += n
		switch {
		case fieldNum == 2 && wireType == 2: // repeated Document documents = 2
			raw, adv, err := consumeBytes(data[pos:])
			if err != nil {
				return idx, err
			}
			pos += adv
			doc, err := decodeSCIPDocument(raw)
			if err != nil {
				return idx, err
			}
			idx.Documents = append(idx.Documents, doc)
		case fieldNum == 3 && wireType == 2: // repeated SymbolInformation external_symbols = 3
			raw, adv, err := consumeBytes(data[pos:])
			if err != nil {
				return idx, err
			}
			pos += adv
			sym, err := decodeSCIPSymbolInfo(raw)
			if err != nil {
				return idx, err
			}
			idx.ExternalSymbols = append(idx.ExternalSymbols, sym)
		default:
			adv, err := skipWireField(data[pos:], wireType)
			if err != nil {
				return idx, err
			}
			pos += adv
		}
	}
	return idx, nil
}

func decodeSCIPDocument(data []byte) (SCIPDocument, error) {
	var doc SCIPDocument
	pos := 0
	for pos < len(data) {
		fieldNum, wireType, n := consumeTag(data[pos:])
		if n <= 0 {
			return doc, errors.New("scip: invalid document tag")
		}
		pos += n
		switch {
		case fieldNum == 1 && wireType == 2: // string relative_path = 1
			raw, adv, err := consumeBytes(data[pos:])
			if err != nil {
				return doc, err
			}
			pos += adv
			doc.RelativePath = filepath.ToSlash(strings.TrimPrefix(string(raw), "./"))
		case fieldNum == 2 && wireType == 2: // repeated Occurrence occurrences = 2
			raw, adv, err := consumeBytes(data[pos:])
			if err != nil {
				return doc, err
			}
			pos += adv
			occ, err := decodeSCIPOccurrence(raw)
			if err != nil {
				return doc, err
			}
			doc.Occurrences = append(doc.Occurrences, occ)
		case fieldNum == 3 && wireType == 2: // repeated SymbolInformation symbols = 3
			raw, adv, err := consumeBytes(data[pos:])
			if err != nil {
				return doc, err
			}
			pos += adv
			sym, err := decodeSCIPSymbolInfo(raw)
			if err != nil {
				return doc, err
			}
			doc.Symbols = append(doc.Symbols, sym)
		case fieldNum == 4 && wireType == 2: // string language = 4
			raw, adv, err := consumeBytes(data[pos:])
			if err != nil {
				return doc, err
			}
			pos += adv
			doc.Language = string(raw)
		default:
			adv, err := skipWireField(data[pos:], wireType)
			if err != nil {
				return doc, err
			}
			pos += adv
		}
	}
	return doc, nil
}

func decodeSCIPOccurrence(data []byte) (SCIPOccurrence, error) {
	var occ SCIPOccurrence
	pos := 0
	for pos < len(data) {
		fieldNum, wireType, n := consumeTag(data[pos:])
		if n <= 0 {
			return occ, errors.New("scip: invalid occurrence tag")
		}
		pos += n
		switch {
		case fieldNum == 1 && wireType == 2: // packed repeated int32 range = 1
			raw, adv, err := consumeBytes(data[pos:])
			if err != nil {
				return occ, err
			}
			pos += adv
			vals, err := decodePackedInt32(raw)
			if err != nil {
				return occ, err
			}
			occ.Range = append(occ.Range, vals...)
		case fieldNum == 1 && wireType == 0:
			v, adv, err := consumeVarint(data[pos:])
			if err != nil {
				return occ, err
			}
			pos += adv
			occ.Range = append(occ.Range, int32(v))
		case fieldNum == 2 && wireType == 2: // string symbol = 2
			raw, adv, err := consumeBytes(data[pos:])
			if err != nil {
				return occ, err
			}
			pos += adv
			occ.Symbol = string(raw)
		case fieldNum == 3 && wireType == 0: // int32 symbol_roles = 3
			v, adv, err := consumeVarint(data[pos:])
			if err != nil {
				return occ, err
			}
			pos += adv
			occ.SymbolRoles = int32(v)
		case fieldNum == 7 && wireType == 2: // packed repeated int32 enclosing_range = 7
			raw, adv, err := consumeBytes(data[pos:])
			if err != nil {
				return occ, err
			}
			pos += adv
			vals, err := decodePackedInt32(raw)
			if err != nil {
				return occ, err
			}
			occ.EnclosingRange = append(occ.EnclosingRange, vals...)
		default:
			adv, err := skipWireField(data[pos:], wireType)
			if err != nil {
				return occ, err
			}
			pos += adv
		}
	}
	return occ, nil
}

func decodeSCIPSymbolInfo(data []byte) (SCIPSymbolInfo, error) {
	var sym SCIPSymbolInfo
	pos := 0
	for pos < len(data) {
		fieldNum, wireType, n := consumeTag(data[pos:])
		if n <= 0 {
			return sym, errors.New("scip: invalid symbol_info tag")
		}
		pos += n
		switch {
		case fieldNum == 1 && wireType == 2: // string symbol = 1
			raw, adv, err := consumeBytes(data[pos:])
			if err != nil {
				return sym, err
			}
			pos += adv
			sym.Symbol = string(raw)
		case fieldNum == 5 && wireType == 0: // Kind kind = 5
			v, adv, err := consumeVarint(data[pos:])
			if err != nil {
				return sym, err
			}
			pos += adv
			sym.Kind = int32(v)
		default:
			adv, err := skipWireField(data[pos:], wireType)
			if err != nil {
				return sym, err
			}
			pos += adv
		}
	}
	return sym, nil
}

func decodePackedInt32(data []byte) ([]int32, error) {
	var out []int32
	pos := 0
	for pos < len(data) {
		v, adv, err := consumeVarint(data[pos:])
		if err != nil {
			return nil, err
		}
		pos += adv
		out = append(out, int32(v))
	}
	return out, nil
}

func consumeTag(data []byte) (fieldNum int, wireType int, n int) {
	v, adv := binary.Uvarint(data)
	if adv <= 0 {
		return 0, 0, adv
	}
	return int(v >> 3), int(v & 7), adv
}

func consumeVarint(data []byte) (uint64, int, error) {
	v, adv := binary.Uvarint(data)
	if adv <= 0 {
		return 0, 0, errors.New("scip: truncated varint")
	}
	return v, adv, nil
}

func consumeBytes(data []byte) ([]byte, int, error) {
	ln, adv, err := consumeVarint(data)
	if err != nil {
		return nil, 0, err
	}
	end := adv + int(ln)
	if ln > uint64(len(data)-adv) || end < adv {
		return nil, 0, errors.New("scip: truncated length-delimited field")
	}
	return data[adv:end], end, nil
}

func skipWireField(data []byte, wireType int) (int, error) {
	switch wireType {
	case 0:
		_, adv, err := consumeVarint(data)
		return adv, err
	case 1:
		if len(data) < 8 {
			return 0, errors.New("scip: truncated fixed64")
		}
		return 8, nil
	case 2:
		_, adv, err := consumeBytes(data)
		return adv, err
	case 5:
		if len(data) < 4 {
			return 0, errors.New("scip: truncated fixed32")
		}
		return 4, nil
	default:
		return 0, fmt.Errorf("scip: unsupported wire type %d", wireType)
	}
}

// ParseSCIPSymbolShortName extracts the short symbol name from a SCIP symbol string.
// E.g. "scip-python pypi app 1.0 `app.helpers`/clean_input()." -> "clean_input".
func ParseSCIPSymbolShortName(sym string) string {
	trimmed := strings.TrimSpace(sym)
	if trimmed == "" || strings.HasPrefix(trimmed, "local ") {
		return ""
	}
	// Descriptors follow the last space separating scheme/package/version from descriptor chain.
	if sp := strings.LastIndexByte(trimmed, ' '); sp >= 0 && sp+1 < len(trimmed) {
		trimmed = trimmed[sp+1:]
	}
	// Trim trailing method/type punctuation while keeping the innermost symbol token.
	for len(trimmed) > 0 {
		last := trimmed[len(trimmed)-1]
		if last == '.' || last == ')' || last == '(' || last == '!' || last == '#' || last == '/' || last == '`' {
			trimmed = trimmed[:len(trimmed)-1]
			continue
		}
		break
	}
	for _, sep := range []string{"#", "/", ".", ":"} {
		if idx := strings.LastIndex(trimmed, sep); idx >= 0 && idx+1 < len(trimmed) {
			trimmed = trimmed[idx+1:]
		}
	}
	return strings.Trim(trimmed, "`")
}

// ExtractCallEdgesFromSCIP converts in-repo SCIP definitions + call references into
// normalized ground-truth call edges.
func ExtractCallEdgesFromSCIP(idx SCIPIndex) []CallEdgeTruth {
	type defEntry struct {
		file      string
		shortName string
		startLine uint32
		endLine   uint32
		symbol    string
		isFunc    bool
	}

	defBySymbol := make(map[string]defEntry)
	docDefs := make(map[string][]defEntry)

	for _, doc := range idx.Documents {
		var defs []defEntry
		for _, occ := range doc.Occurrences {
			if occ.SymbolRoles&SCIPRoleDefinition == 0 || occ.Symbol == "" {
				continue
			}
			isFunc := strings.HasSuffix(occ.Symbol, "().")
			short := ParseSCIPSymbolShortName(occ.Symbol)
			if short == "" {
				continue
			}
			startL, endL := occLineSpan(occ.Range, occ.EnclosingRange)
			de := defEntry{
				file:      doc.RelativePath,
				shortName: short,
				startLine: startL,
				endLine:   endL,
				symbol:    occ.Symbol,
				isFunc:    isFunc,
			}
			defBySymbol[occ.Symbol] = de
			if isFunc {
				defs = append(defs, de)
			}
		}
		slices.SortFunc(defs, func(a, b defEntry) int {
			if c := cmp.Compare(a.startLine, b.startLine); c != 0 {
				return c
			}
			return cmp.Compare(a.endLine, b.endLine)
		})
		docDefs[doc.RelativePath] = defs
	}

	seen := make(map[string]struct{})
	var edges []CallEdgeTruth
	for _, doc := range idx.Documents {
		defs := docDefs[doc.RelativePath]
		for _, occ := range doc.Occurrences {
			if occ.SymbolRoles&SCIPRoleDefinition != 0 || occ.Symbol == "" {
				continue
			}
			callee, ok := defBySymbol[occ.Symbol]
			if !ok || !callee.isFunc {
				continue
			}
			refLine, _ := occLineSpan(occ.Range, nil)
			var caller *defEntry
			for i := range defs {
				if defs[i].startLine <= refLine && (defs[i].endLine == 0 || refLine <= defs[i].endLine) {
					if caller == nil || (defs[i].endLine-defs[i].startLine) <= (caller.endLine-caller.startLine) {
						caller = &defs[i]
					}
				}
			}
			if caller == nil || caller.symbol == callee.symbol {
				continue
			}
			key := fmt.Sprintf("%s|%s->%s|%s", doc.RelativePath, caller.shortName, callee.file, callee.shortName)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			edges = append(edges, CallEdgeTruth{
				CallerFile: doc.RelativePath,
				CallerName: caller.shortName,
				CalleeFile: callee.file,
				CalleeName: callee.shortName,
				Line:       refLine + 1,
			})
		}
	}
	slices.SortFunc(edges, func(a, b CallEdgeTruth) int {
		if c := cmp.Compare(a.CallerFile, b.CallerFile); c != 0 {
			return c
		}
		if c := cmp.Compare(a.CallerName, b.CallerName); c != 0 {
			return c
		}
		if c := cmp.Compare(a.CalleeFile, b.CalleeFile); c != 0 {
			return c
		}
		return cmp.Compare(a.CalleeName, b.CalleeName)
	})
	return edges
}

func occLineSpan(r, enc []int32) (uint32, uint32) {
	span := enc
	if len(span) < 3 {
		span = r
	}
	if len(span) == 0 {
		return 0, 0
	}
	start := uint32(max(0, int(span[0])))
	end := start
	if len(span) >= 4 {
		end = uint32(max(int(start), int(span[2])))
	}
	return start, end
}
