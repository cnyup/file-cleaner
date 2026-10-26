package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bcyup/filecleaner/internal/config"
	"github.com/bcyup/filecleaner/internal/scheduler"
)

// TestPurgeWithRetryNonBusyErrorNoRetry：非 busy 错误（如 retention 解析
// 失败）必须立即返回，不进入 retry——只有锁忙才值得等。
// 观测：interval 5s 下若错误地重试了非 busy 错误，耗时必然 ≥5s。
func TestPurgeWithRetryNonBusyErrorNoRetry(t *testing.T) {
	state := t.TempDir()
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	// retention 非法 → Purge 拿锁成功后在解析处失败（非 busy 路径）
	p := &config.Policy{
		ID:    "p-badret",
		Root:  t.TempDir(),
		Trash: config.Trash{Dir: ".filecleaner-trash", Retention: "not-a-duration"},
	}
	start := time.Now()
	_, err := PurgeWithRetry(context.Background(), &config.Config{StateDir: state}, p, 5*time.Second)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("bad retention must surface error")
	}
	if errors.Is(err, scheduler.ErrPolicyBusy) {
		t.Fatalf("err = %v — must not be busy (lock is free)", err)
	}
	if elapsed >= time.Second {
		t.Fatalf("elapsed = %v — non-busy error must return immediately, no retry", elapsed)
	}
}

// TestPurgeWithRetryWaitsForLock：主 run 持锁 250ms，purge 以 10ms
// 间隔 retry，必须在 run 释放后完成 retire。
// 语义：purge 欠账不再因 busy-skip 丢到次日。
func TestPurgeWithRetryWaitsForLock(t *testing.T) {
	root := t.TempDir()
	state := t.TempDir()
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{StateDir: state}
	p := &config.Policy{
		ID:    "p-retry",
		Root:  root,
		Trash: config.Trash{Dir: ".filecleaner-trash", Retention: "1h"},
	}
	// 构造已过期桶（昨天日期 + 一个文件）
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	bucket := yesterday + ".run-abc"
	bucketDir := filepath.Join(root, ".filecleaner-trash", bucket)
	if err := os.MkdirAll(filepath.Join(bucketDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bucketDir, "sub", "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 主 run 持锁 250ms 后释放
	guard, err := scheduler.Acquire(state, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(250*time.Millisecond, guard.Release)

	start := time.Now()
	retired, err := PurgeWithRetry(context.Background(), cfg, p, 10*time.Millisecond)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("purge with retry: %v", err)
	}
	if len(retired) != 1 || retired[0] != bucket {
		t.Fatalf("retired = %v, want [%s]", retired, bucket)
	}
	// retire 语义：桶进入 .retired
	retiredDir := filepath.Join(root, ".filecleaner-trash", ".retired", bucket)
	if _, err := os.Stat(retiredDir); err != nil {
		t.Fatalf("bucket must be in .retired after purge: %v", err)
	}
	// retry 生效证据：首试必 busy，等锁释放后才成功 → 总耗时贴近持锁时长
	if elapsed < 150*time.Millisecond {
		t.Fatalf("elapsed = %v — purge must have waited for lock (first attempt busy)", elapsed)
	}
}

// TestPurgeWithRetryCtxCancel：ctx 取消时 retry 循环必须及时退出并
// 返回 ctx 错误——daemon 停机不被 retry 拖住。
func TestPurgeWithRetryCtxCancel(t *testing.T) {
	state := t.TempDir()
	guard, err := scheduler.Acquire(state, "p-cancel")
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Release()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(80 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err = PurgeWithRetry(ctx, &config.Config{StateDir: state}, &config.Policy{ID: "p-cancel"}, 10*time.Millisecond)
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed > time.Second {
		t.Fatalf("elapsed = %v — retry loop must exit promptly on ctx cancel", elapsed)
	}
}
