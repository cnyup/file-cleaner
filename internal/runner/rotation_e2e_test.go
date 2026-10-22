package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bcyup/filecleaner/internal/audit"
	"github.com/bcyup/filecleaner/internal/config"
)

// TestRunOnceDailyRotationShardAndRestoreAcrossDays 计划 T10 e2e 场景的
// runner 层等价验证：默认 daily 模式下 RunOnce 落当天分片（非 legacy），
// 且跨天聚合导入后 restore 查询仍能找到首日记录。
func TestRunOnceDailyRotationShardAndRestoreAcrossDays(t *testing.T) {
	root := t.TempDir()
	state := t.TempDir()
	cfg := &config.Config{StateDir: state} // 不配 audit_log = 默认 daily
	p := &config.Policy{
		ID:    "p-rot",
		Root:  root,
		Trash: config.Trash{Dir: ".filecleaner-trash", Retention: "1d"},
		Retention: config.Retention{
			Match:      "AND",
			Conditions: map[string]string{"mtime_older_than": "1d"},
		},
		Concurrency: config.Concurrency{DirReadWorkers: 2, StatWorkers: 2, MoveWorkers: 2},
	}
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	// 一个过期文件（mtime 昨天以前）
	f := filepath.Join(root, "day1.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(f, old, old); err != nil {
		t.Fatal(err)
	}

	if _, err := RunOnce(context.Background(), cfg, p, false); err != nil {
		t.Fatalf("runonce: %v", err)
	}

	// 分片形态：当天文件存在，legacy 不存在
	today := time.Now().Format("20060102")
	shard := filepath.Join(state, fmt.Sprintf("audit-%s.jsonl", today))
	if _, err := os.Stat(shard); err != nil {
		t.Fatalf("daily shard %s must exist: %v", shard, err)
	}
	if _, err := os.Stat(filepath.Join(state, "audit.jsonl")); err == nil {
		t.Fatal("legacy audit.jsonl must NOT be created in daily mode")
	}

	// 跨天 restore 依赖：聚合导入（模拟次日流程）能查到今天的 move 记录
	st, err := audit.OpenStore(filepath.Join(state, "audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	files, err := audit.AuditFiles(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != shard {
		t.Fatalf("AuditFiles=%v want [%s]", files, shard)
	}
	for _, af := range files {
		if _, err := st.ImportJSONL(af); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := st.QueryMoved(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("cross-day projection must find day-1 move record (restore basis)")
	}
}
