package graph

import "strings"

// stringPool deduplicates frequently repeated strings (file paths, language names,
// type names, resolution labels) and clones substrings so slices of large file
// buffers are not retained on the heap.
type stringPool struct {
	m map[string]string
}

func newStringPool() *stringPool {
	return &stringPool{
		m: make(map[string]string),
	}
}

func (p *stringPool) intern(s string) string {
	if s == "" {
		return ""
	}
	if existing, ok := p.m[s]; ok {
		return existing
	}
	cp := strings.Clone(s)
	p.m[cp] = cp
	return cp
}

// labelPool maps low-cardinality strings (such as resolution labels) to compact
// uint32 IDs so edges avoid storing 16-byte string headers per edge.
type labelPool struct {
	strs []string
	toID map[string]uint32
}

func newLabelPool() *labelPool {
	return &labelPool{
		strs: []string{""}, // ID 0 is always ""
		toID: make(map[string]uint32),
	}
}

func (p *labelPool) intern(s string) uint32 {
	if s == "" {
		return 0
	}
	if id, ok := p.toID[s]; ok {
		return id
	}
	cp := strings.Clone(s)
	id := uint32(len(p.strs))
	p.strs = append(p.strs, cp)
	p.toID[cp] = id
	return id
}

func (p *labelPool) get(id uint32) string {
	if int(id) >= len(p.strs) {
		return ""
	}
	return p.strs[id]
}
