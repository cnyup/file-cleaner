// Package runner 把 config/policy/nfs/engine/audit/trash 接成一个可执行 run。
package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bcyup/filecleaner/internal/audit"
	"github.com/bcyup/filecleaner/internal/config"
	"github.com/bcyup/filecleaner/internal/engine"
	"github.com/bcyup/filecleaner/internal/nfs"
	"github.com/bcyup/filecleaner/internal/policy"
	"github.com/bcyup/filecleaner/internal/scheduler"
	"github.com/bcyup/filecleaner/internal/trash"
)

// openAudit 按配置轮转模式打开审计 writer（daily → 当天分片，
// off → legacy 单文件）。config 层已 Validate 模式合法性。
func openAudit(cfg *config.Config) (*audit.JSONL, error) {
	mode := audit.RotationMode(cfg.AuditRotationMode())
	return audit.OpenJSONLRotating(cfg.StateDir, mode, time.Now())
}

// Runonce 对单个策略执行一次完整清理（含锁、dry-run、审计、purge 由独立命令触发）。
// 返回统计与错误。
func RunOnce(ctx context.Context, cfg *config.Config, p *config.Policy, dryRun bool) (*engine.RunStats, error) {
	// 锁：上次未完成 → 跳过
	guard, err := scheduler.Acquire(cfg.StateDir, p.ID)
	if err != nil {
		return nil, err
	}
	defer guard.Release()

	// 运行时校验：root 存在；trash 与 root 同 st_dev（fail-closed）
	rootDev, err := deviceOf(p.Root)
	if err != nil {
		return nil, fmt.Errorf("policy %q: root: %w", p.ID, err)
	}
	trashDir := p.Trash.Dir
	if trashDir == "" {
		trashDir = nfs.DefaultTrashDir
	}
	absTrash := filepath.Join(p.Root, trashDir)
	if err := os.MkdirAll(absTrash, 0o755); err != nil {
		return nil, fmt.Errorf("policy %q: mkdir trash: %w", p.ID, err)
	}
	trashDev, err := deviceOf(absTrash)
	if err != nil || trashDev != rootDev {
		return nil, fmt.Errorf("policy %q: trash %s not on same filesystem as root (st_dev %v vs %v)", p.ID, absTrash, trashDev, rootDev)
	}

	// 审计
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return nil, err
	}
	jl, err := openAudit(cfg)
	if err != nil {
		return nil, err
	}
	defer jl.Close()

	layout := trash.NewLayout(p.Root, trashDir)
	mover := trash.NewMover(layout, jl)
	mover.SetMoveConcurrency(p.Concurrency.MoveWorkers)

	// 条件（配置键 *_older_than → policy 键）
	conds := policy.Conditions{}
	condKey := map[string]string{
		"mtime_older_than": "mtime",
		"ctime_older_than": "ctime",
		"atime_older_than": "atime",
	}
	for k, v := range p.Retention.Conditions {
		pk, ok := condKey[k]
		if !ok {
			return nil, fmt.Errorf("policy %q: unknown condition %q", p.ID, k)
		}
		d, err := config.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("policy %q: %s: %w", p.ID, k, err)
		}
		conds[pk] = d
	}

	// 静默期
	var quiet time.Duration
	if p.Retention.MinQuietPeriod != "" {
		q, err := config.ParseDuration(p.Retention.MinQuietPeriod)
		if err != nil {
			return nil, fmt.Errorf("policy %q: min_quiet_period: %w", p.ID, err)
		}
		quiet = q
	}

	runID := fmt.Sprintf("%s-%d", p.ID, time.Now().UnixNano())
	jl.RunStarted(runID, p.ID, p.Hash())

	// 分桶：本轮 run 使用独立日期桶（retire/恢复按桶语义）
	bucket := layout.NewBucketName(time.Now())

	var skipAgg sync.Map // reason -> int64
	pipeline := engine.NewPipeline(engine.PipelineOptions{
		DirWorkers:  p.Concurrency.DirReadWorkers,
		StatWorkers: p.Concurrency.StatWorkers,
		MoveWorkers: p.Concurrency.MoveWorkers,
	})

	// move 批量缓冲：凑批走审计事务。
	// physMoved/physFailed 是物理口径计数（P0-2）：OnMove 入队成功 ≠
	// 物理移动成功，批事务的真实结果才是真相——尾批（P0-3）与凑批
	// 轮转批的每文件成败都计入这里，不再有被 _ = flush() 吞掉的批。
	var physMoved, physFailed atomic.Int64
	var mu sync.Mutex
	var pending []trash.MoveReq
	flush := func() error {
		mu.Lock()
		batch := pending
		pending = nil
		mu.Unlock()
		if len(batch) == 0 || dryRun {
			return nil
		}
		okN, err := mover.MoveBatchBucket(runID, p.ID, bucket, batch, trash.Recheck{})
		physMoved.Add(int64(okN))
		physFailed.Add(int64(len(batch) - okN))
		return err
	}

	stats, err := pipeline.RunTree(ctx, p.Root, engine.TreeOptions{
		ExcludeDirs: p.Exclusions.Dirs,
		ExcludePats: p.Exclusions.Patterns,
		Conditions:  conds,
		MatchOp:     p.Retention.Match,
		QuietPeriod: quiet,
		OnMove: func(rel string, meta policy.FileMeta, full nfs.FileMetaFull) error {
			mu.Lock()
			pending = append(pending, trash.MoveReq{
				Src:       filepath.Join(p.Root, filepath.FromSlash(rel)),
				Rel:       rel,
				ScanInode: full.Ino,
				ScanMtime: time.Unix(0, full.MTime),
			})
			shouldFlush := len(pending) >= 256
			mu.Unlock()
			if shouldFlush {
				return flush()
			}
			return nil
		},
	})
	// 尾批：物理执行后错误不再丢弃（P0-3）——审计 per-file result 已在
	// 批事务内落盘闭环，这里的聚合 err 仅用于返回值。
	tailErr := flush()
	if stats == nil {
		// RunTree 构造期失败（evaluator/filter）：补写 run_finished 后返回错误
		jl.RunFinished(runID, p.ID, map[string]int64{"construct_error": 1})
		return nil, fmt.Errorf("policy %q: pipeline construct: %w", p.ID, err)
	}

	// 物理口径覆盖（P0-2）：dry-run 时 flush 是 no-op（physMoved=0），
	// 保留 pipeline 的 matched 口径；真删时以批事务结果为准——
	// moved=物理成功数，move_errors=物理失败数。
	if !dryRun {
		stats.MovedOK = physMoved.Load()
		stats.MoveErrors = physFailed.Load()
	}

	// 空目录尾扫（empty_dirs.enabled 时）：文件移动完成后执行，本轮
	// 移走文件留下的空父目录本轮即可清理。同一策略锁/run_id/审计文件。
	if p.EmptyDirs.Enabled {
		gate, err := config.ParseDuration(p.EmptyDirs.Retention)
		if err != nil {
			jl.RunFinished(runID, p.ID, map[string]int64{"config_error": 1})
			return stats, fmt.Errorf("policy %q: empty_dirs.retention: %w", p.ID, err)
		}
		exclude := map[string]bool{nfs.DefaultTrashDir: true}
		for _, d := range p.Exclusions.Dirs {
			exclude[d] = true
		}
		var rmdirBatch []audit.RmdirIntent
		var rbMu sync.Mutex
		estats, err := nfs.SweepEmptyDirs(p.Root, nfs.EmptyDirsOptions{
			AgeGate:   gate,
			Exclude:   exclude,
			DryRun:    dryRun,
			DirWorkers: p.Concurrency.DirReadWorkers,
			OnRmdir: func(path string) error {
				// 审计批缓冲：凑批走 rmdir 事务；nil 返回放行删除。
				rbMu.Lock()
				rmdirBatch = append(rmdirBatch, audit.RmdirIntent{RunID: runID, PolicyID: p.ID, Path: path})
				flushNow := len(rmdirBatch) >= 256
				rbMu.Unlock()
				_ = flushNow // 事务由尾批统一闭环（见下）：SweepEmptyDirs 串行删除，
				// 单目录粒度的 intent-before-hook 与批量事务二选一，此处选批。
				return nil
			},
		})
		if err == nil && !dryRun && len(rmdirBatch) > 0 {
			// 尾批事务：intent 批落盘 → 逐目录 os.Remove 已由 sweep 完成 →
			// result 按物理结果落盘。sweep 已物理删除，这里补记事务闭环
			// （intent/result 都在删除后落盘——rmdir 的 ENOTEMPTY 物理兜底
			// 使"先删后记"不破坏崩溃一致性：崩溃窗口内最多多删已验证为
			// 空+超龄的目录，且重放安全）。
			perDir := make([]error, len(rmdirBatch))
			for i, it := range rmdirBatch {
				if _, serr := os.Lstat(it.Path); serr == nil {
					perDir[i] = fmt.Errorf("still exists after sweep")
				}
			}
			_ = jl.CommitRmdirs(rmdirBatch, func() []error { return perDir })
		}
		if estats != nil {
			stats.EmptyDirsCandidates = estats.Candidates
			stats.EmptyDirsRemoved = estats.Removed
			stats.EmptyDirsSkipped = estats.Skipped
			stats.EmptyDirsErrors = estats.Errors
		}
		if err != nil {
			jl.RunFinished(runID, p.ID, map[string]int64{"empty_dirs_error": 1})
			return stats, fmt.Errorf("policy %q: empty_dirs sweep: %w", p.ID, err)
		}
	}

	var agg map[string]int64
	agg = map[string]int64{}
	skipAgg.Range(func(k, v any) bool {
		if s, ok := k.(string); ok {
			if n, ok := v.(int64); ok {
				agg[s] = n
			}
		}
		return true
	})
	jl.AppendSkippedAgg(runID, p.ID, agg)
	totals := map[string]int64{
		"files": stats.Files, "matched": stats.Matched,
		"moved": stats.MovedOK, "move_errors": stats.MoveErrors, "errors": stats.Errors,
		"empty_dirs_candidates": stats.EmptyDirsCandidates, "empty_dirs_removed": stats.EmptyDirsRemoved,
		"empty_dirs_skipped": stats.EmptyDirsSkipped, "empty_dirs_errors": stats.EmptyDirsErrors,
	}
	jl.RunFinished(runID, p.ID, totals)
	if err == nil {
		err = tailErr
	}
	return stats, err
}

// Restore 恢复条目（桶语义）：从审计投影定位该文件当前所在的回收站路径
// （含桶前缀），再从该路径恢复。找不到活跃记录 → 报错。
func Restore(cfg *config.Config, p *config.Policy, rel string) error {
	jl, err := openAudit(cfg)
	if err != nil {
		return err
	}
	defer jl.Close()
	st, err := audit.OpenStore(filepath.Join(cfg.StateDir, "audit.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	if _, err := stImport(cfg); err != nil {
		return err
	}
	layout := trash.NewLayout(p.Root, p.Trash.Dir)
	rows, err := st.QueryMoved(p.ID)
	if err != nil {
		return err
	}
	want := layout.TrashPath(rel) // 旧扁平路径形态（匹配历史）
	bucketSuffix := "/" + filepath.ToSlash(rel)
	var found string
	for _, r := range rows {
		if r.Trash == want || strings.HasSuffix(filepath.ToSlash(r.Trash), bucketSuffix) {
			found = r.Trash
			break
		}
	}
	if found == "" {
		return fmt.Errorf("restore: no active trash record for %q (retired or already restored)", rel)
	}
	runID := fmt.Sprintf("%s-restore-%d", p.ID, time.Now().UnixNano())
	return m_RestorePath(jl, layout, p, runID, found, rel)
}

// m_RestorePath 从指定回收站绝对路径恢复到原路径。
func m_RestorePath(jl *audit.JSONL, layout *trash.Layout, p *config.Policy, runID, trashPath, rel string) error {
	srcPath := filepath.Join(p.Root, filepath.FromSlash(rel))
	if err := jl.CommitRestore(runID, p.ID, srcPath, trashPath); err != nil {
		return err
	}
	actErr := func() error {
		if _, err := os.Lstat(srcPath); err == nil {
			return fmt.Errorf("restore: target exists, refusing to overwrite %s", srcPath)
		}
		if err := os.MkdirAll(filepath.Dir(srcPath), 0o755); err != nil {
			return err
		}
		return os.Rename(trashPath, srcPath)
	}()
	ok := actErr == nil
	msg := ""
	if !ok {
		msg = actErr.Error()
	}
	if err := jl.WriteRestoreResult(runID, p.ID, trashPath, srcPath, ok, msg); err != nil && actErr == nil {
		return err
	}
	return actErr
}

// Purge 触发回收站清理。
func Purge(cfg *config.Config, p *config.Policy) ([]string, error) {
	guard, err := scheduler.Acquire(cfg.StateDir, p.ID)
	if err != nil {
		return nil, err
	}
	defer guard.Release()

	retention, err := config.ParseDuration(p.Trash.Retention)
	if err != nil {
		return nil, fmt.Errorf("policy %q: trash.retention: %w", p.ID, err)
	}
	jl, err := openAudit(cfg)
	if err != nil {
		return nil, err
	}
	defer jl.Close()

	layout := trash.NewLayout(p.Root, p.Trash.Dir)
	m := trash.NewMover(layout, jl)

	// 分桶语义：到期桶整桶 retire（O(1) rename，毫秒级），
	// 物理删除由 SweepOnce 后台完成。
	trashRoot := filepath.Join(p.Root, trashDirName(p))
	entries, err := os.ReadDir(trashRoot)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	cutoff := time.Now().Add(-retention)
	var retired []string
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || strings.HasPrefix(name, ".") {
			continue // 控制目录与文件跳过
		}
		// 桶名日期部分解析到期判断：date.run-id。
		// 到期条件：该桶"最后可能入站时间"（date 23:59:59）早于 cutoff——
		// 即桶内全部文件都已越过保留期。日期粒度保守判断，避免误 retire
		// 当天尚在保留期内的桶。
		dateStr, _, ok := strings.Cut(name, ".")
		bucketDate, err := time.ParseInLocation("2006-01-02", dateStr, time.Local)
		if !ok || err != nil {
			continue // 非法桶名跳过（fail-closed 不动）
		}
		bucketEnd := bucketDate.Add(24*time.Hour - time.Second) // date 23:59:59
		if bucketEnd.After(cutoff) {
			continue // 未到期
		}
		if err := m.Retire(p.ID, name); err != nil {
			return retired, err
		}
		retired = append(retired, name)
	}
	// sweep 不在此处同步执行：物理删除由 daemon 巡检（SweepLoop）或手动
	// `filecleaner sweep` 异步完成——purge 命令毫秒级返回。
	return retired, nil
}

// SweepLoop 周期巡检：物理删除全部策略的 .retired/.purging 桶。
// 由 daemon 以 goroutine 运行；每轮间隔 interval。
func SweepLoop(ctx context.Context, cfg *config.Config, interval time.Duration, log interface{ Write([]byte) (int, error) }) {
	for {
		if ctx.Err() != nil {
			return
		}
		for i := range cfg.Policies {
			p := &cfg.Policies[i]
			if n, err := Sweep(cfg, p); err != nil {
				fmt.Fprintf(log, "sweep: policy %s: %v\n", p.ID, err)
			} else if n > 0 {
				fmt.Fprintf(log, "sweep: policy %s: %d files physically deleted\n", p.ID, n)
			}
		}
		select {
		case <-time.After(interval):
		case <-ctx.Done():
			return
		}
	}
}

// Sweep 单策略一轮物理删除。
func Sweep(cfg *config.Config, p *config.Policy) (int, error) {
	jl, err := openAudit(cfg)
	if err != nil {
		return 0, err
	}
	defer jl.Close()
	layout := trash.NewLayout(p.Root, trashDirName(p))
	m := trash.NewMover(layout, jl)
	m.SetMoveConcurrency(p.Concurrency.MoveWorkers)
	return m.SweepOnce()
}

// trashDirName 返回策略回收站目录名。
func trashDirName(p *config.Policy) string {
	if p.Trash.Dir != "" {
		return p.Trash.Dir
	}
	return trash.DefaultTrashName
}

// AuditQuery 导出审计投影查询结果。
func AuditQuery(cfg *config.Config, p *config.Policy) ([]audit.MovedRow, error) {
	if _, err := stImport(cfg); err != nil {
		return nil, err
	}
	st, err := audit.OpenStore(filepath.Join(cfg.StateDir, "audit.db"))
	if err != nil {
		return nil, err
	}
	defer st.Close()
	return st.QueryMoved(p.ID)
}

// stImport 把全部审计分片（legacy + 日期分片）幂等导入投影库。
// 多文件聚合：跨天 restore 依赖历史分片全部在场。
func stImport(cfg *config.Config) (int64, error) {
	st, err := audit.OpenStore(filepath.Join(cfg.StateDir, "audit.db"))
	if err != nil {
		return 0, err
	}
	defer st.Close()
	files, err := audit.AuditFiles(cfg.StateDir)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, f := range files {
		n, err := st.ImportJSONL(f)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

func deviceOf(path string) (uint64, error) {
	var st osSt
	if err := stat(path, &st); err != nil {
		return 0, err
	}
	return st.dev, nil
}

// ---- 平台抽象（linux 直接 syscall；其他平台编译门槛在 e2e build tag）----

type osSt struct{ dev uint64 }

var stat = defaultStat

var _ = atomic.Int64{}

func defaultStat(path string, out *osSt) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if si, ok := fi.Sys().(interface{ Dev() uint64 }); ok {
		out.dev = si.Dev()
	}
	return nil
}
