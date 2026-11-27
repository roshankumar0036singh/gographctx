package pipeline

import (
	"cmp"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/roshankumar0036singh/gographctx/internal/config"
	"github.com/roshankumar0036singh/gographctx/internal/parser"
)

// DiscoveredFile represents one indexable source file found during discovery.
type DiscoveredFile struct {
	AbsPath         string `json:"abs_path"`
	RelPath         string `json:"rel_path"`
	Language        string `json:"language"`
	Size            int64  `json:"size"`
	ModTimeUnixNano int64  `json:"mod_time_unix_nano"`
}

// DiscoveryStats summarizes discovery counts and timing for a repository scan.
type DiscoveryStats struct {
	TotalScanned     int            `json:"total_scanned"`
	MatchedFiles     int            `json:"matched_files"`
	IgnoredFiles     int            `json:"ignored_files"`
	IgnoredDirs      int            `json:"ignored_dirs"`
	UnsupportedFiles int            `json:"unsupported_files"`
	SymlinksSkipped  int            `json:"symlinks_skipped"`
	CountByLang      map[string]int `json:"count_by_lang"`
	Duration         time.Duration  `json:"duration"`
}

// DiscoveryResult contains matched files grouped globally and by canonical language.
type DiscoveryResult struct {
	RepoRoot    string                      `json:"repo_root"`
	Files       []DiscoveredFile            `json:"files"`
	FilesByLang map[string][]DiscoveredFile `json:"files_by_lang"`
	Stats       DiscoveryStats              `json:"stats"`
}

var defaultExtToLang = map[string]string{
	".go":   "go",
	".py":   "python",
	".pyi":  "python",
	".ts":   "typescript",
	".mts":  "typescript",
	".cts":  "typescript",
	".tsx":  "tsx",
	".js":   "javascript",
	".mjs":  "javascript",
	".cjs":  "javascript",
	".jsx":  "javascript",
	".rs":   "rust",
	".java": "java",
}

// DetectLanguage resolves the canonical parser language for filePath using reg
// when provided and falling back to GoGraphCtx's standard extension table.
func DetectLanguage(filePath string, reg *parser.Registry) (string, bool) {
	if reg != nil {
		if p, ok := reg.ForFile(filePath); ok {
			return parser.NormalizeLanguage(p.Language()), true
		}
	}
	ext := parser.NormalizeExtension(filepath.Ext(strings.TrimSpace(filePath)))
	if ext == "" {
		return "", false
	}
	lang, ok := defaultExtToLang[ext]
	return lang, ok
}

// DiscoverFiles walks repoRoot, applies nested .gitignore/.ggctxignore rules,
// handles symlinks per cfg.FollowSymlinks with cycle detection, detects file
// languages, and returns deterministically sorted results grouped by language.
func DiscoverFiles(ctx context.Context, repoRoot string, cfg config.Config, reg *parser.Registry) (*DiscoveryResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(repoRoot) == "" {
		return nil, fmt.Errorf("pipeline: empty repository path")
	}

	start := time.Now()
	absRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve repo root: %w", err)
	}
	canonRoot := absRoot
	if resolved, err := filepath.EvalSymlinks(absRoot); err == nil {
		canonRoot = resolved
	}

	rootInfo, err := os.Stat(absRoot)
	if err != nil {
		return nil, fmt.Errorf("stat repo root %q: %w", absRoot, err)
	}
	if !rootInfo.IsDir() {
		return nil, fmt.Errorf("repo root %q is not a directory", absRoot)
	}

	rootMatcher, err := config.LoadRepoIgnoreMatcher(absRoot, cfg)
	if err != nil {
		return nil, fmt.Errorf("load ignore matcher: %w", err)
	}

	allowedLangs := buildAllowedLanguages(cfg.Languages)
	ignoreFileName := strings.TrimSpace(cfg.IgnoreFile)
	if ignoreFileName == "" {
		ignoreFileName = config.DefaultIgnoreFileName
	}

	res := &DiscoveryResult{
		RepoRoot:    absRoot,
		FilesByLang: make(map[string][]DiscoveredFile),
		Stats: DiscoveryStats{
			CountByLang: make(map[string]int),
		},
	}

	visitedDirs := map[string]struct{}{canonRoot: {}}
	w := &discoveryWalker{
		ctx:            ctx,
		absRoot:        absRoot,
		cfg:            cfg,
		reg:            reg,
		allowedLangs:   allowedLangs,
		ignoreFileName: ignoreFileName,
		visitedDirs:    visitedDirs,
		res:            res,
	}

	if err := w.walkDir(absRoot, "", rootMatcher); err != nil {
		return nil, err
	}

	slices.SortFunc(res.Files, func(a, b DiscoveredFile) int {
		return cmp.Compare(a.RelPath, b.RelPath)
	})
	for lang, bucket := range res.FilesByLang {
		slices.SortFunc(bucket, func(a, b DiscoveredFile) int {
			return cmp.Compare(a.RelPath, b.RelPath)
		})
		res.FilesByLang[lang] = bucket
	}
	res.Stats.MatchedFiles = len(res.Files)
	res.Stats.Duration = time.Since(start)
	return res, nil
}

type discoveryWalker struct {
	ctx            context.Context
	absRoot        string
	cfg            config.Config
	reg            *parser.Registry
	allowedLangs   map[string]bool
	ignoreFileName string
	visitedDirs    map[string]struct{}
	res            *DiscoveryResult
}

func (w *discoveryWalker) walkDir(absDir, relDir string, matcher *config.IgnoreMatcher) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}

	// Layer nested directory ignore files (.gitignore, then .ggctxignore).
	curMatcher := matcher
	if relDir != "" {
		if w.cfg.UseGitignore {
			next, err := curMatcher.ChildFromFile(relDir, filepath.Join(absDir, ".gitignore"))
			if err != nil {
				return err
			}
			curMatcher = next
		}
		next, err := curMatcher.ChildFromFile(relDir, filepath.Join(absDir, w.ignoreFileName))
		if err != nil {
			return err
		}
		curMatcher = next
	}

	entries, err := os.ReadDir(absDir)
	if err != nil {
		return fmt.Errorf("read directory %q: %w", absDir, err)
	}

	for _, entry := range entries {
		if err := w.ctx.Err(); err != nil {
			return err
		}

		name := entry.Name()
		relPath := name
		if relDir != "" {
			relPath = path.Join(relDir, name)
		}
		absPath := filepath.Join(absDir, name)

		isSymlink := entry.Type()&fs.ModeSymlink != 0
		isDir := entry.IsDir()
		var info fs.FileInfo

		if isSymlink {
			if !w.cfg.FollowSymlinks {
				w.res.Stats.TotalScanned++
				w.res.Stats.SymlinksSkipped++
				continue
			}
			targetInfo, err := os.Stat(absPath)
			if err != nil {
				// Broken symlink: skip gracefully.
				w.res.Stats.TotalScanned++
				w.res.Stats.SymlinksSkipped++
				continue
			}
			info = targetInfo
			isDir = targetInfo.IsDir()
		}

		if isDir {
			if curMatcher.ShouldIgnorePath(relPath, true) {
				w.res.Stats.IgnoredDirs++
				continue
			}
			if isSymlink {
				canon, err := filepath.EvalSymlinks(absPath)
				if err != nil {
					w.res.Stats.SymlinksSkipped++
					continue
				}
				if _, seen := w.visitedDirs[canon]; seen {
					// Prevent circular symlink recursion.
					w.res.Stats.SymlinksSkipped++
					continue
				}
				w.visitedDirs[canon] = struct{}{}
			}
			if err := w.walkDir(absPath, relPath, curMatcher); err != nil {
				return err
			}
			continue
		}

		w.res.Stats.TotalScanned++
		if curMatcher.ShouldIgnorePath(relPath, false) {
			w.res.Stats.IgnoredFiles++
			continue
		}

		lang, ok := DetectLanguage(relPath, w.reg)
		if !ok || !w.isLanguageEnabled(lang) {
			w.res.Stats.UnsupportedFiles++
			continue
		}

		if info == nil {
			fi, err := entry.Info()
			if err != nil {
				return fmt.Errorf("stat file %q: %w", absPath, err)
			}
			info = fi
		}

		df := DiscoveredFile{
			AbsPath:         absPath,
			RelPath:         relPath,
			Language:        lang,
			Size:            info.Size(),
			ModTimeUnixNano: info.ModTime().UnixNano(),
		}
		w.res.Files = append(w.res.Files, df)
		w.res.FilesByLang[lang] = append(w.res.FilesByLang[lang], df)
		w.res.Stats.CountByLang[lang]++
	}

	return nil
}

func (w *discoveryWalker) isLanguageEnabled(lang string) bool {
	if len(w.allowedLangs) == 0 {
		return true
	}
	return w.allowedLangs[parser.NormalizeLanguage(lang)]
}

func buildAllowedLanguages(langs []string) map[string]bool {
	if len(langs) == 0 {
		return nil
	}
	out := make(map[string]bool, len(langs)+1)
	for _, raw := range langs {
		norm := parser.NormalizeLanguage(raw)
		if norm == "" {
			continue
		}
		out[norm] = true
		if norm == "typescript" {
			out["tsx"] = true
		}
	}
	return out
}
