package trash

import (
	"os"
	"path/filepath"
	"testing"
)

// ENOENT→already_gone 分流（handover 2026-10-13 待办，2026-10-16 生产实锤：
// total=14911 失败全为 ENOENT——业务侧在同一目录批量清理，良性竞态）。
//
// 分流语义：rename/recheck 遇 fs.ErrNotExist 时，该文件计入 already_gone
// 而非 move_errors；MoveBatchBucket 聚合 err 不再包含 ENOENT（已走文件
// 不构成 run 级错误，这正是信噪比修复点）。审计 per-file result 保持
// fail-closed：仍记 ok:false + err 文本——文件不在桶里，restore 投影
// 不应认为该文件可恢复。
func TestMoveBatchBucketENOENTClassifiedAsAlreadyGone(t *testing.T) {
	layout, root, jsonlPath := setup(t)
	src1 := filepath.Join(root, "a/f1.txt")
	mustWrite(t, src1, "one")

	jl, err := OpenAudit(jsonlPath)
	if err != nil {
		t.Fatal(err)
	}
	defer jl.Close()
	m := NewMover(layout, jl)

	// 全 ENOENT 批：2 个 src 都不存在（扫描后被业务删走）。
	reqs := []MoveReq{
		{Src: filepath.Join(root, "a/gone1.txt"), Rel: "a/gone1.txt"},
		{Src: filepath.Join(root, "a/gone2.txt"), Rel: "a/gone2.txt"},
	}
	okCount, gone, err := m.MoveBatchBucket("r-eno", "p1", "2026-10-23.run-1", reqs, Recheck{})
	if err != nil {
		t.Fatalf("all-ENOENT batch must not surface aggregate error (already-gone is not a run failure): %v", err)
	}
	if okCount != 0 {
		t.Fatalf("ok count = %d, want 0", okCount)
	}
	if gone != 2 {
		t.Fatalf("already_gone = %d, want 2", gone)
	}

	// 混合批：1 真删成功 + 1 ENOENT + 1 真失败（dst 父目录不可建 → 非 ENOENT 错误）。
	// 真失败用 escape-root 构造不可实现——改用 recheck 路径：src 是目录导致
	// rename 到已存在目录失败?——不稳定。改直接断言聚合 err 判定语义即可（上面已覆盖）。
	// 此处补审计 fail-closed 断言：ENOENT 文件的 result 仍 ok:false。
	data, err := os.ReadFile(jsonlPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if got := countStr(content, `"ok":false`); got != 2 {
		t.Errorf("ok:false results = %d, want 2 (ENOENT files must stay fail-closed in audit)", got)
	}
	if !containsStr(content, "gone1.txt") || !containsStr(content, "no such file") {
		t.Errorf("ENOENT results must carry per-file src and error text:\n%s", content)
	}
}

// 快照模式（ScanInode>0）下的 ENOENT 同样分流：rename 直接报 ENOENT。
func TestMoveBatchBucketSnapshotENOENT(t *testing.T) {
	layout, root, jsonlPath := setup(t)
	jl, err := OpenAudit(jsonlPath)
	if err != nil {
		t.Fatal(err)
	}
	defer jl.Close()
	m := NewMover(layout, jl)

	// ScanInode 非 0 → 快照模式（rename → 事后核验），src 不存在 → rename ENOENT。
	reqs := []MoveReq{
		{Src: filepath.Join(root, "b/vanished.txt"), Rel: "b/vanished.txt", ScanInode: 42},
	}
	okCount, gone, err := m.MoveBatchBucket("r-eno2", "p1", "2026-10-23.run-2", reqs, Recheck{})
	if err != nil {
		t.Fatalf("snapshot ENOENT must not surface aggregate error: %v", err)
	}
	if okCount != 0 || gone != 1 {
		t.Fatalf("ok=%d gone=%d, want 0/1", okCount, gone)
	}
}

// 边界：非 ENOENT 的 rename 失败仍进 move_errors 路径（不误吞真错误）。
// 用只读目录内的 src：rename 源不可读 → EACCES，非 ENOENT。
func TestMoveBatchBucketNonENOENTStillFails(t *testing.T) {
	layout, root, jsonlPath := setup(t)
	src := filepath.Join(root, "ro/keep.txt")
	mustWrite(t, src, "x")
	// 源目录去写权限 → rename 出 EACCES（NFS/本地文件系统均成立）。
	if err := os.Chmod(filepath.Dir(src), 0o500); err != nil {
		t.Skipf("cannot chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(src), 0o700) })

	jl, err := OpenAudit(jsonlPath)
	if err != nil {
		t.Fatal(err)
	}
	defer jl.Close()
	m := NewMover(layout, jl)

	reqs := []MoveReq{{Src: src, Rel: "ro/keep.txt"}}
	okCount, gone, err := m.MoveBatchBucket("r-eacc", "p1", "2026-10-23.run-3", reqs, Recheck{})
	if err == nil {
		t.Fatal("EACCES must still surface aggregate error (only ENOENT is classified)")
	}
	if okCount != 0 || gone != 0 {
		t.Fatalf("ok=%d gone=%d, want 0/0 (EACCES is a real failure, not already-gone)", okCount, gone)
	}
}
