package engine

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bcyup/filecleaner/internal/nfs"
	"github.com/bcyup/filecleaner/internal/policy"
)

// fakeFeed 生成 N 个虚拟条目（不落盘），用于验证流水线有界性与背压。
type fakeFeed struct {
	n     int
	block atomic.Bool // 置 true 时 visitor 阻塞
}

func TestPipelineBoundedAndDrain(t *testing.T) {
	ctx := context.Background()

	// 100 万虚拟条目：验证扫描计数与 drain 独立计时
	feed := &fakeFeed{n: 1_000_000}
	p := NewPipeline(PipelineOptions{
		DirWorkers: 4, StatWorkers: 8, MoveWorkers: 2,
	})

	var scanned, matched atomic.Int64
	start := time.Now()
	stats, err := p.RunVirtual(ctx, feed.n, func(m policy.FileMeta) bool {
		scanned.Add(1)
		// 全部命中
		return true
	}, func(path string) error {
		matched.Add(1)
		return nil
	})
	if err != nil {
		t.Fatalf("RunVirtual: %v", err)
	}
	if scanned.Load() != int64(feed.n) {
		t.Fatalf("scanned=%d want=%d", scanned.Load(), feed.n)
	}
	if stats.ScanElapsed <= 0 || stats.TotalElapsed <= stats.ScanElapsed {
		t.Fatalf("scan_elapsed=%v total=%v must satisfy 0<scan<=total", stats.ScanElapsed, stats.TotalElapsed)
	}
	if stats.DrainElapsed < 0 {
		t.Fatalf("drain=%v must be >=0", stats.DrainElapsed)
	}
	_ = start
}

// TestPipelineSlowConsumerNoLeak：move 消费者永久阻塞时，取消 ctx 后
// 流水线必须收敛返回（内联背压路径被 ctx 解锁），结束后无 goroutine 泄漏。
func TestPipelineSlowConsumerNoLeak(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	p := NewPipeline(PipelineOptions{DirWorkers: 2, StatWorkers: 2, MoveWorkers: 1})
	before := runtimeGoroutines()

	var started atomic.Int64
	doneCh := make(chan *RunStats, 1)
	errCh := make(chan error, 1)
	go func() {
		stats, err := p.RunVirtual(ctx, 1000, func(m policy.FileMeta) bool { return true }, func(path string) error {
			started.Add(1)
			<-ctx.Done() // 阻塞直到取消
			return ctx.Err()
		})
		doneCh <- stats
		errCh <- err
	}()

	// 等待至少一个 move 已启动，再取消
	deadline := time.Now().Add(2 * time.Second)
	for started.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if started.Load() == 0 {
		t.Fatal("move never started")
	}
	cancel()

	select {
	case <-doneCh:
		if err := <-errCh; err == nil {
			t.Fatalf("canceled run must surface ctx error, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunVirtual did not converge after cancel")
	}

	time.Sleep(100 * time.Millisecond)
	after := runtimeGoroutines()
	if after > before+8 {
		t.Fatalf("goroutine leak: before=%d after=%d", before, after)
	}
}

// TestRealTreeScanEvaluate：真实 tmpdir 树 + 扫描 + 条件判定端到端。
func TestRealTreeScanEvaluate(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-48 * time.Hour)
	newer := time.Now().Add(-1 * time.Hour)

	// old-a.txt（应命中）、new.txt（不命中）、excl/old-b.txt（目录排除）、keep.log（pattern 排除）
	for name, ts := range map[string]time.Time{
		"old-a.txt":        old,
		"new.txt":          newer,
		"keep.log":         old,
		"excl/old-b.txt":   old,
		"sub/old-c.txt":    old,
		"sub/new-d.txt":    newer,
	} {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, ts, ts); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	var moved []string
	p := NewPipeline(PipelineOptions{DirWorkers: 4, StatWorkers: 8, MoveWorkers: 4})
	stats, err := p.RunTree(ctx2(), root, TreeOptions{
		ExcludeDirs: []string{"excl"},
		ExcludePats: []string{"*.log"},
		Conditions:  policy.Conditions{"mtime": 24 * time.Hour},
		OnMove: func(rel string, meta policy.FileMeta, full nfs.FileMetaFull) error {
			mu.Lock()
			moved = append(moved, rel)
			mu.Unlock()
			return nil
		},
	})
	if err != nil {
		t.Fatalf("RunTree: %v", err)
	}
	_ = nfs.DefaultBatchSize

	want := map[string]bool{"old-a.txt": false, "sub/old-c.txt": false}
	for _, m := range moved {
		if _, ok := want[m]; !ok {
			t.Errorf("unexpected move: %q", m)
		}
		want[m] = true
	}
	for m, ok := range want {
		if !ok {
			t.Errorf("expected move missing: %q (moved=%v)", m, moved)
		}
	}
	if stats.Files != 5 {
		t.Errorf("stats.Files=%d want 5 (excluded subtree not visited)", stats.Files)
	}
}
