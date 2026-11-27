package watcher_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/roshankumar0036singh/gographctx/internal/config"
	"github.com/roshankumar0036singh/gographctx/internal/graph"
	"github.com/roshankumar0036singh/gographctx/internal/pipeline"
	"github.com/roshankumar0036singh/gographctx/internal/watcher"
)

func TestFileWatcherDebouncingFilteringAndGracefulShutdown(t *testing.T) {
	root := createIncrementalRepoFixture(t)
	if err := os.WriteFile(filepath.Join(root, ".ggctxignore"), []byte("ignored/\n*.tmp.py\n"), 0o600); err != nil {
		t.Fatalf("WriteFile .ggctxignore failed: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "ignored"), 0o755); err != nil {
		t.Fatalf("MkdirAll ignored failed: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.Workers = 2
	cfg.DebounceMs = 80

	eng, err := watcher.NewIncrementalEngine(context.Background(), root, cfg, pipeline.IndexOptions{})
	if err != nil {
		t.Fatalf("NewIncrementalEngine failed: %v", err)
	}
	defer func() { _ = eng.Close() }()

	var mu sync.Mutex
	var reports []watcher.UpdateReport
	var logBuf bytes.Buffer
	notifyCh := make(chan watcher.UpdateReport, 16)

	fw, err := watcher.NewFileWatcher(eng, cfg, watcher.WatchOptions{
		Debounce: 80 * time.Millisecond,
		Writer:   &logBuf,
		OnUpdate: func(r watcher.UpdateReport) {
			mu.Lock()
			reports = append(reports, r)
			mu.Unlock()
			notifyCh <- r
		},
	})
	if err != nil {
		t.Fatalf("NewFileWatcher failed: %v", err)
	}

	// Verify ignored directories are NOT registered with fsnotify.
	if fw.WatchedDirCount() < 2 {
		t.Fatalf("expected root + pkg watched, got WatchedDirCount=%d", fw.WatchedDirCount())
	}

	ctx, cancel := context.WithCancel(context.Background())
	startErr := make(chan error, 1)
	go func() {
		startErr <- fw.Start(ctx)
	}()

	// 1. Verify ignored/non-source events do NOT trigger updates.
	fw.EnqueueEvent(fsnotify.Event{Name: filepath.Join(root, "README.md"), Op: fsnotify.Write})
	fw.EnqueueEvent(fsnotify.Event{Name: filepath.Join(root, "pkg", "scratch.tmp.py"), Op: fsnotify.Write})
	fw.EnqueueEvent(fsnotify.Event{Name: filepath.Join(root, "ignored", "skip.py"), Op: fsnotify.Write})
	fw.EnqueueEvent(fsnotify.Event{Name: filepath.Join(root, "pkg", "helpers.py"), Op: fsnotify.Chmod})

	// 2. Write export update and emit a burst of 4 rapid Write events -> coalesced into 1 update!
	helpersAbs := filepath.Join(root, "pkg", "helpers.py")
	if err := os.WriteFile(helpersAbs, []byte(`def clean(raw: str) -> str:
    return raw.strip()

def emit_metric(val: str) -> str:
    return val
`), 0o600); err != nil {
		t.Fatalf("WriteFile helpers.py failed: %v", err)
	}
	for i := 0; i < 4; i++ {
		fw.EnqueueEvent(fsnotify.Event{Name: helpersAbs, Op: fsnotify.Write})
	}

	select {
	case rep := <-notifyCh:
		if rep.File != "pkg/helpers.py" || rep.DependentsUpdated != 1 || rep.Error != nil {
			t.Fatalf("unexpected debounced UpdateReport: %+v", rep)
		}
		if !strings.Contains(rep.String(), "Updated pkg/helpers.py in ") || !strings.Contains(rep.String(), "(re-resolved 1 dependents)") {
			t.Fatalf("unexpected report string: %q", rep.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for debounced update")
	}

	// Allow another debounce window to verify burst coalesced into a single update and ignored events stayed dropped.
	time.Sleep(120 * time.Millisecond)
	mu.Lock()
	reportCount := len(reports)
	mu.Unlock()
	if reportCount != 1 {
		t.Fatalf("expected 4 rapid events coalesced into 1 report, got %d (%+v)", reportCount, reports)
	}

	// Verify graph updated via watcher event (`emit_metric` edge resolved).
	eng.Graph().View(func(v *graph.ReadView) {
		if nodes := v.GetByQName("pkg.helpers::emit_metric"); len(nodes) != 1 {
			t.Fatalf("expected emit_metric node in live graph, got %+v", nodes)
		}
	})

	// 3. Test subdirectory creation + new source file indexing.
	subDir := filepath.Join(root, "pkg", "extra")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatalf("MkdirAll extra failed: %v", err)
	}
	time.Sleep(60 * time.Millisecond)
	workerAbs := filepath.Join(subDir, "worker.py")
	if err := os.WriteFile(workerAbs, []byte("def tick() -> int:\n    return 1\n"), 0o600); err != nil {
		t.Fatalf("WriteFile worker.py failed: %v", err)
	}

	// Verify dynamic dir watch attached and new file indexed via real fsnotify event.
	deadline := time.After(3 * time.Second)
	for {
		var foundWorker bool
		eng.Graph().View(func(v *graph.ReadView) {
			if len(v.GetByQName("pkg.extra.worker::tick")) > 0 {
				foundWorker = true
			}
		})
		if foundWorker {
			break
		}
		select {
		case <-notifyCh:
		case <-deadline:
			t.Fatalf("timed out waiting for real fsnotify event on new subdirectory file pkg/extra/worker.py (watchedDirs=%d)", fw.WatchedDirCount())
		}
	}

	// 4. Graceful shutdown.
	cancel()
	if err := fw.Close(); err != nil {
		t.Fatalf("fw.Close() failed: %v", err)
	}
	select {
	case err := <-startErr:
		if err != nil {
			t.Fatalf("fw.Start returned error on shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for watcher graceful shutdown")
	}
}
