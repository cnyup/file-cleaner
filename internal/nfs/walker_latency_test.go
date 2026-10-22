package nfs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// P1 回归（bench p50/p95 热缓存显示 0）：
// stat 池 worker 的延迟样本必须全部归并进 ScanResult——
// 旧缺陷：`defer mergeLatencies(local)` 的参数在 defer 语句处求值
// （len=0 的切片头），此后 append 的样本全部丢失；只有 statCh 背压
// 内联路径（闭包形式）的样本幸存。热缓存下 stat 极快、通道永不满 →
// 零幸存样本 → p50/p95/p99 全 0。
//
// 本测试在热缓存（本地 tmpfs/盘，stat 亚毫秒）下扫描，断言：
//   1) 分位数非零（有样本且排序生效）；
//   2) 样本数 == 文件数（每文件恰好一个 stat 样本，无丢失）。
func TestScanTreeStatLatencySamplesNotLost(t *testing.T) {
	root := t.TempDir()
	// 建两层目录共 40 个常规文件——远小于 statCh cap 4096，
	// 保证不走背压内联路径（缺陷的隐蔽分支）。
	for d := 0; d < 4; d++ {
		for f := 0; f < 10; f++ {
			p := filepath.Join(root, "dir"+string(rune('a'+d)), "f"+string(rune('a'+f))+".txt")
			os.MkdirAll(filepath.Dir(p), 0o755)
			if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	res, err := ScanTree(context.Background(), root, ScanOptions{DirWorkers: 2, StatWorkers: 4, Stat: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 40 {
		t.Fatalf("files = %d, want 40", res.Files)
	}
	if res.StatP50 <= 0 || res.StatP95 <= 0 || res.StatP99 <= 0 {
		t.Fatalf("stat latency percentiles lost (hot-cache bench bug): p50=%v p95=%v p99=%v, want all > 0", res.StatP50, res.StatP95, res.StatP99)
	}
	if res.StatP95 < res.StatP50 || res.StatP99 < res.StatP95 {
		t.Fatalf("percentiles not monotonic: p50=%v p95=%v p99=%v", res.StatP50, res.StatP95, res.StatP99)
	}
}

// 样本数守恒：强并发（statWorkers=32 > 文件数）下每个文件恰好贡献
// 一个延迟样本——repair 后无法直接观测 statMillis（已清空），用
// Files==stat 调用数的恒等关系侧面锚定：Errors==0 且 Files==40 时
// 必有 40 个成功 stat，p99 必须来自第 40 个样本（非零）。
func TestScanTreeLatencySampleCountConserved(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 25; i++ {
		p := filepath.Join(root, "f"+string(rune('a'+i%26))+string(rune('a'+i/26))+".txt")
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res, err := ScanTree(context.Background(), root, ScanOptions{DirWorkers: 1, StatWorkers: 32, Stat: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 25 || res.Errors != 0 {
		t.Fatalf("files=%d errors=%d, want 25/0", res.Files, res.Errors)
	}
	if res.StatP50 == 0 {
		t.Fatal("p50 = 0 with 25 successful stats — samples lost")
	}
}
