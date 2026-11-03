package nfs

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// TestWalkMetaErrorSamplesConcurrentNoCrossTalk 复现 2026-11-03 N1 部署后
// 发现的竞态：lastWalkCounters 是包级全局，而 daemon 并行调度
//（max_concurrent_runs=2，job 触发即起 goroutine）——两个策略的 scan
// 并行时，后起 walk 的启动清零会抹掉前一个 walk 已积累的错误样本。
// 2026-11-03 10:00 部署重启后 run_on_start 已让 video/ocr 真实并行过。
//
// 不变量：一次带错误的 walk 完成后，其错误样本必须仍在场——
// 并发启动的其他 walk 不得抹掉或污染它。
func TestWalkMetaErrorSamplesConcurrentNoCrossTalk(t *testing.T) {
	// A：denied 目录 + 大子树。DirWorkers=1 + ReadDir 字典序保证
	// denied 的 Open 失败先于 sub 内任何文件 visit 被处理。
	rootA := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootA, "denied"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(rootA, "denied"), 0o755) })
	for d := 0; d < 100; d++ {
		dir := filepath.Join(rootA, "sub", "d"+strconv.Itoa(d))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for f := 0; f < 50; f++ {
			p := filepath.Join(dir, "f"+strconv.Itoa(f))
			if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	started := make(chan struct{})
	var visitCount atomic.Int64
	var wsA WalkStats
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		wsA, _ = WalkMeta(context.Background(), rootA, WalkOptions{DirWorkers: 1, StatWorkers: 1},
			func(rel string, m FileMetaFull) {
				if visitCount.Add(1) == 1 {
					// 首个 visit ⇒ denied 错误已采样（见上排序论证）。
					close(started)
				}
			})
	}()
	<-started // A 正在遍历中段，denied 样本已在 A 的采样器里。

	// B：干净小树，A 运行期间并发起跑。全局样本表时代，B 的启动会清零
	// 全局 errSamples——A 的 denied 样本被抹掉。per-walk 交付后互不相干。
	rootB := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootB, "ok"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootB, "ok", "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	wsB, _ := WalkMeta(context.Background(), rootB, WalkOptions{DirWorkers: 1, StatWorkers: 1},
		func(rel string, m FileMetaFull) {})
	if len(wsB.ErrSamples) != 0 {
		t.Fatalf("clean walk B must have zero samples, got %v", wsB.ErrSamples)
	}

	wg.Wait() // A 完成。

	samples := wsA.ErrSamples
	if len(samples) == 0 {
		t.Fatal("concurrent clean walk wiped A's error samples — walk A had errors but they are unattributable")
	}
	found := false
	for _, s := range samples {
		if strings.Contains(s, "denied") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected denied sample to survive concurrent walk, got %v", samples)
	}
}
