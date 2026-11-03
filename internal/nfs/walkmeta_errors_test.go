package nfs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWalkMetaErrorSamples N1（2026-11-03 巡检观察①）：scan 期错误必须留痕
// 路径样本——当日 errors=2 但无逐条明细，事后无法回查是哪 2 个路径、何种
// 错误，只能排除法定性。前 8 条样本随 WalkMeta 返回值交付，供 runner
// 写入 run_finished extra。
func TestWalkMetaErrorSamples(t *testing.T) {
	root := t.TempDir()
	// 正常目录 + 文件：遍历可完成。
	if err := os.MkdirAll(filepath.Join(root, "ok"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ok", "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 权限拒绝目录：进入时 os.Open 失败 → 采样点之一。
	if err := os.Mkdir(filepath.Join(root, "denied"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "denied"), 0o755) })

	ws, err := WalkMeta(context.Background(), root, WalkOptions{DirWorkers: 1, StatWorkers: 1}, func(rel string, meta FileMetaFull) {})
	if err != nil {
		t.Fatalf("WalkMeta: %v", err)
	}
	if ws.Errs < 1 {
		t.Fatalf("expected >=1 walk error, got %d", ws.Errs)
	}
	samples := ws.ErrSamples
	if len(samples) == 0 {
		t.Fatal("no error samples — scan errors stay unattributable (11-03 gap)")
	}
	if len(samples) > 8 {
		t.Fatalf("sample cap exceeded: %d", len(samples))
	}
	found := false
	for _, s := range samples {
		if strings.Contains(s, "denied") && strings.Contains(s, "permission denied") {
			found = true
		}
	}
	if !found {
		t.Fatalf("denied-dir sample missing: %v", samples)
	}
}

// TestWalkMetaErrorSamplesReset 样本不跨轮残留：修复权限后错误消失，
// 同一 root 再走一遍 → 新一轮的样本必须为空。
func TestWalkMetaErrorSamplesReset(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 第一次：带错误目录。
	d := filepath.Join(root, "denied")
	if err := os.Mkdir(d, 0o000); err != nil {
		t.Fatal(err)
	}
	ws1, _ := WalkMeta(context.Background(), root, WalkOptions{DirWorkers: 1}, func(rel string, m FileMetaFull) {})
	if len(ws1.ErrSamples) == 0 {
		t.Fatal("first walk should sample the error")
	}
	// 第二次：修复权限后错误消失 → 样本必须为空（per-walk 交付，天然
	// 不残留；保留断言防回归到包级全局）。
	if err := os.Chmod(d, 0o755); err != nil {
		t.Fatal(err)
	}
	ws2, _ := WalkMeta(context.Background(), root, WalkOptions{DirWorkers: 1}, func(rel string, m FileMetaFull) {})
	if got := ws2.ErrSamples; len(got) != 0 {
		t.Fatalf("samples must reset per walk, got %v", got)
	}
}
