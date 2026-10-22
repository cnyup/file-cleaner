//go:build linux || darwin

// Package scheduler 实现本地 flock 策略锁与 cron 调度。
//
// 语义（设计文档 §5/§9）：
//   - 锁文件 state_dir/locks/<policy>.lock，flock LOCK_EX|LOCK_NB；
//   - 拿不到锁 = 上次未完成 → 本次触发直接跳过（不排队不并行）；
//   - cleanup 与 purge 共用同一把策略锁（互斥但独立调度）；
//   - 残留锁文件不阻塞：flock 语义下进程退出即释放，文件存在无妨。
package scheduler

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/robfig/cron/v3"
)

// Guard 是一把已获取的策略锁。
type Guard struct {
	f *os.File
}

// Acquire 获取策略锁；失败返回错误（调用方按"跳过"处理）。
func Acquire(stateDir, policyID string) (*Guard, error) {
	dir := filepath.Join(stateDir, "locks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("scheduler: mkdir locks: %w", err)
	}
	path := filepath.Join(dir, policyID+".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return nil, fmt.Errorf("scheduler: open lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("scheduler: policy %q busy (previous run in progress), skipping", policyID)
	}
	return &Guard{f: f}, nil
}

// Release 释放锁。
func (g *Guard) Release() {
	if g == nil || g.f == nil {
		return
	}
	_ = syscall.Flock(int(g.f.Fd()), syscall.LOCK_UN)
	g.f.Close()
	g.f = nil
}

// Schedule 封装 cron 表达式。
type Schedule struct {
	spec cron.Schedule
}

// ParseSchedule 解析标准 5 段 cron。
func ParseSchedule(spec string) (*Schedule, error) {
	s, err := cron.ParseStandard(spec)
	if err != nil {
		return nil, fmt.Errorf("scheduler: bad cron %q: %w", spec, err)
	}
	return &Schedule{spec: s}, nil
}

// Next 返回 t 之后的下一次触发时间。
func (s *Schedule) Next(t time.Time) time.Time { return s.spec.Next(t) }

// 测试辅助（非 Linux 构建下 syscall.Flock 不存在于 plan9 等，不支持即编译失败——
// 本包整体以 linux||darwin 为门槛）。
func mkdirAll(dir string) error { return os.MkdirAll(dir, 0o755) }

func writeFile(path string, b []byte) error { return os.WriteFile(path, b, 0o640) }
