package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bcyup/filecleaner/internal/audit"
	"github.com/bcyup/filecleaner/internal/config"
	"github.com/bcyup/filecleaner/internal/runner"
	"github.com/bcyup/filecleaner/internal/scheduler"
)

// jobKind 区分主清理与回收站 purge 两类调度任务。
type jobKind int

const (
	jobRun jobKind = iota
	jobPurge
)

// job 是一条已解析的调度任务（run 或 purge）。
type job struct {
	pol  *config.Policy
	sch  *scheduler.Schedule
	kind jobKind
	next time.Time
}

// buildJobs 从配置展开调度任务：每个带 schedule 的策略一个 run job；
// 每个带 trash.purge_schedule 的策略额外一个 purge job。二者共用同一把
// 策略锁（RunOnce/Purge 内部 Acquire 同一 lock 文件），天然互斥。
func buildJobs(cfg *config.Config) ([]*job, error) {
	var jobs []*job
	for i := range cfg.Policies {
		p := &cfg.Policies[i]
		if p.Schedule != "" {
			sch, err := scheduler.ParseSchedule(p.Schedule)
			if err != nil {
				return nil, fmt.Errorf("daemon: policy %s schedule: %w", p.ID, err)
			}
			jobs = append(jobs, &job{pol: p, sch: sch, kind: jobRun, next: sch.Next(time.Now())})
		}
		if p.Trash.PurgeSchedule != "" {
			sch, err := scheduler.ParseSchedule(p.Trash.PurgeSchedule)
			if err != nil {
				return nil, fmt.Errorf("daemon: policy %s purge_schedule: %w", p.ID, err)
			}
			jobs = append(jobs, &job{pol: p, sch: sch, kind: jobPurge, next: sch.Next(time.Now())})
		}
	}
	return jobs, nil
}

// daemonLoop 调度主循环：最近触发的 job 先跑；ctx 取消即返回。
// 输出写 w（测试可注入 buffer；生产传 os.Stdout）。
func daemonLoop(ctx context.Context, cfg *config.Config, w io.Writer) {
	jobs, err := buildJobs(cfg)
	if err != nil {
		fmt.Fprintf(w, "daemon: %v\n", err)
		return
	}
	if len(jobs) == 0 {
		fmt.Fprintln(w, "daemon: no scheduled jobs")
		return
	}
	fmt.Fprintf(w, "daemon: %d scheduled jobs\n", len(jobs))

	// on-start 轮：run_on_start=true 时启动即清理（消除部署到首个
	// cron 触发间的空窗）。走 daemonLoop 内同一执行路径与策略锁；
	// 单个失败不阻塞其余 job——错误处理与定时轮一致（打印继续）。
	if cfg.RunOnStart {
		for _, j := range jobs {
			if ctx.Err() != nil {
				break
			}
			if j.kind != jobRun {
				continue // purge 有独立 purge_schedule，on-start 只补清理欠账
			}
			fmt.Fprintf(w, "daemon: on-start run policy %s\n", j.pol.ID)
			if _, err := runner.RunOnce(ctx, cfg, j.pol, false); err != nil {
				fmt.Fprintf(w, "daemon: on-start %s: %v\n", j.pol.ID, err)
			}
		}
	}

	for {
		// 最近触发的 job 先跑；next 为零值的 job（永不存在的日期，
		// 如 2月31日）排除出竞争，否则 Until(零值)<0 会立即触发并死循环。
		var soonest *job
		for _, j := range jobs {
			if j.next.IsZero() {
				continue
			}
			if soonest == nil || j.next.Before(soonest.next) {
				soonest = j
			}
		}
		if soonest == nil {
			// 全部 job 永不触发：等待退出信号
			fmt.Fprintln(w, "daemon: no future triggers, idling")
			<-ctx.Done()
			fmt.Fprintln(w, "daemon: shutting down")
			return
		}
		wait := time.Until(soonest.next)
		if wait > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				fmt.Fprintln(w, "daemon: shutting down")
				return
			}
		}
		if ctx.Err() != nil {
			fmt.Fprintln(w, "daemon: shutting down")
			return
		}
		j := soonest
		switch j.kind {
		case jobRun:
			fmt.Fprintf(w, "daemon: run policy %s\n", j.pol.ID)
			if _, err := runner.RunOnce(ctx, cfg, j.pol, false); err != nil {
				fmt.Fprintf(w, "daemon: policy %s: %v\n", j.pol.ID, err)
			}
		case jobPurge:
			fmt.Fprintf(w, "daemon: purge policy %s\n", j.pol.ID)
			if _, err := runner.Purge(cfg, j.pol); err != nil {
				fmt.Fprintf(w, "daemon: purge %s: %v\n", j.pol.ID, err)
			}
		}
		j.next = j.sch.Next(time.Now())
	}
}

// auditPruneLoop 周期清理超过保留期的 audit 分片：启动即一轮，此后
// 每 24h 一轮。rotation: off 时 no-op。删除动作逐文件打日志（journalctl
// 可审计 prune 行为本身）。
func auditPruneLoop(ctx context.Context, cfg *config.Config, w io.Writer) {
	if cfg.AuditRotationMode() != config.AuditRotationDaily {
		return
	}
	keep, err := cfg.AuditRetentionDur()
	if err != nil || keep <= 0 {
		return // Validate 已拦截非法值；这里防御性退出
	}
	prune := func() {
		removed, err := audit.PruneOldFiles(cfg.StateDir, keep, time.Now())
		if err != nil {
			fmt.Fprintf(w, "audit prune: %v\n", err)
		}
		for _, name := range removed {
			fmt.Fprintf(w, "audit prune: removed %s (older than %s)\n", name, cfg.AuditLog.Retention)
		}
	}
	prune()
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			prune()
		}
	}
}

// runDaemon 实现 `filecleaner daemon --config <yaml>`。
func runDaemon(args []string) int {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfgPath := fs.String("config", "config.yaml", "config file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "daemon: %v\n", err)
		return 1
	}
	if errs := config.Validate(cfg); len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "daemon: invalid config: %v\n", e)
		}
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// audit 分片保留期清理：启动即一轮 + 每日一轮（轻量：state_dir 内
	// 分片数 ≤ retention 天数 + legacy 1 个）。仅 daily 模式生效。
	go auditPruneLoop(ctx, cfg, os.Stdout)

	// sweeper 巡检 goroutine：周期物理删除 .retired/.purging 桶（异步，
	// 不持策略锁——桶级安全由 rename 原子性保证）。
	go runner.SweepLoop(ctx, cfg, time.Hour, os.Stdout)

	daemonLoop(ctx, cfg, os.Stdout)
	return 0
}
