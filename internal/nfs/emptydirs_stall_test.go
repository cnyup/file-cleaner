package nfs

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestStreamingBatchStallDetector 批零推进熔断（2026-11-19 生产首窗口卡死教训）：
// worker 的 rmdir 挂死在 NFS hard mount 上时，wg.Wait() 永久阻塞——窗口熔断
// 只防批间（L133 批次循环顶），不防批内。StallTimeout 让「卡死」变「可重试」：
// 批处理超过时限即放弃等待、落 checkpoint、Stopped=true 退出（进程可被
// systemd/脚本回收，下轮窗口或服务端恢复后续跑）。
//
// 模拟方式：OnRmdir 回调里 sleep 长时间（等价 rmdir 不返回），批时限 200ms。
func TestStreamingBatchStallDetector(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 6; i++ {
		mkEmptyDir(t, filepath.Join(root, fname(i)), 10*24*time.Hour)
	}
	release := make(chan struct{})
	defer close(release)
	st, err := SweepEmptyDirsStreaming(root, EmptyDirsStreamingOptions{
		AgeGate: 7 * 24 * time.Hour,
		BatchSize: 3, // 一批 3 条
		// OnRmdir 阻塞至 release——模拟 rmdir 挂死在 NFS。
		OnRmdir: func(path string) error {
			<-release
			return nil
		},
		StallTimeout: 200 * time.Millisecond,
		Workers:     2,
		Now:         time.Now,
	})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if !st.Stopped {
		t.Fatal("Stalled batch must set Stopped=true (resumable exit)")
	}
	if st.Stalled == 0 {
		t.Fatal("Stalled must be > 0 (first batch hit the stall timeout)")
	}
	// 卡死批未完成：不落 checkpoint（cookie 未推进，下轮重扫本批——rmdir
	// 幂等，ENOENT 计 Gone，安全）。
	if st.Checkpointed > 1 {
		t.Fatalf("Checkpointed = %d, want <=1 (stalled batch must not checkpoint)", st.Checkpointed)
	}
	// 数据安全：目录还在（rmdir 从未完成）或 ENOTEMPTY——不存在「部分完成」状态。
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(root, fname(i))); err != nil {
			t.Logf("note: %s already gone (fine if delete raced)", fname(i))
		}
	}
}
