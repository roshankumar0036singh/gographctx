package config

import (
	"strings"
	"testing"
)

func FuzzIgnoreMatcher(f *testing.F) {
	f.Add("*.log\n!keep.log\nvendor/\n**/tmp/**\n", "src/vendor/keep.log", false)
	f.Add("/build/\n!build/important/\n", "build/important", true)
	f.Add("foo\\ bar \n", "foo bar", false)

	f.Fuzz(func(t *testing.T, rulesRaw, relPath string, isDir bool) {
		lines := strings.Split(rulesRaw, "\n")
		if len(lines) > 64 {
			lines = lines[:64]
		}
		m, err := NewIgnoreMatcher(lines)
		if err != nil {
			return
		}
		_ = m.ShouldIgnorePath(relPath, isDir)
	})
}
