package trash

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bcyup/filecleaner/internal/audit"
)

// TestBucketedLayout 分桶布局：TrashPath 带 runID 时落在 日期/run 桶内，
// 原相对路径结构保留；默认（无 runID）回退旧扁平路径（迁移兼容）。
func TestBucketedLayout(t *testing.T) {
	layout, root, _ := setup(t)
	_ = jlOpen(t, root)

	// 桶名 = 日期.run-id 整体：活跃桶路径
	p := layout.BucketPath("2026-09-29.run-abc", "a/b/f.txt")
	want := filepath.Join(root, ".filecleaner-trash", "2026-09-29.run-abc", "a", "b", "f.txt")
	if p != want {
		t.Fatalf("BucketPath = %q, want %q", p, want)
	}
	// 旧扁平路径仍可用（迁移/兼容）
	if got := layout.TrashPath("a/b/f.txt"); got != filepath.Join(root, ".filecleaner-trash", "a", "b", "f.txt") {
		t.Fatalf("flat TrashPath = %q", got)
	}
}

// TestRetireBucketIsORename retire = 整桶 rename 进 .retired，O(1)：
// 断言桶目录原样出现在 .retired 下且活跃桶消失。
func TestRetireBucketIsORename(t *testing.T) {
	layout, root, _ := setup(t)
	jl := jlOpen(t, root)
	defer jl.Close()
	m := NewMover(layout, jl)

	// 建桶并移入 2 文件
	reqs := []MoveReq{
		{Src: filepath.Join(root, "f1.txt"), Rel: "f1.txt"},
		{Src: filepath.Join(root, "f2.txt"), Rel: "f2.txt"},
	}
	mustWrite(t, reqs[0].Src, "v1")
	mustWrite(t, reqs[1].Src, "v2")
	bucket := layout.NewBucketName(time.Now())
	if _, err := m.MoveBatchBucket("r1", "p1", bucket, reqs, Recheck{}); err != nil {
		t.Fatalf("MoveBatchBucket: %v", err)
	}
	bucketDir := layout.BucketDir(bucket)
	if _, err := os.Stat(bucketDir); err != nil {
		t.Fatalf("bucket dir must exist: %v", err)
	}

	// retire：O(1) rename
	if err := m.Retire("p1", bucket); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	retired := filepath.Join(layout.RetiredDir(), bucket)
	if _, err := os.Stat(retired); err != nil {
		t.Fatalf("retired bucket must exist: %v", err)
	}
	if _, err := os.Stat(bucketDir); !os.IsNotExist(err) {
		t.Fatal("active bucket must be gone after retire")
	}
	// 桶内文件原样保留（等待后台物理删除）
	for _, f := range []string{"f1.txt", "f2.txt"} {
		if _, err := os.Stat(filepath.Join(retired, f)); err != nil {
			t.Fatalf("retired file %s must remain until swept: %v", f, err)
		}
	}
}

// TestRestoreFromBucket 恢复语义：活跃桶可恢复；retire 后拒绝。
func TestRestoreFromBucket(t *testing.T) {
	layout, root, _ := setup(t)
	jl := jlOpen(t, root)
	defer jl.Close()
	m := NewMover(layout, jl)

	src := filepath.Join(root, "x.txt")
	mustWrite(t, src, "data")
	bucket := layout.NewBucketName(time.Now())
	if _, err := m.MoveBatchBucket("r1", "p1", bucket, []MoveReq{{Src: src, Rel: "x.txt"}}, Recheck{}); err != nil {
		t.Fatal(err)
	}
	// 活跃桶恢复成功
	if err := m.RestoreFromBucket("r2", "p1", bucket, "x.txt"); err != nil {
		t.Fatalf("restore from active bucket: %v", err)
	}
	if b, _ := os.ReadFile(src); string(b) != "data" {
		t.Fatal("restored content mismatch")
	}

	// 再移入并 retire，恢复必须拒绝
	mustWrite(t, src, "data2")
	if _, err := m.MoveBatchBucket("r3", "p1", bucket, []MoveReq{{Src: src, Rel: "x.txt"}}, Recheck{}); err != nil {
		t.Fatal(err)
	}
	if err := m.Retire("p1", bucket); err != nil {
		t.Fatal(err)
	}
	if err := m.RestoreFromBucket("r4", "p1", bucket, "x.txt"); err == nil {
		t.Fatal("restore after retire must be refused")
	}
}

// jlOpen 测试辅助：开审计文件（bucket 测试共用 setup）。
func jlOpen(t *testing.T, root string) *audit.JSONL {
	t.Helper()
	jl, err := OpenAudit(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return jl
}
