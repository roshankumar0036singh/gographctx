package lang

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"

	"github.com/odvcencio/gotreesitter"
)

const maxCollectedSyntaxErrors = 16

// ComputeContentHash returns the lowercase hex SHA-256 digest of source.
func ComputeContentHash(source []byte) string {
	sum := sha256.Sum256(source)
	return hex.EncodeToString(sum[:])
}

// CountLines returns the 1-based line count of source (0 for empty input).
func CountLines(source []byte) uint32 {
	if len(source) == 0 {
		return 0
	}
	lines := uint32(1)
	for i, b := range source {
		if b == '\n' && i < len(source)-1 {
			lines++
		}
	}
	return lines
}

// NodeStartLine returns the 1-based start line of n, or 0 if n is nil.
func NodeStartLine(n *gotreesitter.Node) uint32 {
	if n == nil {
		return 0
	}
	return n.StartPoint().Row + 1
}

// NodeEndLine returns the 1-based end line of n, or 0 if n is nil.
func NodeEndLine(n *gotreesitter.Node) uint32 {
	if n == nil {
		return 0
	}
	return n.EndPoint().Row + 1
}

// NodeStartCol returns the 1-based start column of n, or 0 if n is nil.
func NodeStartCol(n *gotreesitter.Node) uint32 {
	if n == nil {
		return 0
	}
	return n.StartPoint().Column + 1
}

// ChildrenByField returns all direct children of parent tagged with fieldName.
func ChildrenByField(parent *gotreesitter.Node, fieldName string, lang *gotreesitter.Language) []*gotreesitter.Node {
	if parent == nil || lang == nil || fieldName == "" {
		return nil
	}
	count := parent.ChildCount()
	var out []*gotreesitter.Node
	for i := 0; i < count; i++ {
		if parent.FieldNameForChild(i, lang) == fieldName {
			if child := parent.Child(i); child != nil {
				out = append(out, child)
			}
		}
	}
	return out
}

// DeriveModuleScope computes a deterministic package/module prefix for QNames.
// Relative directory paths are preserved so distinct packages (such as two cmd
// binaries both named package main) get distinct qualified scopes, while
// single-filename or absolute paths fall back to pkgName cleanly.
func DeriveModuleScope(path string, pkgName string) string {
	pkg := strings.TrimSpace(pkgName)
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return pkg
	}

	slashPath := filepath.ToSlash(trimmedPath)
	cleaned := filepath.Clean(trimmedPath)
	dir := filepath.ToSlash(filepath.Dir(cleaned))
	if dir == "." || dir == "/" || filepath.IsAbs(cleaned) || strings.HasPrefix(slashPath, "/") || strings.HasPrefix(dir, "..") {
		if pkg != "" {
			return pkg
		}
		base := strings.TrimSuffix(filepath.Base(cleaned), filepath.Ext(cleaned))
		return base
	}

	if pkg != "" && strings.HasSuffix(pkg, "_test") && filepath.Base(dir) != pkg {
		return dir + "_test"
	}
	return dir
}

// JoinQName joins non-empty symbol scope components with "::".
func JoinQName(parts ...string) string {
	nonEmpty := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			nonEmpty = append(nonEmpty, trimmed)
		}
	}
	return strings.Join(nonEmpty, "::")
}

// CollectSyntaxErrors walks root when syntax errors are present and returns
// bounded human-readable diagnostic messages.
func CollectSyntaxErrors(root *gotreesitter.Node, lang *gotreesitter.Language, limit int) []string {
	if root == nil || !root.HasError() {
		return nil
	}
	if limit <= 0 {
		limit = maxCollectedSyntaxErrors
	}
	var errs []string
	var walk func(n *gotreesitter.Node)
	walk = func(n *gotreesitter.Node) {
		if n == nil || len(errs) >= limit {
			return
		}
		if n.IsMissing() {
			typ := "token"
			if lang != nil {
				if t := n.Type(lang); t != "" {
					typ = t
				}
			}
			pt := n.StartPoint()
			errs = append(errs, formatSyntaxError(pt.Row+1, pt.Column+1, "missing "+typ))
			return
		}
		if n.IsError() {
			pt := n.StartPoint()
			errs = append(errs, formatSyntaxError(pt.Row+1, pt.Column+1, "unexpected syntax"))
			return
		}
		if !n.HasError() {
			return
		}
		count := n.ChildCount()
		for i := 0; i < count && len(errs) < limit; i++ {
			walk(n.Child(i))
		}
	}
	walk(root)
	if len(errs) == 0 && root.HasError() {
		errs = append(errs, "syntax error detected in file")
	}
	return errs
}

func formatSyntaxError(line, col uint32, msg string) string {
	return "line " + uitoa(line) + " col " + uitoa(col) + ": " + msg
}

func uitoa(v uint32) string {
	if v == 0 {
		return "0"
	}
	var buf [10]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// LeadingDocComment extracts contiguous slash or block comments immediately
// preceding targetNode (separated by no blank lines).
func LeadingDocComment(targetNode *gotreesitter.Node, source []byte, lang *gotreesitter.Language) string {
	if targetNode == nil || lang == nil {
		return ""
	}
	var lines []string
	nextStartRow := targetNode.StartPoint().Row
	for prev := targetNode.PrevSibling(); prev != nil; prev = prev.PrevSibling() {
		typ := prev.Type(lang)
		if typ != "comment" && typ != "line_comment" && typ != "block_comment" {
			break
		}
		endRow := prev.EndPoint().Row
		// Require comments to sit immediately above the target or prior comment line
		// (allow at most 1 newline between consecutive lines).
		if nextStartRow > endRow+1 {
			break
		}
		raw := strings.TrimSpace(prev.Text(source))
		cleaned := cleanCommentText(raw)
		if cleaned != "" {
			lines = append(lines, cleaned)
		}
		nextStartRow = prev.StartPoint().Row
	}
	if len(lines) == 0 {
		return ""
	}
	// Comments were gathered bottom-up; reverse into source order.
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func cleanCommentText(raw string) string {
	raw = strings.ReplaceAll(strings.ReplaceAll(raw, "\r\n", "\n"), "\r", "\n")
	if strings.HasPrefix(raw, "//") {
		txt := strings.TrimPrefix(raw, "//")
		return strings.TrimPrefix(txt, " ")
	}
	if strings.HasPrefix(raw, "/*") && strings.HasSuffix(raw, "*/") {
		inner := strings.TrimSuffix(strings.TrimPrefix(raw, "/*"), "*/")
		parts := strings.Split(inner, "\n")
		out := make([]string, 0, len(parts))
		for _, part := range parts {
			line := strings.TrimSpace(part)
			line = strings.TrimPrefix(line, "*")
			line = strings.TrimPrefix(line, " ")
			out = append(out, line)
		}
		return strings.TrimSpace(strings.Join(out, "\n"))
	}
	return raw
}

// CollapseWhitespace normalizes internal runs of newlines/tabs/spaces into single spaces.
func CollapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
