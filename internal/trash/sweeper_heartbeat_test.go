package trash

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestSweepOnceHeartbeat C2（2026-11-02 晨报仍开放项）：SweepOnce 长跑
// 期间必须周期性打进度心跳——11-01 video sweep 长跑 1h55m，journal 只有
// 轮首/轮尾两行，期间完全静默（每策略串行，后位策略整轮等位）。
// 心跳行含 policy 与 files/dirs 计数，使"删到哪了"在长跑中可观测。
// 首条心跳在删除开始时立即发出（不等第一个周期）——快测试无需真实等待。
func TestSweepOnceHeartbeat(t *testing.T) {
	layout, root, _ := setup(t)
	jl := jlOpen(t, root)
	defer jl.Close()
	m := NewMover(layout, jl)
	m.SetPolicyID("p-video")

	// 造一个 retired 桶：多文件，删除期间心跳可报进度。
	bucket := layout.NewBucketName(nowForTest())
	retired := filepath.Join(layout.RetiredDir(), bucket)
	if err := os.MkdirAll(filepath.Join(retired, "a/b"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		p := filepath.Join(retired, "a/b", string(rune('f'+i)))
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	var lines []string
	m.SetHeartbeat(func(msg string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, msg)
	})

	m.SweepOnce()

	mu.Lock()
	defer mu.Unlock()
	if len(lines) == 0 {
		t.Fatal("no heartbeat lines during sweep — long-run blind spot stays")
	}
	for _, l := range lines {
		if !strings.Contains(l, "p-video") {
			t.Fatalf("heartbeat line missing policy id: %q", l)
		}
	}
}

// TestSweepOnceHeartbeatNil 未注入回调时行为不变（默认关闭，零开销）。
func TestSweepOnceHeartbeatNil(t *testing.T) {
	layout, root, _ := setup(t)
	jl := jlOpen(t, root)
	defer jl.Close()
	m := NewMover(layout, jl)

	bucket := layout.NewBucketName(nowForTest())
	retired := filepath.Join(layout.RetiredDir(), bucket)
	if err := os.MkdirAll(retired, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(retired, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.SweepOnce(); err != nil {
		t.Fatalf("SweepOnce with nil heartbeat: %v", err)
	}
}
