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

// lastWalkCounters 保存最近一次 WalkMeta 的目录/错误计数（单 run 内有效，
// 仅在 WalkMeta 返回后读取）。
var lastWalkCounters struct {
	dirs, errors int64
	// errSamples 前 8 条 scan 错误样本（2026-11-03 巡检观察①：errors=2
	// 无逐条明细，事后无法归因）。mu 保护 append；容量封顶防 audit 膨胀。
	errSamples []string
	mu         sync.Mutex
}

// walkErrSample 记录一条 scan 错误样本（路径 + 错误），封顶 8 条。
func walkErrSample(path string, err error) {
	lastWalkCounters.mu.Lock()
	defer lastWalkCounters.mu.Unlock()
	if len(lastWalkCounters.errSamples) >= 8 {
		return
	}
	lastWalkCounters.errSamples = append(lastWalkCounters.errSamples, path+": "+err.Error())
}

// LastWalkErrorSamples 返回最近一次 WalkMeta 的错误样本（≤8 条）。
// 供 runner 写入 run_finished extra——ENOENT race 噪声与真实故障可区分。
func LastWalkErrorSamples() []string {
	lastWalkCounters.mu.Lock()
	defer lastWalkCounters.mu.Unlock()
	out := make([]string, len(lastWalkCounters.errSamples))
	copy(out, lastWalkCounters.errSamples)
	return out
}

// LastWalkCounters 返回最近一次 WalkMeta 的 (dirs, errors)。
func LastWalkCounters() (int64, int64) {
	return lastWalkCounters.dirs, lastWalkCounters.errors
}

// WalkMeta 遍历 root，对每个非常规目录条目（文件）回调 visit(rel, meta)。
// rel 为相对 root 的斜杠路径。内部复用 ScanTree 的遍历协议，但携带
// 完整元数据。回收站目录 DefaultTrashDir 恒被排除。
func WalkMeta(ctx context.Context, root string, opts WalkOptions, visit func(rel string, meta FileMetaFull)) error {
	// N1：每轮清零样本（LastWalkCounters 同语义——只反映最近一次）。
	lastWalkCounters.mu.Lock()
	lastWalkCounters.errSamples = nil
	lastWalkCounters.mu.Unlock()
	if _, err := os.Lstat(root); err != nil {
		return err
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
			walkErrSample(t.rel, err) // N1：路径留痕（stat 失败点）
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
			walkErrSample(rel, err) // N1：路径留痕（目录打开失败）
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
				walkErrSample(rel, readErr) // N1：路径留痕（readdir 失败）
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

	lastWalkCounters.dirs = dirs
	lastWalkCounters.errors = errs
	return nil
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
