package trash

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSweepSkeletonBucketDirsCounted 骨架桶回归锚定（2026-10-29 巡检盲区）：
// 外部 skeletonize 后的桶（0 文件、纯目录树）物理删除时 files=0 但
// dirs>0。旧签名只返回 files → SweepLoop n==0 静默、零审计——本用例
// 钉死新语义：(files, dirs) 双计数 + sweep_result 事件落盘。
func TestSweepSkeletonBucketDirsCounted(t *testing.T) {
	layout, root, _ := setup(t)
	jl := jlOpen(t, root)
	defer jl.Close()
	m := NewMover(layout, jl)

	// 骨架桶：a/b/c 嵌套空目录，0 文件。
	bucket := layout.NewBucketName(nowForTest())
	retired := filepath.Join(layout.RetiredDir(), bucket)
	os.MkdirAll(filepath.Join(retired, "a/b/c"), 0o755)

	files, dirs, err := m.SweepOnce()
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if files != 0 {
		t.Fatalf("files = %d, want 0 (skeleton bucket)", files)
	}
	if dirs != 4 { // a/b/c + 桶根
		t.Fatalf("dirs = %d, want 4 (a, a/b, a/b/c, bucket root)", dirs)
	}
	if _, err := os.Stat(retired); !os.IsNotExist(err) {
		t.Fatal("skeleton bucket must be fully removed")
	}
}

// TestSweepResultEventWritten sweep_result 事件落盘断言：正常桶（含文件）
// 也必须有一条 type=sweep_result、ok=true 的事件，extra 含 files/dirs。
func TestSweepResultEventWritten(t *testing.T) {
	layout, root, _ := setup(t)
	jl := jlOpen(t, root)
	defer jl.Close()
	m := NewMover(layout, jl)

	bucket := layout.NewBucketName(nowForTest())
	retired := filepath.Join(layout.RetiredDir(), bucket)
	os.MkdirAll(filepath.Join(retired, "a/b"), 0o755)
	os.WriteFile(filepath.Join(retired, "a/b/f1"), []byte("x"), 0o644)

	files, dirs, err := m.SweepOnce()
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if files != 1 || dirs != 3 {
		t.Fatalf("files=%d dirs=%d, want 1/3", files, dirs)
	}

	// sweep_result 事件必须已在审计分片里
	f, err := os.Open(jl.Path())
	if err != nil {
		t.Fatalf("open audit jsonl: %v", err)
	}
	defer f.Close()
	found := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		var ev map[string]any
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		if ev["type"] != "sweep_result" {
			continue
		}
		found = true
		if ev["ok"] != true {
			t.Fatalf("sweep_result ok = %v, want true", ev["ok"])
		}
		extra, _ := ev["extra"].(map[string]any)
		if extra["files"] != float64(1) {
			t.Fatalf("sweep_result extra.files = %v, want 1", extra["files"])
		}
		if extra["dirs"] != float64(3) {
			t.Fatalf("sweep_result extra.dirs = %v, want 3", extra["dirs"])
		}
	}
	if !found {
		t.Fatal("no sweep_result event in audit jsonl — sweeper must write one per SweepOnce")
	}
}

// TestSweepResultEventEmptyRound 空轮（无桶可处理）也写 sweep_result：
// "跑过且无事"本身是信息（区分 sweeper 死了 vs 没活干）。
func TestSweepResultEventEmptyRound(t *testing.T) {
	layout, root, _ := setup(t)
	jl := jlOpen(t, root)
	defer jl.Close()
	m := NewMover(layout, jl)

	if _, _, err := m.SweepOnce(); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	b, err := os.ReadFile(jl.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "\"type\":\"sweep_result\"") {
		t.Fatal("empty round must still write sweep_result (proof of life)")
	}
}

// 忽略未用告警（time 供 nowForTest 注入方使用习惯保留）。
var _ = time.Now
