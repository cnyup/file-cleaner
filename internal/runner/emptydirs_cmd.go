package runner

// 独立空目录清理入口（2026-11-16）：把 empty_dirs 从 RunOnce 里解耦，
// 允许只跑空目录清理、跳过全树 scan。
//
// 背景：basic-serice-video-daily 策略的全树 scan 需遍历 94 万顶层目录 +
// 千万级子目录，实测 2 天 18 小时未完成（2026-11-16）。而该 NAS 上几乎
// 没有文件（抽样 1,000 目录文件数 0），真正的清理目标是空目录本身。
// empty_dirs 在 RunOnce 里位于 scan 之后，scan 跑不完就永远执行不到。
//
// 本函数只做空目录清理，不做文件清理，因此：
//   - 不读 retention.conditions（文件期限与空目录无关）
//   - 不移动任何文件（无 trash 桶、无 move 审计）
//   - 复用 nfs.SweepEmptyDirs（含三重安全保险：readdir 复查、mtime 年龄门、
//     rmdir 的 ENOTEMPTY 内核兜底）

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/bcyup/filecleaner/internal/audit"
	"github.com/bcyup/filecleaner/internal/config"
	"github.com/bcyup/filecleaner/internal/nfs"
)

// EmptyDirsResult 独立空目录清理的结果。
type EmptyDirsResult struct {
	PolicyID   string        `json:"policy_id"`
	Root       string        `json:"root"`
	DryRun     bool          `json:"dry_run"`
	AgeGate    time.Duration `json:"age_gate"`
	Candidates int64         `json:"candidates"`
	Removed    int64         `json:"removed"`
	Skipped    int64         `json:"skipped"`
	NonEmpty   int64         `json:"skipped_nonempty"`
	Young      int64         `json:"skipped_young"`
	Gone       int64         `json:"skipped_gone"`
	Errors     int64         `json:"errors"`
	ElapsedMS  int64         `json:"elapsed_ms"`
	CollectMS  int64         `json:"collect_ms"`
	DeleteMS   int64         `json:"delete_ms"`
	// streaming 模式字段（mode=streaming 时填充）。
	Mode         string `json:"mode,omitempty"`
	EntriesScanned int64 `json:"entries_scanned,omitempty"`
	Resumed      int64  `json:"resumed,omitempty"`
	Stopped      bool   `json:"stopped,omitempty"` // 到窗口尾/上限优雅停（未完，可续）
}

// RunEmptyDirs 对单个策略执行一轮空目录清理（不含文件清理）。
// dryRun=true 时只统计不删除。审计：复用策略的 state_dir，落 rmdir 事务；
// dryRun 时不写审计（无实际删除）。
// opts 可选：WithIgnoreWindow(true) 贯穿 streaming 引擎的窗口熔断
// （--ignore-window 调试逃生口；生产 cron 永远不带）。
func RunEmptyDirs(cfg *config.Config, p *config.Policy, dryRun bool, opts ...EmptyDirsOption) (*EmptyDirsResult, error) {
	oc := emptyDirsOptChain{}
	if !p.EmptyDirs.Enabled {
		return nil, fmt.Errorf("policy %q: empty_dirs not enabled in config", p.ID)
	}
	gate, err := config.ParseDuration(p.EmptyDirs.Retention)
	if err != nil {
		return nil, fmt.Errorf("policy %q: empty_dirs.retention: %w", p.ID, err)
	}

	// 排除集：回收站 + 配置的排除目录（与 RunOnce 一致）
	exclude := map[string]bool{nfs.DefaultTrashDir: true}
	for _, d := range p.Exclusions.Dirs {
		exclude[d] = true
	}

	var jl *audit.JSONL
	runID := fmt.Sprintf("%s-emptydirs-%d", p.ID, time.Now().UnixNano())
	if !dryRun {
		if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
			return nil, err
		}
		jl, err = openAudit(cfg)
		if err != nil {
			return nil, err
		}
		defer jl.Close()
		jl.RunStarted(runID, p.ID, p.Hash())
	}

	var rmdirBatch []audit.RmdirIntent
	start := time.Now()
	for _, o := range opts {
		o(&oc)
	}

	var res *EmptyDirsResult
	if p.EmptyDirs.Mode == "streaming" {
		res, err = runEmptyDirsStreaming(cfg, p, gate, exclude, dryRun, &rmdirBatch, oc.ignoreWindow)
	} else {
		var estats *nfs.EmptyDirsStats
		estats, err = nfs.SweepEmptyDirs(p.Root, nfs.EmptyDirsOptions{
			AgeGate:    gate,
			Exclude:    exclude,
			DryRun:     dryRun,
			DirWorkers: p.Concurrency.DirReadWorkers,
			OnRmdir: func(path string) error {
				// 审计缓冲：与 RunOnce 同策略——批事务在尾批统一闭环。
				rmdirBatch = append(rmdirBatch, audit.RmdirIntent{
					RunID: runID, PolicyID: p.ID, Path: path,
				})
				return nil
			},
		})
		if err == nil {
			res = &EmptyDirsResult{
				PolicyID:   p.ID,
				Root:       p.Root,
				DryRun:     dryRun,
				AgeGate:    gate,
				Candidates: estats.Candidates,
				Removed:    estats.Removed,
				Skipped:    estats.Skipped,
				NonEmpty:   estats.SkippedNonEmpty,
				Young:      estats.SkippedYoung,
				Gone:       estats.SkippedGone,
				Errors:     estats.Errors,
				ElapsedMS:  time.Since(start).Milliseconds(),
				CollectMS:  estats.CollectElapsed.Milliseconds(),
				DeleteMS:   estats.DeleteElapsed.Milliseconds(),
			}
		}
	}
	elapsed := time.Since(start)
	_ = elapsed // legacy 分支已内联计时；保留变量供后续指标统一
	if err != nil {
		if jl != nil {
			jl.RunFinished(runID, p.ID, map[string]int64{"empty_dirs_error": 1})
		}
		return nil, fmt.Errorf("policy %q: empty_dirs sweep: %w", p.ID, err)
	}

	// 审计尾批（仅真实删除时）：intent 批落盘 → 复查物理结果 → result 落盘。
	// 与 RunOnce 同语义：sweep 已物理删除，这里补记事务闭环（先删后记，
	// 靠 rmdir 的 ENOTEMPTY 兜底保证崩溃一致性）。
	if jl != nil && len(rmdirBatch) > 0 {
		perDir := make([]error, len(rmdirBatch))
		for i, it := range rmdirBatch {
			if _, serr := os.Lstat(it.Path); serr == nil {
				perDir[i] = fmt.Errorf("still exists after sweep")
			}
		}
		if rerr := jl.CommitRmdirs(rmdirBatch, func() []error { return perDir }); rerr != nil {
			res.Errors++
		}
	}
	if jl != nil {
		jl.RunFinished(runID, p.ID, map[string]int64{
			"empty_dirs_candidates":       res.Candidates,
			"empty_dirs_removed":          res.Removed,
			"empty_dirs_skipped":          res.Skipped,
			"empty_dirs_skipped_nonempty": res.NonEmpty,
			"empty_dirs_skipped_young":    res.Young,
			"empty_dirs_skipped_gone":     res.Gone,
			"empty_dirs_errors":           res.Errors,
			"empty_dirs_ms":               res.ElapsedMS,
			"empty_dirs_collect_ms":       res.CollectMS,
			"empty_dirs_delete_ms":        res.DeleteMS,
		})
	}
	return res, nil
}

// emptyDirsOptChain 选项链聚合结果。
type emptyDirsOptChain struct {
	ignoreWindow bool
}

// EmptyDirsOption RunEmptyDirs 的可选参数。
type EmptyDirsOption func(*emptyDirsOptChain)

// WithIgnoreWindow 贯穿窗口熔断到 streaming 引擎（--ignore-window）。
func WithIgnoreWindow(v bool) EmptyDirsOption {
	return func(c *emptyDirsOptChain) { c.ignoreWindow = v }
}

// runEmptyDirsStreaming streaming 模式的一轮（一个窗口）清理。
// 窗口已过时返回错误（CLI 层的熔断语义在命令入口，这里是引擎侧兜底）；
// 窗口内启动、到点优雅停（Stopped=true，checkpoint 已落，下窗口续）。
func runEmptyDirsStreaming(cfg *config.Config, p *config.Policy, gate time.Duration, exclude map[string]bool, dryRun bool, rmdirBatch *[]audit.RmdirIntent, ignoreWindow bool) (*EmptyDirsResult, error) {
	stopAt, err := windowStopAt(p.EmptyDirs.Window, time.Now())
	if err != nil {
		if !ignoreWindow {
			return nil, err
		}
		// ignoreWindow：窗外照跑，StopAt 置零（不限时，调试语义）。
		stopAt = time.Time{}
	}
	runID := fmt.Sprintf("%s-emptydirs-streaming-%d", p.ID, time.Now().UnixNano())
	_ = runID // runID 由调用方生成；这里仅内部引用占位
	estats, err := nfs.SweepEmptyDirsStreaming(p.Root, nfs.EmptyDirsStreamingOptions{
		AgeGate:       gate,
		Exclude:       exclude,
		DryRun:        dryRun,
		CheckpointDir: p.EmptyDirs.CheckpointDir,
		StopAt:        stopAt,
		Workers:       p.Concurrency.DirReadWorkers,
		OnRmdir: func(path string) error {
			*rmdirBatch = append(*rmdirBatch, audit.RmdirIntent{
				RunID: runID, PolicyID: p.ID, Path: path,
			})
			return nil
		},
		Now: time.Now,
	})
	if err != nil {
		return nil, err
	}
	return &EmptyDirsResult{
		PolicyID:   p.ID,
		Root:       p.Root,
		DryRun:     dryRun,
		AgeGate:    gate,
		Mode:       "streaming",
		Candidates: estats.EntriesScanned,
		Removed:    estats.Removed,
		Skipped:    estats.SkippedYoung + estats.SkippedNonEmpty + estats.SkippedGone + estats.SkippedExclude,
		NonEmpty:   estats.SkippedNonEmpty,
		Young:      estats.SkippedYoung,
		Gone:       estats.SkippedGone,
		Errors:     estats.Errors,
		ElapsedMS:  estats.Elapsed.Milliseconds(),
		EntriesScanned: estats.EntriesScanned,
		Resumed:    estats.Resumed,
		Stopped:    estats.Stopped,
	}, nil
}

// windowStopAt 把窗口配置换算成本轮的绝对停止时刻：
//   - 当前在窗口内：本窗口的结束时刻（今天或明天，跨天窗口自动处理）；
//   - 当前在窗口外：返回错误（调用方决定拒绝——白天对巨目录 readdir
//     是实测过的生产风险，不是理论风险）。
func windowStopAt(w string, now time.Time) (time.Time, error) {
	s, e, err := config.ParseWindow(w)
	if err != nil {
		return time.Time{}, fmt.Errorf("empty_dirs.window: %w", err)
	}
	in, err := config.InWindow(w, now)
	if err != nil {
		return time.Time{}, err
	}
	if !in {
		return time.Time{}, fmt.Errorf("current time %s is outside window %q (daytime readdir on mega-dirs stalls the NAS; measured 60x slower than night) — refusing to start",
			now.Format("15:04"), w)
	}
	// 今日窗口结束时刻；跨天窗口（s > e）且当前已过 s → 结束在明天。
	endToday := time.Date(now.Year(), now.Month(), now.Day(), e/60, e%60, 0, 0, now.Location())
	if s > e && now.Hour()*60+now.Minute() >= s {
		endToday = endToday.AddDate(0, 0, 1)
	}
	return endToday, nil
}

// emptyDirsTrashPath 辅助：返回策略回收站的绝对路径（供调用方打印提示）。
func emptyDirsTrashPath(p *config.Policy) string {
	dir := p.Trash.Dir
	if dir == "" {
		dir = nfs.DefaultTrashDir
	}
	return filepath.Join(p.Root, dir)
}
