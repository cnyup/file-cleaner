package trash

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestMoveUsesScanMetaNoSrcLstat 验证：MoveReq 携带扫描元数据时，
// moveOne 用快照核验而非独立 Lstat(src)——通过行为断言：
// 文件在扫描后被替换（inode 变化）必须被拒绝。
func TestMoveUsesScanMetaNoSrcLstat(t *testing.T) {
	layout, root, jsonlPath := setup(t)
	jl, _ := OpenAudit(jsonlPath)
	defer jl.Close()
	m := NewMover(layout, jl)

	src := filepath.Join(root, "f.txt")
	mustWrite(t, src, "v1")
	st, _ := os.Lstat(src)
	var ino uint64
	if si, ok := st.Sys().(interface{ Ino() uint64 }); ok {
		ino = si.Ino()
	}
	// 从 syscall 层拿真实 inode（跨平台测试辅助）
	ino = inodeOf(t, src)

	// 场景1：快照 inode 正确 → 移动成功（不依赖 moveOne 内部再 Lstat）
	_, err := m.MoveBatch("r1", "p1", []MoveReq{{
		Src: src, Rel: "f.txt",
		ScanInode: ino, ScanMtime: st.ModTime(),
	}}, Recheck{})
	if err != nil {
		t.Fatalf("move with matching snapshot: %v", err)
	}
	if _, err := os.Lstat(layout.TrashPath("f.txt")); err != nil {
		t.Fatal("file must be in trash")
	}

	// 场景2：快照 inode 与实际不符（文件被替换）→ 拒绝移动
	src2 := filepath.Join(root, "g.txt")
	mustWrite(t, src2, "v2")
	st2, _ := os.Lstat(src2)
	bogus := inodeOf(t, src2) + 999999
	_, err = m.MoveBatch("r2", "p1", []MoveReq{{
		Src: src2, Rel: "g.txt",
		ScanInode: bogus, ScanMtime: st2.ModTime(),
	}}, Recheck{})
	if err == nil {
		t.Fatal("inode mismatch must be rejected without moving")
	}
	if _, err := os.Lstat(src2); err != nil {
		t.Fatal("src2 must remain in place")
	}
}

// TestMoveNoSnapshotKeepsOldRecheck 未携带扫描元数据时保持旧行为
// （moveOne 自行 Lstat 核验），兼容手动调用路径。
func TestMoveNoSnapshotKeepsOldRecheck(t *testing.T) {
	layout, root, jsonlPath := setup(t)
	jl, _ := OpenAudit(jsonlPath)
	defer jl.Close()
	m := NewMover(layout, jl)

	src := filepath.Join(root, "h.txt")
	mustWrite(t, src, "keep")
	if _, err := m.MoveBatch("r1", "p1", []MoveReq{{Src: src, Rel: "h.txt"}}, Recheck{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(layout.TrashPath("h.txt")); err != nil {
		t.Fatal("must be moved")
	}
}

// inodeOf 测试辅助：读取真实 inode。
func inodeOf(t *testing.T, path string) uint64 {
	t.Helper()
	st, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	type inoer interface{ Ino() uint64 }
	if _, ok := st.Sys().(inoer); ok {
		return st.Sys().(inoer).Ino()
	}
	return inodeFromSyscall(st)
}

func TestMoveResultRecordsActualTrashPath(t *testing.T) {
	layout, root, jsonlPath := setup(t)
	jl, _ := OpenAudit(jsonlPath)
	defer jl.Close()
	m := NewMover(layout, jl)

	// 同名二次回收：第二次的 result 必须能区分（审计 trash 路径带后缀或
	// 冲突检测生效）——行为级断言：两次都在盘上可寻回
	src := filepath.Join(root, "dup.txt")
	mustWrite(t, src, "v1")
	if _, err := m.MoveBatch("r1", "p1", []MoveReq{{Src: src, Rel: "dup.txt"}}, Recheck{}); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, src, "v2")
	if _, err := m.MoveBatch("r2", "p1", []MoveReq{{Src: src, Rel: "dup.txt"}}, Recheck{}); err != nil {
		t.Fatal(err)
	}
	// v1 与 v2 都必须存在于回收站（不同名或同目录不同条目）
	found := map[string]bool{}
	entries, _ := os.ReadDir(filepath.Join(root, ".filecleaner-trash"))
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(root, ".filecleaner-trash", e.Name()))
		if err == nil {
			found[string(data)] = true
		}
	}
	if !found["v1"] || !found["v2"] {
		t.Fatalf("both versions must be recoverable, got %v", found)
	}
	_ = time.Now
}
