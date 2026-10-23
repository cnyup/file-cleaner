package trash

import (
	"os"
	"path/filepath"
	"testing"
)

// P0 回归 2+3：MoveBatchBucket 必须返回批内物理成功数，供 runner 把
// RunStats.MovedOK/MoveErrors 校准为物理移动口径（旧缺陷：OnMove 入队
// 成功即计数，批结果对 pipeline 失明；尾批 flush() 错误被 _ = 丢弃，
// 尾批失败连 MoveErrors 都不进）。
//
// 注入方式：批内 3 个请求，其中 1 个 Src 指向不存在路径（模拟扫描后
// 文件被业务删走）→ 该条 rename ENOENT 物理失败，其余 2 条成功。
func TestMoveBatchBucketPartialFailurePhysicalCount(t *testing.T) {
	layout, root, jsonlPath := setup(t)
	src1 := filepath.Join(root, "a/f1.txt")
	src2 := filepath.Join(root, "a/f2.txt")
	mustWrite(t, src1, "one")
	mustWrite(t, src2, "two")

	jl, err := OpenAudit(jsonlPath)
	if err != nil {
		t.Fatal(err)
	}
	defer jl.Close()
	m := NewMover(layout, jl)

	reqs := []MoveReq{
		{Src: src1, Rel: "a/f1.txt"},
		{Src: filepath.Join(root, "a/gone.txt"), Rel: "a/gone.txt"}, // 扫描后消失 → 物理失败
		{Src: src2, Rel: "a/f2.txt"},
	}
	okCount, goneCount, err := m.MoveBatchBucket("r1", "p1", "2026-10-15.run-1", reqs, Recheck{})
	if err != nil {
		// 2026-10-23 ENOENT 分流后：批内唯一失败是 gone.txt 的 ENOENT（良性），
		// 聚合错误不再包含 already-gone——原断言「must surface aggregate error」
		// 随分流语义作废。
		t.Fatalf("ENOENT-only failure is already_gone, not aggregate error: %v", err)
	}
	if okCount != 2 {
		t.Fatalf("physical ok count = %d, want 2 (physical, not enqueue, semantics)", okCount)
	}
	if goneCount != 1 {
		t.Fatalf("already_gone = %d, want 1", goneCount)
	}

	// 物理：2 个文件在桶内，src 消失；失败者 src 本就不存在。
	bucket := layout.BucketDir("2026-10-15.run-1")
	for _, rel := range []string{"a/f1.txt", "a/f2.txt"} {
		if _, err := os.Lstat(filepath.Join(bucket, rel)); err != nil {
			t.Errorf("physically moved file missing in bucket: %s (%v)", rel, err)
		}
	}

	// 审计：3 条 result——2 条 ok:true、1 条 ok:false 且 err 归因到该文件。
	data, err := os.ReadFile(jsonlPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if got := countStr(content, `"type":"result"`); got != 3 {
		t.Fatalf("result events = %d, want 3", got)
	}
	if got := countStr(content, `"ok":true`); got != 2 {
		t.Errorf("ok:true results = %d, want 2 (successful files must not be marked failed)", got)
	}
	if got := countStr(content, `"ok":false`); got != 1 {
		t.Errorf("ok:false results = %d, want 1", got)
	}
	if !containsStr(content, "a/gone.txt") || !containsStr(content, "no such file") {
		t.Errorf("failed result must carry the failing file's own src and error text:\n%s", content)
	}
}

func countStr(s, sub string) int {
	n := 0
	for i := 0; ; {
		j := indexOfStr(s, sub, i)
		if j < 0 {
			return n
		}
		n++
		i = j + len(sub)
	}
}

func indexOfStr(s, sub string, from int) int {
	for i := from; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func containsStr(s, sub string) bool { return indexOfStr(s, sub, 0) >= 0 }
