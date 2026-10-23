package audit

import (
	"os"
	"path/filepath"
	"testing"
)

// P2 验收：rmdir 尾批审计事务写失败必须返回非 nil error——runner 依赖
// 该返回值把失败计入 empty_dirs_errors（42f919d 之前此路径被 _ = 静默）。
// 组件级证据：生产制造审计盘故障不可接受。
func TestCommitRmdirsAuditWriteFailureSurfaces(t *testing.T) {
	// jsonlPath 指向一个目录：OpenJSONL 对目录 O_WRONLY 会失败，
	// 后续 CommitRmdirs 内部的 appendLocked 写入失败必须冒泡为返回值。
	dir := t.TempDir()
	blocked := filepath.Join(dir, "audit.jsonl")
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	jl, err := OpenJSONL(filepath.Join(dir, "shard.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	// 把底层文件换成目录路径：写入必然失败。
	if err := jl.f.Close(); err != nil {
		t.Fatal(err)
	}
	jl.f = nil

	intents := []RmdirIntent{{RunID: "r1", PolicyID: "p1", Path: "/tmp/x"}}
	rerr := jl.CommitRmdirs(intents, func() []error { return nil })
	if rerr == nil {
		t.Fatal("CommitRmdirs must surface audit write failure as error (runner counts it into empty_dirs_errors)")
	}
}
