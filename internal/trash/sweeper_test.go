package trash

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSweepRetiredBucket sweeper：.retired 桶 → .purging → 物理删除 → 清空。
func TestSweepRetiredBucket(t *testing.T) {
	layout, root, _ := setup(t)
	jl := jlOpen(t, root)
	defer jl.Close()
	m := NewMover(layout, jl)

	// 构造一个 retired 桶（含嵌套文件）
	bucket := layout.NewBucketName(nowForTest())
	retired := filepath.Join(layout.RetiredDir(), bucket)
	os.MkdirAll(filepath.Join(retired, "a/b"), 0o755)
	os.WriteFile(filepath.Join(retired, "a/b/f1"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(retired, "f2"), []byte("x"), 0o644)

	n, err := m.SweepOnce()
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if n != 2 {
		t.Fatalf("swept = %d, want 2", n)
	}
	// 桶目录连同文件全部消失
	if _, err := os.Stat(retired); !os.IsNotExist(err) {
		t.Fatal("retired bucket must be fully removed")
	}
}

// TestSweepCrashResume .purging 中断恢复：残留标记桶重启后续删（幂等）。
func TestSweepCrashResume(t *testing.T) {
	layout, root, _ := setup(t)
	jl := jlOpen(t, root)
	defer jl.Close()
	m := NewMover(layout, jl)

	bucket := layout.NewBucketName(nowForTest())
	purging := filepath.Join(layout.PurgingDir(), bucket)
	os.MkdirAll(filepath.Join(purging, "sub"), 0o755)
	os.WriteFile(filepath.Join(purging, "sub", "f"), []byte("x"), 0o644)

	n, err := m.SweepOnce()
	if err != nil {
		t.Fatalf("SweepOnce (resume): %v", err)
	}
	if n != 1 {
		t.Fatalf("resumed swept = %d, want 1", n)
	}
	if _, err := os.Stat(purging); !os.IsNotExist(err) {
		t.Fatal("purging bucket must be removed after resume")
	}
}

// TestSweepIgnoresActive sweeper 不得触碰活跃桶与回收站外内容。
func TestSweepIgnoresActive(t *testing.T) {
	layout, root, _ := setup(t)
	jl := jlOpen(t, root)
	defer jl.Close()
	m := NewMover(layout, jl)

	bucket := layout.NewBucketName(nowForTest())
	active := layout.BucketDir(bucket)
	os.MkdirAll(active, 0o755)
	os.WriteFile(filepath.Join(active, "f"), []byte("x"), 0o644)

	n, err := m.SweepOnce()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("swept = %d, want 0 (active buckets untouched)", n)
	}
	if _, err := os.Stat(filepath.Join(active, "f")); err != nil {
		t.Fatal("active bucket file must remain")
	}
}
