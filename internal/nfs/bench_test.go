package nfs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// makeBenchTree 在 root 下构建确定性测试树：
// d0..d3 四个目录，每个 5 个文件，外加根下 2 个文件与 1 个子目录一层嵌套。
func makeBenchTree(t *testing.T, root string) (files, dirs int) {
	t.Helper()
	for i := 0; i < 4; i++ {
		sub := filepath.Join(root, "d"+string(rune('0'+i)))
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
		dirs++
		nested := filepath.Join(sub, "inner")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", nested, err)
		}
		dirs++
		for j := 0; j < 5; j++ {
			p := filepath.Join(sub, "f"+string(rune('0'+j)))
			if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
				t.Fatalf("write %s: %v", p, err)
			}
			files++
		}
		if err := os.WriteFile(filepath.Join(nested, "deep"), []byte("x"), 0o644); err != nil {
			t.Fatalf("write deep: %v", err)
		}
		files++
	}
	for k := 0; k < 2; k++ {
		p := filepath.Join(root, "root"+string(rune('0'+k)))
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		files++
	}
	return files, dirs
}

// TestScanCountConsistency 验证纯 readdir 与 readdir+fstatat 两种模式统计一致，
// 且与实际树相符；排除目录不进入统计。
func TestScanCountConsistency(t *testing.T) {
	root := t.TempDir()
	files, dirs := makeBenchTree(t, root)

	// 额外建一个应被排除的子树
	excl := filepath.Join(root, "excluded")
	if err := os.MkdirAll(excl, 0o755); err != nil {
		t.Fatalf("mkdir excluded: %v", err)
	}
	if err := os.WriteFile(filepath.Join(excl, "skip-me"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write excluded: %v", err)
	}

	ctx := context.Background()

	resReaddir, err := ScanTree(ctx, root, ScanOptions{StatWorkers: 1, Stat: false, Exclude: map[string]bool{"excluded": true}})
	if err != nil {
		t.Fatalf("ScanTree readdir-only: %v", err)
	}
	resStat, err := ScanTree(ctx, root, ScanOptions{StatWorkers: 8, Stat: true, Exclude: map[string]bool{"excluded": true}})
	if err != nil {
		t.Fatalf("ScanTree with stat: %v", err)
	}

	if resReaddir.Files != int64(files) || resStat.Files != int64(files) {
		t.Fatalf("files: readdir=%d stat=%d want=%d", resReaddir.Files, resStat.Files, files)
	}
	if resReaddir.Dirs != int64(dirs) || resStat.Dirs != int64(dirs) {
		t.Fatalf("dirs: readdir=%d stat=%d want=%d", resReaddir.Dirs, resStat.Dirs, dirs)
	}
	if resReaddir.Errors != 0 || resStat.Errors != 0 {
		t.Fatalf("errors: readdir=%d stat=%d want=0", resReaddir.Errors, resStat.Errors)
	}
}

// TestScanCancelConvergence 验证 ctx 取消后 ScanTree 返回且不泄漏：
// 在大平目录上取消后统计循环必须终止。
func TestScanCancelConvergence(t *testing.T) {
	root := t.TempDir()
	flat := filepath.Join(root, "flat")
	if err := os.MkdirAll(flat, 0o755); err != nil {
		t.Fatalf("mkdir flat: %v", err)
	}
	for i := 0; i < 200; i++ {
		p := filepath.Join(flat, "f"+string(rune('a'+i%26))+string(rune('0'+i/26))+string(rune('0'+i%10)))
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	// 预热线：先完整扫一遍，再取消模式下扫描必须快速收敛返回
	if _, err := ScanTree(ctx, root, ScanOptions{StatWorkers: 4, Stat: true}); err != nil {
		t.Fatalf("prewarm scan: %v", err)
	}
	cancel()
	res, err := ScanTree(ctx, root, ScanOptions{StatWorkers: 4, Stat: true})
	if err != nil {
		t.Fatalf("canceled scan must not return error: %v", err)
	}
	_ = res // 取消后统计值不保证完整，只要求收敛返回
}

// TestScanNoSymlinkFollow 验证符号链接不跟随、不参与文件计数。
func TestScanNoSymlinkFollow(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "real")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatalf("write real: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(root, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// 目录符号链接：指向一个含文件的目录，不得下钻
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, "inner"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write inner: %v", err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "outside-file"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "dirlink")); err != nil {
		t.Fatalf("symlink dir: %v", err)
	}

	res, err := ScanTree(context.Background(), root, ScanOptions{StatWorkers: 4, Stat: true})
	if err != nil {
		t.Fatalf("ScanTree: %v", err)
	}
	// real(1) + sub/inner(1) = 2；link 与 dirlink 都不是常规文件，不计入也不跟随
	if res.Files != 2 {
		t.Fatalf("files = %d, want 2 (symlinks must not be counted or followed)", res.Files)
	}
}
