package runner

// RunEmptyDirs 独立空目录清理的行为测试（2026-11-16）。
//
// 契约：
//  1. dry-run 只统计不删除（文件系统零改动）
//  2. 真实运行时删除超龄空目录、保留未超龄的
//  3. 非空目录永不删除（三重安全保险之一）
//  4. empty_dirs 未启用时明确报错，不静默返回

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bcyup/filecleaner/internal/config"
)

// emptyDirsTestCfg 构造一个带 empty_dirs 的最小配置。
func emptyDirsTestCfg(t *testing.T, root, stateDir string, retention string) (*config.Config, *config.Policy) {
	t.Helper()
	cfg := &config.Config{
		StateDir: stateDir,
		Policies: []config.Policy{
			{
				ID:   "p-ed",
				Root: root,
				Trash: config.Trash{
					Dir:       ".filecleaner-trash",
					Retention: "10d",
				},
				EmptyDirs: config.EmptyDirs{
					Enabled:   true,
					Retention: retention,
				},
				Concurrency: config.Concurrency{DirReadWorkers: 4},
			},
		},
	}
	return cfg, &cfg.Policies[0]
}

// TestRunEmptyDirsDryRunNoDelete dry-run 必须零改动。
func TestRunEmptyDirsDryRunNoDelete(t *testing.T) {
	root := t.TempDir()
	cfg, pol := emptyDirsTestCfg(t, root, filepath.Join(root, "state"), "7d")

	victim := filepath.Join(root, "old_empty")
	if err := os.Mkdir(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	backdateDir(t, victim, 10*24*time.Hour)

	res, err := RunEmptyDirs(cfg, pol, true)
	if err != nil {
		t.Fatalf("RunEmptyDirs: %v", err)
	}
	if res.Candidates != 1 {
		t.Fatalf("candidates=%d want 1", res.Candidates)
	}
	if res.Removed != 1 {
		t.Fatalf("dry-run removed=%d want 1 (dry-run 计入 removed 口径)", res.Removed)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("dry-run 删除了目录：%v", err)
	}
}

// TestRunEmptyDirsDeletesOldKeepsYoung 真实运行：超龄删、未超龄留。
func TestRunEmptyDirsDeletesOldKeepsYoung(t *testing.T) {
	root := t.TempDir()
	cfg, pol := emptyDirsTestCfg(t, root, filepath.Join(root, "state"), "7d")

	old := filepath.Join(root, "old_empty")
	young := filepath.Join(root, "young_empty")
	for _, d := range []string{old, young} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	backdateDir(t, old, 10*24*time.Hour) // 超龄
	// young 保持当前 mtime

	res, err := RunEmptyDirs(cfg, pol, false)
	if err != nil {
		t.Fatalf("RunEmptyDirs: %v", err)
	}
	if res.Removed != 1 {
		t.Fatalf("removed=%d want 1", res.Removed)
	}
	if res.Young != 1 {
		t.Fatalf("skipped_young=%d want 1", res.Young)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("超龄空目录未被删除")
	}
	if _, err := os.Stat(young); err != nil {
		t.Fatal("未超龄目录被误删")
	}
}

// TestRunEmptyDirsKeepsNonEmpty 非空目录永不删除。
func TestRunEmptyDirsKeepsNonEmpty(t *testing.T) {
	root := t.TempDir()
	cfg, pol := emptyDirsTestCfg(t, root, filepath.Join(root, "state"), "7d")

	keep := filepath.Join(root, "has_file")
	if err := os.Mkdir(keep, 0o755); err != nil {
		t.Fatal(err)
	}
	backdateDir(t, keep, 10*24*time.Hour)
	f := filepath.Join(keep, "data.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := RunEmptyDirs(cfg, pol, false)
	if err != nil {
		t.Fatalf("RunEmptyDirs: %v", err)
	}
	if res.Removed != 0 {
		t.Fatalf("removed=%d want 0 (非空目录必须保留)", res.Removed)
	}
	if _, err := os.Stat(f); err != nil {
		t.Fatalf("文件被删除：%v", err)
	}
}

// TestRunEmptyDirsNotEnabledFails empty_dirs 未启用时必须报错而非静默返回。
func TestRunEmptyDirsNotEnabledFails(t *testing.T) {
	root := t.TempDir()
	cfg, pol := emptyDirsTestCfg(t, root, filepath.Join(root, "state"), "7d")
	pol.EmptyDirs.Enabled = false

	if _, err := RunEmptyDirs(cfg, pol, true); err == nil {
		t.Fatal("empty_dirs 未启用时应返回错误")
	}
}

// 注：backdateDir 已在 emptydirs_runner_test.go 定义，同包复用。

// TestWindowStopAtCases 窗口→绝对停止时刻换算（2026-11-18 streaming 落地）：
// 窗口内返回本窗结束；跨天窗口前半夜返回次日结束、后半夜返回当天结束；
// 窗外返回错误（文本含窗口与当前时刻）。
func TestWindowStopAtCases(t *testing.T) {
	mk := func(h, m int) time.Time { return time.Date(2026, 11, 18, h, m, 0, 0, time.Local) }
	cases := []struct {
		name   string
		window string
		at     time.Time
		want   time.Time
		errSub string
	}{
		{"in-window mid", "01:00-07:00", mk(3, 0), mk(7, 0), ""},
		{"in-window at start", "01:00-07:00", mk(1, 0), mk(7, 0), ""},
		{"outside window", "01:00-07:00", mk(12, 0), time.Time{}, "outside window"},
		{"cross-day late night", "23:00-06:00", mk(23, 30), mk(6, 0).AddDate(0, 0, 1), ""},
		{"cross-day early morning", "23:00-06:00", mk(2, 0), mk(6, 0), ""},
		{"bad window", "9:00", mk(2, 0), time.Time{}, "want HH:MM"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := windowStopAt(tc.window, tc.at)
			if tc.errSub != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errSub) {
					t.Fatalf("want err containing %q, got %v", tc.errSub, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("windowStopAt(%q,%s): %v", tc.window, tc.at, err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("windowStopAt(%q, %s) = %s, want %s",
					tc.window, tc.at.Format("15:04"), got, tc.want)
			}
		})
	}
}
