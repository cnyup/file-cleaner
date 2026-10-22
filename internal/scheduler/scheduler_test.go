//go:build linux || darwin

package scheduler

import (
	"path/filepath"
	"testing"
	"time"
)

func TestLockSkipSemantics(t *testing.T) {
	dir := t.TempDir()

	l1, err := Acquire(dir, "p1")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	// 同策略第二次获取 → 必须失败（跳过语义）
	l2, err := Acquire(dir, "p1")
	if err == nil {
		l2.Release()
		t.Fatal("second acquire must fail (skip)")
	}
	// 不同策略 → 可获取
	l3, err := Acquire(dir, "p2")
	if err != nil {
		t.Fatalf("other policy acquire: %v", err)
	}
	l3.Release()

	// 释放后可重新获取
	l1.Release()
	l4, err := Acquire(dir, "p1")
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	l4.Release()
}

func TestStaleLockFileRecovers(t *testing.T) {
	dir := t.TempDir()
	// 残留锁文件（上次崩溃未清理）但无进程持锁 → flock 应可获取
	path := filepath.Join(dir, "locks", "p1.lock")
	if err := mkdirAll(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(path, []byte("99999")); err != nil {
		t.Fatal(err)
	}
	l, err := Acquire(dir, "p1")
	if err != nil {
		t.Fatalf("stale lock file must not block: %v", err)
	}
	l.Release()
}

func TestCronNext(t *testing.T) {
	s, err := ParseSchedule("30 2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 6, 14, 0, 0, 0, 0, time.Local)
	next := s.Next(base)
	want := time.Date(2026, 6, 14, 2, 30, 0, 0, time.Local)
	if !next.Equal(want) {
		t.Fatalf("next=%v want=%v", next, want)
	}
}
