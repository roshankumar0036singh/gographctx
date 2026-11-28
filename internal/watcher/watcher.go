package watcher

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/roshankumar0036singh/gographctx/internal/config"
	"github.com/roshankumar0036singh/gographctx/internal/graph"
	"github.com/roshankumar0036singh/gographctx/internal/parser"
	"github.com/roshankumar0036singh/gographctx/internal/pipeline"
)

// UpdateReport summarizes one debounced incremental update triggered by the FileWatcher.
type UpdateReport struct {
	File              string        `json:"file"`
	Action            ChangeAction  `json:"action"`
	BodyOnly          bool          `json:"body_only"`
	DependentsUpdated int           `json:"dependents_updated"`
	Duration          time.Duration `json:"duration"`
	Error             error         `json:"error,omitempty"`
}

// String formats UpdateReport cleanly, matching GoGraphCtx's live watch log style.
func (r UpdateReport) String() string {
	if r.Error != nil {
		return fmt.Sprintf("Failed updating %s in %dms: %v", r.File, r.Duration.Milliseconds(), r.Error)
	}
	return fmt.Sprintf("Updated %s in %dms (re-resolved %d dependents)",
		r.File, r.Duration.Milliseconds(), r.DependentsUpdated)
}

// WatchOptions configures debouncing, callbacks, and diagnostic output for FileWatcher.
type WatchOptions struct {
	Debounce time.Duration
	OnUpdate func(UpdateReport)
	OnError  func(error)
	Writer   io.Writer
}

// FileWatcher recursively monitors a repository using fsnotify, filters non-source
// and ignored paths, debounces rapid event bursts, and drives IncrementalEngine.
type FileWatcher struct {
	repoRoot  string
	cfg       config.Config
	engine    *IncrementalEngine
	matcher   *config.IgnoreMatcher
	fsw       *fsnotify.Watcher
	debounce  time.Duration
	onUpdate  func(UpdateReport)
	onError   func(error)
	writer    io.Writer
	allowed   map[string]bool
	synthetic chan fsnotify.Event

	mu          sync.Mutex
	started     bool
	closed      bool
	done        chan struct{}
	cancel      context.CancelFunc
	dirs        map[string]struct{}
	dirMatchers map[string]*config.IgnoreMatcher // relDir -> layered ignore matcher
}

// NewFileWatcher constructs a recursive FileWatcher attached to engine.
func NewFileWatcher(engine *IncrementalEngine, cfg config.Config, opts WatchOptions) (*FileWatcher, error) {
	if engine == nil {
		return nil, fmt.Errorf("watcher: nil IncrementalEngine")
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create fsnotify watcher: %w", err)
	}

	matcher, err := config.LoadRepoIgnoreMatcher(engine.repoRoot, cfg)
	if err != nil {
		_ = fsw.Close()
		return nil, fmt.Errorf("load ignore matcher: %w", err)
	}

	debounce := opts.Debounce
	if debounce <= 0 {
		debounce = cfg.DebounceDuration()
	}
	if debounce <= 0 {
		debounce = time.Duration(config.DefaultDebounceMs) * time.Millisecond
	}

	fw := &FileWatcher{
		repoRoot:    engine.repoRoot,
		cfg:         cfg,
		engine:      engine,
		matcher:     matcher,
		fsw:         fsw,
		debounce:    debounce,
		onUpdate:    opts.OnUpdate,
		onError:     opts.OnError,
		writer:      opts.Writer,
		allowed:     buildAllowedWatchLanguages(cfg.Languages),
		synthetic:   make(chan fsnotify.Event, 64),
		dirs:        make(map[string]struct{}),
		dirMatchers: map[string]*config.IgnoreMatcher{"": matcher},
	}

	if err := fw.watchRecursive(engine.repoRoot); err != nil {
		_ = fsw.Close()
		return nil, err
	}
	return fw, nil
}

// Start runs the watcher event and debounce loop until ctx is canceled or Close is called.
func (fw *FileWatcher) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	fw.mu.Lock()
	if fw.closed {
		fw.mu.Unlock()
		return fmt.Errorf("watcher: already closed")
	}
	if fw.started {
		fw.mu.Unlock()
		return fmt.Errorf("watcher: already started")
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	fw.started = true
	fw.cancel = cancel
	fw.done = done
	fw.mu.Unlock()

	defer func() {
		cancel()
		close(done)
	}()

	pending := make(map[string]fsnotify.Event)
	var timer *time.Timer
	var timerC <-chan time.Time

	// Drain fw.fsw.Events continuously on a helper goroutine so calling fw.fsw.Add
	// on new subdirectories never deadlocks against fsnotify's unbuffered Events channel.
	eventsCh := make(chan fsnotify.Event, 256)
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		var q []fsnotify.Event
		for {
			if len(q) == 0 {
				select {
				case <-runCtx.Done():
					return
				case ev, ok := <-fw.fsw.Events:
					if !ok {
						return
					}
					q = append(q, ev)
				}
				continue
			}
			select {
			case <-runCtx.Done():
				return
			case ev, ok := <-fw.fsw.Events:
				if !ok {
					return
				}
				q = append(q, ev)
			case eventsCh <- q[0]:
				q[0] = fsnotify.Event{}
				q = q[1:]
			}
		}
	}()
	defer func() {
		cancel()
		<-drainDone
	}()

	resetTimer := func() {
		if timer == nil {
			timer = time.NewTimer(fw.debounce)
			timerC = timer.C
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(fw.debounce)
	}

	flushPending := func() {
		if len(pending) == 0 {
			return
		}
		paths := make([]string, 0, len(pending))
		for p := range pending {
			paths = append(paths, p)
		}
		slices.SortFunc(paths, cmp.Compare[string])
		events := make([]fsnotify.Event, len(paths))
		for i, p := range paths {
			events[i] = pending[p]
		}
		clear(pending)

		for _, ev := range events {
			if runCtx.Err() != nil {
				return
			}
			fw.processPath(runCtx, ev.Name)
		}
	}

	stopTimer := func() {
		if timer != nil {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
	}

	for {
		select {
		case <-runCtx.Done():
			stopTimer()
			return nil

		case err, ok := <-fw.fsw.Errors:
			if !ok {
				stopTimer()
				return nil
			}
			if err != nil && fw.onError != nil {
				fw.onError(err)
			}

		case ev, ok := <-eventsCh:
			if !ok {
				stopTimer()
				return nil
			}
			if fw.acceptAndTrackEvent(ev, pending) {
				resetTimer()
			}

		case ev := <-fw.synthetic:
			if fw.acceptAndTrackEvent(ev, pending) {
				resetTimer()
			}

		case <-timerC:
			flushPending()
		}
	}
}

// EnqueueEvent submits a synthetic fsnotify event for deterministic testing or manual triggers.
func (fw *FileWatcher) EnqueueEvent(ev fsnotify.Event) {
	if fw == nil || fw.synthetic == nil {
		return
	}
	fw.mu.Lock()
	closed := fw.closed
	done := fw.done
	fw.mu.Unlock()
	if closed {
		return
	}
	if done != nil {
		select {
		case fw.synthetic <- ev:
		case <-done:
		}
		return
	}
	fw.synthetic <- ev
}

// WatchedDirCount returns the number of directories currently registered with fsnotify.
func (fw *FileWatcher) WatchedDirCount() int {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	return len(fw.dirs)
}

// Close gracefully stops the watch loop and closes the underlying fsnotify watcher.
func (fw *FileWatcher) Close() error {
	fw.mu.Lock()
	if fw.closed {
		done := fw.done
		fw.mu.Unlock()
		if done != nil {
			<-done
		}
		return nil
	}
	fw.closed = true
	cancel := fw.cancel
	done := fw.done
	fsw := fw.fsw
	fw.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	if fsw != nil {
		return fsw.Close()
	}
	return nil
}

func (fw *FileWatcher) acceptAndTrackEvent(ev fsnotify.Event, pending map[string]fsnotify.Event) bool {
	if ev.Name == "" || ev.Op == fsnotify.Chmod {
		return false
	}

	absPath := ev.Name
	if !filepath.IsAbs(absPath) {
		absPath = filepath.Join(fw.repoRoot, filepath.FromSlash(absPath))
	}
	canonPath := fw.canonicalPath(absPath)
	relPath, ok := fw.relPathFromCanonical(canonPath)
	if !ok {
		return false
	}

	// Watch newly created subdirectories recursively and enqueue any source files within them.
	if ev.Op&fsnotify.Create != 0 {
		if st, err := os.Stat(canonPath); err == nil && st.IsDir() {
			if !fw.matcherForDir(path.Dir(relPath)).ShouldIgnorePath(relPath, true) {
				_ = fw.watchRecursive(canonPath)
				_ = fw.enqueueNewDirSources(canonPath, pending)
			}
			return len(pending) > 0
		}
	}

	if ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
		fw.mu.Lock()
		_, wasDir := fw.dirs[canonPath]
		if wasDir {
			delete(fw.dirs, canonPath)
			for dirPath := range fw.dirs {
				if strings.HasPrefix(dirPath, canonPath+string(filepath.Separator)) {
					delete(fw.dirs, dirPath)
				}
			}
			fw.mu.Unlock()
			fw.enqueueRemovedDirFiles(relPath, pending)
			return len(pending) > 0
		}
		fw.mu.Unlock()
	}

	if !fw.isWatchableSource(relPath) {
		return false
	}
	pending[relPath] = fsnotify.Event{Name: relPath, Op: ev.Op}
	return true
}

func (fw *FileWatcher) enqueueRemovedDirFiles(relDir string, pending map[string]fsnotify.Event) {
	if fw.engine == nil || fw.engine.Graph() == nil || relDir == "" {
		return
	}
	prefix := strings.TrimSuffix(relDir, "/") + "/"
	var doomed []string
	fw.engine.Graph().View(func(v *graph.ReadView) {
		for _, n := range v.AllNodes() {
			normFile := strings.ReplaceAll(n.File, "\\", "/")
			if n.Kind == graph.KindFile && strings.HasPrefix(normFile, prefix) {
				doomed = append(doomed, normFile)
			}
		}
	})
	for _, f := range doomed {
		pending[f] = fsnotify.Event{Name: f, Op: fsnotify.Remove}
	}
}

func (fw *FileWatcher) processPath(ctx context.Context, relPath string) {
	absPath := filepath.Join(fw.repoRoot, filepath.FromSlash(relPath))
	var (
		cs  ChangeSet
		err error
	)
	if _, statErr := os.Stat(absPath); os.IsNotExist(statErr) {
		cs, err = fw.engine.RemoveFile(ctx, relPath)
	} else {
		cs, err = fw.engine.UpdateFile(ctx, relPath)
	}

	rep := UpdateReport{
		File:              relPath,
		Action:            cs.Action,
		BodyOnly:          cs.BodyOnly,
		DependentsUpdated: cs.DependentsUpdated,
		Duration:          cs.Duration,
		Error:             err,
	}
	if fw.writer != nil {
		_, _ = fmt.Fprintln(fw.writer, rep.String())
	}
	if fw.onUpdate != nil {
		fw.onUpdate(rep)
	}
	if err != nil && fw.onError != nil {
		fw.onError(err)
	}
}

func (fw *FileWatcher) watchRecursive(rootDir string) error {
	return filepath.WalkDir(rootDir, func(absPath string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		relDir := ""
		if absPath != fw.repoRoot {
			rel, ok := fw.relPath(absPath)
			if !ok {
				return filepath.SkipDir
			}
			parentDir := path.Dir(rel)
			if parentDir == "." {
				parentDir = ""
			}
			if fw.matcherForDir(parentDir).ShouldIgnorePath(rel, true) {
				return filepath.SkipDir
			}
			relDir = rel
			fw.layerDirIgnoreFiles(absPath, relDir, parentDir)
		} else {
			fw.layerDirIgnoreFiles(absPath, "", "")
		}
		fw.mu.Lock()
		canonDir := fw.canonicalPath(absPath)
		_, seen := fw.dirs[canonDir]
		fw.mu.Unlock()
		if !seen {
			if addErr := fw.fsw.Add(canonDir); addErr == nil {
				fw.mu.Lock()
				fw.dirs[canonDir] = struct{}{}
				fw.mu.Unlock()
			}
		}
		return nil
	})
}

func (fw *FileWatcher) layerDirIgnoreFiles(absDir, relDir, parentDir string) {
	if relDir == "" {
		return
	}
	baseMatcher := fw.matcherForDir(parentDir)
	m := baseMatcher
	if fw.cfg.UseGitignore {
		if next, err := m.ChildFromFile(relDir, filepath.Join(absDir, ".gitignore")); err == nil {
			m = next
		}
	}
	ignoreName := strings.TrimSpace(fw.cfg.IgnoreFile)
	if ignoreName == "" {
		ignoreName = config.DefaultIgnoreFileName
	}
	if next, err := m.ChildFromFile(relDir, filepath.Join(absDir, ignoreName)); err == nil {
		m = next
	}
	fw.mu.Lock()
	fw.dirMatchers[relDir] = m
	fw.mu.Unlock()
}

func (fw *FileWatcher) matcherForDir(relDir string) *config.IgnoreMatcher {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	cur := relDir
	for {
		if m, ok := fw.dirMatchers[cur]; ok && m != nil {
			return m
		}
		if cur == "" || cur == "." {
			break
		}
		parent := path.Dir(cur)
		if parent == "." {
			cur = ""
		} else {
			cur = parent
		}
	}
	return fw.matcher
}

func (fw *FileWatcher) enqueueNewDirSources(dirAbs string, pending map[string]fsnotify.Event) error {
	return filepath.WalkDir(dirAbs, func(absPath string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if absPath != dirAbs {
				rel, ok := fw.relPath(absPath)
				if ok && fw.matcherForDir(rel).ShouldIgnorePath(rel, true) {
					return filepath.SkipDir
				}
			}
			return nil
		}
		rel, ok := fw.relPath(absPath)
		if ok && fw.isWatchableSource(rel) {
			pending[rel] = fsnotify.Event{Name: rel, Op: fsnotify.Create}
		}
		return nil
	})
}

func (fw *FileWatcher) isWatchableSource(relPath string) bool {
	if relPath == "" {
		return false
	}
	dir := path.Dir(relPath)
	if dir == "." {
		dir = ""
	}
	if fw.matcherForDir(dir).ShouldIgnorePath(relPath, false) {
		return false
	}
	lang, ok := pipeline.DetectLanguage(relPath, fw.engine.preg)
	if !ok {
		return false
	}
	if len(fw.allowed) > 0 && !fw.allowed[parser.NormalizeLanguage(lang)] {
		return false
	}
	return true
}

func (fw *FileWatcher) canonicalPath(absPath string) string {
	canon := filepath.Clean(absPath)
	if resolved, err := filepath.EvalSymlinks(canon); err == nil {
		return filepath.Clean(resolved)
	}
	// Handle deleted files/dirs cleanly even when repoRoot lives behind a symlink or short 8.3 path.
	if dirResolved, errDir := filepath.EvalSymlinks(filepath.Dir(canon)); errDir == nil {
		return filepath.Clean(filepath.Join(dirResolved, filepath.Base(canon)))
	}
	return canon
}

func (fw *FileWatcher) relPathFromCanonical(canon string) (string, bool) {
	rel, err := filepath.Rel(fw.repoRoot, canon)
	if err != nil {
		return "", false
	}
	slash := strings.TrimPrefix(filepath.ToSlash(path.Clean(filepath.ToSlash(rel))), "./")
	if slash == "." || slash == "" || strings.HasPrefix(slash, "../") || slash == ".." {
		return "", false
	}
	return slash, true
}

func (fw *FileWatcher) relPath(absPath string) (string, bool) {
	return fw.relPathFromCanonical(fw.canonicalPath(absPath))
}

func buildAllowedWatchLanguages(langs []string) map[string]bool {
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
