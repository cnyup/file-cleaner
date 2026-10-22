// Package engine 组合扫描内核、策略判定与移动执行为一条流水线。
//
// 分工：
//   - nfs.Walker 提供元数据（mtime/ctime/atime/dev/ino + mode）；
//   - policy.Evaluator 判定命中；
//   - OnMove 回调执行实际 rename（Wave 2 接审计与回收站）；
//   - scan 与 drain 独立计时：ScanElapsed 在扫描+判定完成时取样，
//     DrainElapsed 覆盖 OnMove 消费排空阶段。
package engine

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bcyup/filecleaner/internal/nfs"
	"github.com/bcyup/filecleaner/internal/policy"
)

// PipelineOptions 并发配置。
type PipelineOptions struct {
	DirWorkers  int
	StatWorkers int
	MoveWorkers int
}

// RunStats 分阶段计时与计数。
type RunStats struct {
	Files         int64
	Dirs          int64
	Errors        int64
	Matched       int64
	MovedOK       int64
	MoveErrors    int64
	ScanElapsed   time.Duration
	DrainElapsed  time.Duration
	TotalElapsed  time.Duration
	// EmptyDirs* 由 runner 尾扫填充（empty_dirs.enabled 时）：
	// Removed/Skipped/Errors 为物理口径（dry-run 时 Removed=候选计数）。
	EmptyDirsCandidates int64
	EmptyDirsRemoved    int64
	EmptyDirsSkipped    int64
	EmptyDirsErrors     int64
}

// moveTask 命中条目（相对路径 + 元数据快照，full 含 inode 供核验）。
type moveTask struct {
	rel  string
	meta policy.FileMeta
	full nfs.FileMetaFull
}

// Pipeline 是一次 run 的流水线实例（不可复用）。
type Pipeline struct {
	opts PipelineOptions
}

// NewPipeline 构造流水线。
func NewPipeline(opts PipelineOptions) *Pipeline {
	if opts.DirWorkers <= 0 {
		opts.DirWorkers = 8
	}
	if opts.StatWorkers <= 0 {
		opts.StatWorkers = 1
	}
	if opts.MoveWorkers <= 0 {
		opts.MoveWorkers = 4
	}
	return &Pipeline{opts: opts}
}

// RunVirtual 对 N 个虚拟元数据跑 判定→move 流水线（热路径行为等价，
// 不触碰文件系统），用于有界性/背压/泄漏测试与容量预估。
func (p *Pipeline) RunVirtual(ctx context.Context, n int,
	match func(policy.FileMeta) bool, move func(string) error) (*RunStats, error) {

	total := time.Now()
	stats := &RunStats{}
	moveCh := make(chan moveTask, 4096)

	var moveWG sync.WaitGroup
	for i := 0; i < p.opts.MoveWorkers; i++ {
		moveWG.Add(1)
		go func() {
			defer moveWG.Done()
			for mt := range moveCh {
				if ctx.Err() != nil {
					continue
				}
				if err := move(mt.rel); err != nil {
					atomic.AddInt64(&stats.MoveErrors, 1)
				} else {
					atomic.AddInt64(&stats.MovedOK, 1)
				}
			}
		}()
	}

	// 虚拟生产者：计数即"扫描"
	scanStart := time.Now()
	for i := 0; i < n; i++ {
		if ctx.Err() != nil {
			break
		}
		stats.Files++
		meta := policy.FileMeta{MTime: time.Now(), CTime: time.Now(), ATime: time.Now()}
		if match(meta) {
			stats.Matched++
			select {
			case moveCh <- moveTask{rel: itoa(i), meta: meta}:
			default:
				// 有界背压：通道满时在调用方内联消费（由 move worker 池
				// 之外的 this goroutine 代跑，防止无界内存）。
				if err := move(itoa(i)); err != nil {
					atomic.AddInt64(&stats.MoveErrors, 1)
				} else {
					atomic.AddInt64(&stats.MovedOK, 1)
				}
			}
		}
	}
	stats.ScanElapsed = time.Since(scanStart)
	close(moveCh)
	drainStart := time.Now()
	moveWG.Wait()
	stats.DrainElapsed = time.Since(drainStart)
	stats.TotalElapsed = time.Since(total)
	return stats, ctx.Err()
}

// TreeOptions 真实树扫描选项。
type TreeOptions struct {
	ExcludeDirs []string
	ExcludePats []string
	Conditions  policy.Conditions
	MatchOp     string // "" | "AND" | "OR"
	QuietPeriod time.Duration
	// OnMove 命中回调；meta 含扫描快照（mtime/ctime/atime），full 含
	// inode/dev/size——调用方应把快照带给 mover 以省核验 RPC。
	OnMove func(rel string, meta policy.FileMeta, full nfs.FileMetaFull) error
}

// RunTree 扫描 root + 判定 + OnMove；返回分阶段统计。
// Wave 1 版本：OnMove 由调用方注入（测试用）；Wave 2 接审计 mover。
func (p *Pipeline) RunTree(ctx context.Context, root string, opts TreeOptions) (*RunStats, error) {
	total := time.Now()
	stats := &RunStats{}

	ev, err := policy.NewEvaluator(opts.Conditions, opts.MatchOp, time.Now)
	if err != nil {
		return nil, err
	}
	ev.SetQuietPeriod(opts.QuietPeriod)

	filter, err := policy.NewFilter(opts.ExcludePats, opts.ExcludeDirs)
	if err != nil {
		return nil, err
	}

	moveCh := make(chan moveTask, 4096)
	var moveWG sync.WaitGroup
	for i := 0; i < p.opts.MoveWorkers; i++ {
		moveWG.Add(1)
		go func() {
			defer moveWG.Done()
			for mt := range moveCh {
				if ctx.Err() != nil {
					continue
				}
				if opts.OnMove == nil {
					continue
				}
				if err := opts.OnMove(mt.rel, mt.meta, mt.full); err != nil {
					atomic.AddInt64(&stats.MoveErrors, 1)
				} else {
					atomic.AddInt64(&stats.MovedOK, 1)
				}
			}
		}()
	}

	scanStart := time.Now()
	err = nfs.WalkMeta(ctx, root, nfs.WalkOptions{
		DirWorkers:  p.opts.DirWorkers,
		StatWorkers: p.opts.StatWorkers,
		ExcludeDirs: append(opts.ExcludeDirs, nfs.DefaultTrashDir),
	}, func(rel string, meta nfs.FileMetaFull) {
		// visit 在 stat 池多 goroutine 并发执行：全部计数必须原子。
		atomic.AddInt64(&stats.Files, 1)
		if filter.MatchName(baseName(rel)) {
			return
		}
		pm := policy.FileMeta{
			MTime: time.Unix(0, meta.MTime),
			CTime: time.Unix(0, meta.CTime),
			ATime: time.Unix(0, meta.ATime),
		}
		if !ev.Match(pm) {
			return
		}
		atomic.AddInt64(&stats.Matched, 1)
		mt := moveTask{rel: rel, meta: pm, full: meta}
		select {
		case moveCh <- mt:
		default:
			// 背压：内联执行 move，保住有界内存
			if opts.OnMove != nil {
				if err := opts.OnMove(mt.rel, mt.meta, mt.full); err != nil {
					atomic.AddInt64(&stats.MoveErrors, 1)
				} else {
					atomic.AddInt64(&stats.MovedOK, 1)
				}
			}
		}
	})
	stats.ScanElapsed = time.Since(scanStart)
	stats.Dirs, stats.Errors = nfs.LastWalkCounters()

	close(moveCh)
	drainStart := time.Now()
	moveWG.Wait()
	stats.DrainElapsed = time.Since(drainStart)
	stats.TotalElapsed = time.Since(total)
	return stats, err
}

func baseName(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func runtimeGoroutines() int { return runtime.NumGoroutine() }

func ctx2() context.Context { return context.Background() }
