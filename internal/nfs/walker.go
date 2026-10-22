// Package nfs 提供面向 NFS 的只读扫描遍历内核与基准。
//
// 设计约束（继承旧工程验证过的模式，独立实现）：
//   - 分批流式 ReadDir，不物化整个目录条目，不排序；
//   - 目录读取与文件 stat 相互独立的有界并发：目录 worker 池控制同时
//     打开的目录数，stat 池控制并发 fstatat；两类并发互不占用槽位；
//   - stat 走目录 fd 相对 fstatat(AT_SYMLINK_NOFOLLOW)，只解析单个
//     路径分量，天然不跟随符号链接；
//   - 目录 fd 的生命周期：本目录发出的全部在途 stat 结束（localWG.Wait）
//     之后才关闭 fd——Wait 的 defer 必须注册在 Close 之后（LIFO 先执行），
//     否则 fstatat 会命中已关闭/复用的 fd；
//   - 终止协议：pending 计数器对"已登记未完成"的目录任务计数；入队或
//     内联二选一（select 不带 ctx 分支），每条路径恰好一次递减；归零即
//     关闭 dirCh，worker 排空退出——不存在"归零却没人关闭"的路径；
//   - ctx 取消只检查不参与发送仲裁：processDir/stat 池在循环顶部看到
//     取消立即返回，计数仍按协议闭合；
//   - 热路径无日志、无正则、无整树路径物化。
package nfs

import (
	"context"
	"io"
	"io/fs"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultBatchSize 单批 ReadDir 条目数。
const DefaultBatchSize = 500

// ScanOptions 控制一次扫描。
type ScanOptions struct {
	// DirWorkers 目录读取并发；<=0 时取 8。
	DirWorkers int
	// StatWorkers 并发 fstatat 数；<=0 时取 1。
	StatWorkers int
	// Stat 为 false 时纯 readdir 统计（条目类型来自 dirent），不做 fstatat。
	Stat bool
	// Exclude 目录 basename 排除集（含回收站名），命中则不下钻。
	Exclude map[string]bool
}

// ScanResult 是一次扫描的计数与延迟统计。
type ScanResult struct {
	Files   int64
	Dirs    int64
	Errors  int64
	StatP50 float64 // 毫秒
	StatP95 float64
	StatP99 float64

	statMillis []float64 // 仅在收敛后读取
}

// statTask 是 stat 池的任务单元：目录 fd + 单个文件名 + 完成回调。
type statTask struct {
	dirfd int
	name  string
	done  func()
}

// ScanTree 遍历 root 并统计文件/目录/错误；Stat=true 时逐文件 fstatat
// 并记录延迟分位数。返回错误仅当 root 自身无法 lstat。
func ScanTree(ctx context.Context, root string, opts ScanOptions) (*ScanResult, error) {
	if _, err := os.Lstat(root); err != nil {
		return nil, err
	}
	if opts.DirWorkers <= 0 {
		opts.DirWorkers = 8
	}
	if opts.StatWorkers <= 0 {
		opts.StatWorkers = 1
	}
	res := &ScanResult{}

	dirCh := make(chan string, 128)
	statCh := make(chan statTask, 4096)

	// 延迟采样无锁化：每 worker 一块本地切片，worker 退出时一次性归并。
	// 修复前的全局 statMu 每 stat 一次锁竞争，在热缓存（stat 亚微秒）下
	// 16 并发比 4 并发慢 ~38 倍（实测 2026-09-29 阿里云 NAS）。
	var mergeMu sync.Mutex
	var statWG sync.WaitGroup
	mergeLatencies := func(local []float64) {
		if len(local) == 0 {
			return
		}
		mergeMu.Lock()
		res.statMillis = append(res.statMillis, local...)
		mergeMu.Unlock()
	}

	// classifyNow 执行 stat 并返回 (mode, latencyMS)。
	classifyNow := func(t statTask) (fs.FileMode, float64) {
		start := time.Now()
		mode, err := fstatAt(t.dirfd, t.name)
		took := float64(time.Since(start).Nanoseconds()) / 1e6
		if err != nil {
			atomic.AddInt64(&res.Errors, 1)
			return mode, took
		}
		if mode.IsRegular() {
			atomic.AddInt64(&res.Files, 1)
		}
		// 符号链接与其他非常规文件：不计数、不跟随。
		return mode, took
	}

	var dirWG sync.WaitGroup
	var pending atomic.Int64
	closeOnce := sync.Once{}
	closeDirCh := func() { closeOnce.Do(func() { close(dirCh) }) }

	// stat 池：取消时只回收完成信号，不产出计数。
	// 每 worker 本地延迟切片，退出时一次性归并（热路径零锁）。
	// 注意：mergeLatencies 必须以闭包 defer 调用——`defer mergeLatencies(local)`
	// 的参数在 defer 语句处求值，捕获 len=0 的切片头，此后 append 的样本
	// 全部丢失（P1 缺陷：bench 热缓存下 p50/p95 恒 0 的根因）。
	if opts.Stat {
		for i := 0; i < opts.StatWorkers; i++ {
			statWG.Add(1)
			go func() {
				defer statWG.Done()
				local := make([]float64, 0, 4096)
				defer func() { mergeLatencies(local) }()
				for t := range statCh {
					if ctx.Err() != nil {
						t.done()
						continue
					}
					_, took := classifyNow(t)
					local = append(local, took)
					t.done()
				}
			}()
		}
	}

	// processDir 由目录 worker 或发送方内联执行。
	var processDir func(dir string)
	processDir = func(dir string) {
		defer func() {
			if pending.Add(-1) == 0 {
				closeDirCh()
			}
		}()

		f, err := os.Open(dir)
		if err != nil {
			atomic.AddInt64(&res.Errors, 1)
			return
		}
		// fd 生命周期：先注册 Close，再注册 Wait（LIFO 下 Wait 先执行，
		// 全部在途 stat 结束后 fd 才关闭）。
		defer f.Close()
		var localWG sync.WaitGroup
		defer localWG.Wait()

		// 内联背压路径的延迟样本：processDir 返回时并入（一次锁）。
		var inlineLatency []float64
		defer func() { mergeLatencies(inlineLatency) }()

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
					if opts.Exclude[name] {
						continue
					}
					atomic.AddInt64(&res.Dirs, 1)
					child := dir + "/" + name
					pending.Add(1)
					select {
					case dirCh <- child:
					default:
						// 通道满：内联处理保证推进，递减仍走其 defer。
						processDir(child)
					}
					continue
				}
				if opts.Stat {
					localWG.Add(1)
					t := statTask{dirfd: dirfd, name: name, done: localWG.Done}
					select {
					case statCh <- t:
					default:
						// stat 通道满：内联 stat 形成背压。
						// 延迟样本记入调用方 goroutine 的临时块，
						// processDir 结束时并入结果（同样零全局锁）。
						_, took := classifyNow(t)
						inlineLatency = append(inlineLatency, took)
						localWG.Done()
					}
					continue
				}
				// 纯 readdir 模式：靠 dirent 类型分类。
				if e.Type()&fs.ModeSymlink != 0 {
					continue
				}
				atomic.AddInt64(&res.Files, 1)
			}
			if readErr == io.EOF {
				return
			}
			if readErr != nil {
				atomic.AddInt64(&res.Errors, 1)
				return
			}
			if len(batch) == 0 {
				// 防御：空批 + nil 错误不应出现；一旦出现按 EOF 收敛，避免死循环。
				return
			}
		}
	}

	// 目录 worker 池。
	for i := 0; i < opts.DirWorkers; i++ {
		dirWG.Add(1)
		go func() {
			defer dirWG.Done()
			for d := range dirCh {
				processDir(d)
			}
		}()
	}

	// 根任务：入队或内联二选一；取消时 processDir 顶部检查立即返回，
	// pending 协议仍闭合（归零 → closeDirCh）。
	pending.Add(1)
	select {
	case dirCh <- root:
	default:
		processDir(root)
	}

	dirWG.Wait()
	close(statCh)
	statWG.Wait()

	res.computePercentiles()
	return res, nil
}

// computePercentiles 排序 stat 延迟并填充 P50/P95/P99。
func (r *ScanResult) computePercentiles() {
	n := len(r.statMillis)
	if n == 0 {
		return
	}
	s := append([]float64(nil), r.statMillis...)
	sort.Float64s(s)
	pick := func(p float64) float64 {
		idx := int(p * float64(n-1))
		return s[idx]
	}
	r.StatP50 = pick(0.50)
	r.StatP95 = pick(0.95)
	r.StatP99 = pick(0.99)
	r.statMillis = nil
}
