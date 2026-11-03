package nfs

import (
	"context"
	"io"
	"os"
	"sync"
	"sync/atomic"
)

// DefaultTrashDir 是回收站目录名（策略 root 下），遍历恒排除。
const DefaultTrashDir = ".filecleaner-trash"

// FileMetaFull 是策略评估需要的完整元数据。
type FileMetaFull struct {
	Size  int64
	MTime int64 // unix nanos
	CTime int64
	ATime int64
	Dev   uint64
	Ino   uint64
	Mode  uint32
}

// WalkOptions 元数据遍历选项。
type WalkOptions struct {
	DirWorkers  int
	StatWorkers int
	ExcludeDirs []string          // basename 排除
	Exclude     map[string]bool   // 显式集合（与 ExcludeDirs 合并）
}

// WalkStats 一次 WalkMeta 的目录/错误计数与错误样本。per-walk 随返回值
// 交付——不落包级全局：daemon 并行调度（max_concurrent_runs 缺省 2，
// job 触发即起 goroutine）下，全局样本表会被并发 walk 互相清零/污染
//（2026-11-03 N1 部署后发现的竞态，walkmeta_concurrent_test.go 复现）。
type WalkStats struct {
	Dirs  int64
	Errs  int64
	// ErrSamples scan 期前 8 条错误样本（"path: err"）。供 runner 写入
	// run_finished——errors 计数有逐条归因，ENOENT race 与故障可区分
	//（2026-11-03 巡检观察①）。
	ErrSamples []string
}

// errSampler per-walk 错误采样器：封顶 8 条（防 audit 膨胀），
// mu 保护 dir/stat 池并发 append。
type errSampler struct {
	mu      sync.Mutex
	samples []string
}

// add 记录一条 scan 错误样本（路径 + 错误），封顶 8 条。
func (s *errSampler) add(path string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.samples) >= 8 {
		return
	}
	s.samples = append(s.samples, path+": "+err.Error())
}

// snapshot 返回样本副本。在 WalkMeta 返回前调用——walk 已 join（dirWG/
// statWG.Wait），锁拷贝是防御性冗余，保证返回值与内部状态彻底解耦。
func (s *errSampler) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.samples) == 0 {
		return nil
	}
	out := make([]string, len(s.samples))
	copy(out, s.samples)
	return out
}

// WalkMeta 遍历 root，对每个非常规目录条目（文件）回调 visit(rel, meta)。
// rel 为相对 root 的斜杠路径。内部复用 ScanTree 的遍历协议，但携带
// 完整元数据。回收站目录 DefaultTrashDir 恒被排除。
// 返回 per-walk 计数与错误样本（WalkStats）——并行调用互不干扰。
func WalkMeta(ctx context.Context, root string, opts WalkOptions, visit func(rel string, meta FileMetaFull)) (WalkStats, error) {
	if _, err := os.Lstat(root); err != nil {
		return WalkStats{}, err
	}
	if opts.DirWorkers <= 0 {
		opts.DirWorkers = 8
	}
	if opts.StatWorkers <= 0 {
		opts.StatWorkers = 1
	}
	excl := map[string]bool{DefaultTrashDir: true}
	for _, d := range opts.ExcludeDirs {
		excl[d] = true
	}
	for k := range opts.Exclude {
		excl[k] = true
	}

	var dirs, errs int64
	var sampler errSampler
	// dirCh 携带 (绝对路径, 相对路径)；入队或内联二选一，pending 协议同 ScanTree。
	type dirJob struct{ abs, rel string }
	dirCh := make(chan dirJob, 128)
	statCh := make(chan metaTask, 4096)

	type pendingCtr struct{ v atomic.Int64 }
	var pending atomic.Int64
	var closeOnce sync.Once

	var statWG sync.WaitGroup
	var dirWG sync.WaitGroup

	emit := func(t metaTask) {
		meta, err := fstatFull(t.dirfd, t.name)
		if err != nil {
			atomic.AddInt64(&errs, 1)
			sampler.add(t.rel, err) // N1：路径留痕（stat 失败点）
			return
		}
		if meta.Mode&0o170000 == 0o100000 { // S_IFREG
			visit(t.rel, meta)
		}
	}

	// stat 池
	for i := 0; i < opts.StatWorkers; i++ {
		statWG.Add(1)
		go func() {
			defer statWG.Done()
			for t := range statCh {
				if ctx.Err() != nil {
					t.done()
					continue
				}
				emit(t)
				t.done()
			}
		}()
	}

	var processDir func(dir, rel string)
	processDir = func(dir, rel string) {
		defer func() {
			if pending.Add(-1) == 0 {
				closeOnce.Do(func() { close(dirCh) })
			}
		}()
		f, err := os.Open(dir)
		if err != nil {
			atomic.AddInt64(&errs, 1)
			sampler.add(rel, err) // N1：路径留痕（目录打开失败）
			return
		}
		defer f.Close()
		var localWG sync.WaitGroup
		defer localWG.Wait()

		dirfd := int(f.Fd())
		for {
			if ctx.Err() != nil {
				return
			}
			batch, readErr := f.ReadDir(DefaultBatchSize)
			for _, e := range batch {
				if ctx.Err() != nil {
					return
				}
				name := e.Name()
				if e.IsDir() {
					if excl[name] {
						continue
					}
					atomic.AddInt64(&dirs, 1)
					child, childRel := dir+"/"+name, joinRel(rel, name)
					pending.Add(1)
					select {
					case dirCh <- dirJob{abs: child, rel: childRel}:
					default:
						processDir(child, childRel)
					}
					continue
				}
				localWG.Add(1)
				t := metaTask{dirfd: dirfd, name: name, rel: joinRel(rel, name), done: localWG.Done}
				select {
				case statCh <- t:
				default:
					emit(t)
					localWG.Done()
				}
			}
			if readErr == io.EOF {
				return
			}
			if readErr != nil {
				atomic.AddInt64(&errs, 1)
				sampler.add(rel, readErr) // N1：路径留痕（readdir 失败）
				return
			}
			if len(batch) == 0 {
				return
			}
		}
	}
	_ = pendingCtr{}

	for i := 0; i < opts.DirWorkers; i++ {
		dirWG.Add(1)
		go func() {
			defer dirWG.Done()
			for j := range dirCh {
				processDir(j.abs, j.rel)
			}
		}()
	}

	pending.Add(1)
	select {
	case dirCh <- dirJob{abs: root, rel: ""}:
	default:
		processDir(root, "")
	}

	dirWG.Wait()
	close(statCh)
	statWG.Wait()

	return WalkStats{Dirs: dirs, Errs: errs, ErrSamples: sampler.snapshot()}, nil
}

// metaTask 元数据 stat 任务。
type metaTask struct {
	dirfd int
	name  string
	rel   string
	done  func()
}

// joinRel 拼接相对路径（避免引入 path 开销）。
func joinRel(base, name string) string {
	if base == "" {
		return name
	}
	return base + "/" + name
}
