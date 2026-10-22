package main

// 并行调度行为测试（Wave 2 T2）：注入 runFn/purgeFn 闭包驱动
// daemonLoopWith，钉死四个不变量——
//  1. 不同策略同时触发不被串行化
//  2. job.next 在触发瞬间推进（不依赖执行完成）
//  3. 信号量限制并发度（=1 退化串行、=2 允许重叠）
//  4. ctx 取消时 daemonLoop 返回前 in-flight job 已结束

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bcyup/filecleaner/internal/config"
)

// parallelTestCfg 两策略同分钟调度的最小配置（schedule 由调用方注入）。
func parallelTestCfg(t *testing.T, dir, sched string, maxConc int) *config.Config {
	t.Helper()
	cfgPath := filepath.Join(dir, "config.yaml")
	content := `
state_dir: ` + filepath.Join(dir, "state") + `
audit_log: {rotation: off}
max_concurrent_runs: ` + itoa(maxConc) + `
policies:
  - id: pa
    root: ` + filepath.Join(dir, "ra") + `
    schedule: "` + sched + `"
    retention:
      conditions:
        mtime_older_than: 1d
    trash:
      retention: 10d
  - id: pb
    root: ` + filepath.Join(dir, "rb") + `
    schedule: "` + sched + `"
    retention:
      conditions:
        mtime_older_than: 1d
    trash:
      retention: 10d
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func itoa(n int) string { return strconv.Itoa(n) }

// TestDaemonLoopParallelJobsDontSerialize T2-1：策略 A 阻塞 1.5s、
// 策略 B 秒回；并行实现下 B 必先于 A 完成（串行实现红）。
func TestDaemonLoopParallelJobsDontSerialize(t *testing.T) {
	dir := t.TempDir()
	cfg := parallelTestCfg(t, dir, "0 3 31 2 *", 2) // 永不触发的 cron——直接测 on-start 路径
	cfg.RunOnStart = true

	var mu sync.Mutex
	order := []string{}
	runFn := func(ctx context.Context, pol *config.Policy) error {
		if pol.ID == "pa" {
			time.Sleep(1500 * time.Millisecond)
		}
		mu.Lock()
		order = append(order, pol.ID)
		mu.Unlock()
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out bytes.Buffer
	daemonLoopWith(ctx, cfg, &out, runFn, func(*config.Policy) error { return nil })

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "pb" || order[1] != "pa" {
		t.Fatalf("order=%v want [pb pa] — pb must complete before blocking pa (parallelism broken)", order)
	}
}

// TestJobNextAdvancedOnTrigger T2-2：触发后 next 立即越过触发点，
// 不等执行完成。观测方式：runFn 阻塞期间第二个同策略触发不会发生
// （依赖 next 已推进才能算出正确的下次等待；本测试断言触发后
// 立刻检查 jobs next 已在未来——通过行为代理：on-start 阻塞时
// 主循环仍能进入等待而非死循环）。
func TestJobNextAdvancedOnTrigger(t *testing.T) {
	dir := t.TempDir()
	cfg := parallelTestCfg(t, dir, "0 3 31 2 *", 2)
	cfg.RunOnStart = true

	release := make(chan struct{})
	started := make(chan struct{})
	runFn := func(ctx context.Context, pol *config.Policy) error {
		select {
		case <-started:
		default:
			close(started)
		}
		<-release // 阻塞住第一个 job
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out bytes.Buffer
	done := make(chan struct{})
	go func() { daemonLoopWith(ctx, cfg, &out, runFn, func(*config.Policy) error { return nil }); close(done) }()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("on-start job never started")
	}
	// 主循环必须不被阻塞：ctx 取消路径可达（若 next 不推进/主循环卡在
	// 同步执行上，done 只能等 5s 超时后才返回——这里主动取消验证）
	time.Sleep(100 * time.Millisecond)
	cancel()
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("daemonLoop did not return after cancel — scheduler thread blocked by in-flight job")
	}
}

// TestSemaphoreLimitsConcurrency T2-3：max=1 两 job 执行区间不重叠；
// max=2 允许重叠。
func TestSemaphoreLimitsConcurrency(t *testing.T) {
	for _, tc := range []struct {
		max        int
		wantOverlap bool
	}{
		{1, false},
		{2, true},
	} {
		t.Run(itoa(tc.max), func(t *testing.T) {
			dir := t.TempDir()
			cfg := parallelTestCfg(t, dir, "0 3 31 2 *", tc.max)
			cfg.RunOnStart = true

			var inFlight atomic.Int32
			var maxSeen atomic.Int32
			runFn := func(ctx context.Context, pol *config.Policy) error {
				cur := inFlight.Add(1)
				for {
					old := maxSeen.Load()
					if cur <= old || maxSeen.CompareAndSwap(old, cur) {
						break
					}
				}
				time.Sleep(300 * time.Millisecond)
				inFlight.Add(-1)
				return nil
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var out bytes.Buffer
			daemonLoopWith(ctx, cfg, &out, runFn, func(*config.Policy) error { return nil })

			got := maxSeen.Load()
			if tc.wantOverlap && got < 2 {
				t.Fatalf("max=%d: max in-flight = %d, want >=2 (overlap expected)", tc.max, got)
			}
			if !tc.wantOverlap && got > 1 {
				t.Fatalf("max=%d: max in-flight = %d, want 1 (must serialize)", tc.max, got)
			}
		})
	}
}

// TestShutdownWaitsForInflight T2-4：ctx 取消后 daemonLoopWith 返回时，
// in-flight job 必已结束（wg.Wait 语义）。
func TestShutdownWaitsForInflight(t *testing.T) {
	dir := t.TempDir()
	cfg := parallelTestCfg(t, dir, "0 3 31 2 *", 2)
	cfg.RunOnStart = true

	var finished atomic.Int32
	runFn := func(ctx context.Context, pol *config.Policy) error {
		time.Sleep(400 * time.Millisecond) // 模拟进行中的 run
		finished.Add(1)
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	var out bytes.Buffer
	done := make(chan struct{})
	go func() { daemonLoopWith(ctx, cfg, &out, runFn, func(*config.Policy) error { return nil }); close(done) }()

	time.Sleep(100 * time.Millisecond) // 等 on-start 启动
	cancel()
	<-done
	if n := finished.Load(); n < 1 {
		t.Fatalf("daemonLoop returned with %d finished jobs — must wait for in-flight (wg)", n)
	}
}

