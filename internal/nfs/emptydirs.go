// emptydirs.go 空目录清理（对齐旧版 remove_empty_dirs 语义）。
//
// 两阶段算法，独立于 ScanTree 主扫描（不侵入文件清理热路径）：
//
//	阶段一（并发收集）：自顶向下遍历，readdir 为空的目录记为候选，
//	                          取 lstat 得 mtime；排除集与文件清理一致。
//	阶段二（串行删除）：候选按路径深度降序（子先于父），逐个复查：
//	                          readdir 仍为空 且 mtime 超龄 → rmdir。
//
// 年龄门为 mtime-only（与文件清理的 mtime+atime 门有意的不同）：
// 空目录的 atime 是扫描者噪声——本工具的文件扫描每天 readdir 全部目录，
// relatime 下持续刷新 atime，mtime+atime 门将永远不会放行（自我挫败，
// 2026-10-20 runner 集成测试实证）。而目录 mtime 只在条目增删时更新：
// 业务在目录里建/删/移文件必然刷新 mtime——mtime 超龄 = 7d 内无任何
// 条目活动，是安全且有效的判据。
//
// 安全三重保险（无数据丢失路径）：
//  1. 阶段二删前 readdir 复查（TOCTOU 防线一：收集后写入的新文件挡在 rmdir 前）
//  2. mtime 年龄门（防线二：业务正在写入的目录 mtime 新，天然被挡）
//  3. rmdir 物理特性（防线三：os.Remove 对非空目录必返 ENOTEMPTY，复查后
//     写入的极端竞态由内核兜底，物理上不可能删掉带文件的目录）
//
// 级联语义：收集时即空且超龄的嵌套目录单轮自底向上连删；因本轮删除
// 才变空的父目录（收集时非空）不进本轮候选，留待下轮。
package nfs

import (
	"io"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// EmptyDirsOptions 空目录清理参数。
type EmptyDirsOptions struct {
	// AgeGate 空目录年龄门：mtime 早于 now-AgeGate 才可删（mtime-only，
	// 见文件头注释——atime 对目录是扫描者噪声）。
	AgeGate time.Duration
	// Exclude 目录 basename 排除集（含回收站名），命中则不下钻、不删。
	Exclude map[string]bool
	// DryRun true 时只收集统计，不执行删除。
	DryRun bool
	// OnRmdir 删除成功回调（审计事务挂点；nil = 无审计）。
	// 每个目录一次调用：先 OnRmdir(path, nil) 作为 intent-before-hook 由
	// 调用方决定事务形态，返回 error 时该目录跳过删除（fail-closed）。
	OnRmdir func(path string) error
	// DirWorkers 收集阶段并发（<=0 取 8）。
	DirWorkers int
	// now 可注入时钟（测试）。
	now func() time.Time
}

// EmptyDirsStats 一轮空目录清理统计。
type EmptyDirsStats struct {
	Candidates int64 // 阶段一收集到的空目录候选数
	Removed    int64 // 物理删除数
	Skipped    int64 // 复查不过（非空/未超龄）跳过数
	Errors     int64 // lstat/rmdir 错误数
}

// emptyDirCandidate 阶段一产物：空目录路径 + mtime 快照。
type emptyDirCandidate struct {
	path  string
	mtime time.Time
}

// SweepEmptyDirs 执行一轮空目录清理（收集 + 删除）。
// root 自身永不被删（只处理其子树）。
func SweepEmptyDirs(root string, opts EmptyDirsOptions) (*EmptyDirsStats, error) {
	if opts.now == nil {
		opts.now = time.Now
	}
	cands, err := collectEmptyDirs(root, opts)
	if err != nil {
		return nil, err
	}
	stats := &EmptyDirsStats{Candidates: int64(len(cands))}

	// 深度降序：子目录先删；同深度按路径排序保证确定性。
	sort.Slice(cands, func(i, j int) bool {
		pi, pj := cands[i].path, cands[j].path
		di, dj := depth(pi), depth(pj)
		if di != dj {
			return di > dj
		}
		return pi < pj
	})

	cutoff := opts.now().Add(-opts.AgeGate)
	for _, c := range cands {
		// 复查一：目录仍存在且 readdir 为空。
		entries, err := os.ReadDir(c.path)
		if err != nil {
			if os.IsNotExist(err) {
				atomic.AddInt64(&stats.Skipped, 1)
				continue
			}
			atomic.AddInt64(&stats.Errors, 1)
			continue
		}
		if len(entries) > 0 {
			atomic.AddInt64(&stats.Skipped, 1)
			continue
		}
		// 复查二：年龄门（收集时刻的 mtime 快照；readdir 复查不刷 mtime，
		// 但业务增删条目会刷——快照与复查共同覆盖竞态窗口）。
		if c.mtime.After(cutoff) {
			atomic.AddInt64(&stats.Skipped, 1)
			continue
		}

		if opts.DryRun {
			atomic.AddInt64(&stats.Removed, 1)
			continue
		}
		if opts.OnRmdir != nil {
			if err := opts.OnRmdir(c.path); err != nil {
				atomic.AddInt64(&stats.Errors, 1)
				continue
			}
		}
		if err := os.Remove(c.path); err != nil {
			// ENOTEMPTY：复查后写入的极端竞态——按设计由内核兜底，计跳过。
			atomic.AddInt64(&stats.Skipped, 1)
			continue
		}
		atomic.AddInt64(&stats.Removed, 1)
	}
	return stats, nil
}

// collectEmptyDirs 阶段一：并发遍历收集空目录候选（含 inode 时间戳）。
// 遍历模式与 walker.ScanTree 同族：dir worker 池 + pending 计数协议。
func collectEmptyDirs(root string, opts EmptyDirsOptions) ([]emptyDirCandidate, error) {
	if _, err := os.Lstat(root); err != nil {
		return nil, err
	}
	workers := opts.DirWorkers
	if workers <= 0 {
		workers = 8
	}

	var mu sync.Mutex
	var cands []emptyDirCandidate
	var pending atomic.Int64
	dirCh := make(chan string, 128)
	closeOnce := sync.Once{}
	closeDirCh := func() { closeOnce.Do(func() { close(dirCh) }) }

	var processDir func(dir string)
	processDir = func(dir string) {
		defer func() {
			if pending.Add(-1) == 0 {
				closeDirCh()
			}
		}()
		// mtime 快照必须先于本目录的 Open/ReadDir 取（防御性：readdir 不
		// 刷 mtime，但保持"快照先于观察"的一致纪律）。
		st, stErr := os.Lstat(dir)
		f, err := os.Open(dir)
		if err != nil {
			return
		}
		defer f.Close()

		var children []string
		empty := true
		for {
			batch, readErr := f.ReadDir(DefaultBatchSize)
			for _, e := range batch {
				name := e.Name()
				if !e.IsDir() {
					empty = false
					continue
				}
				if opts.Exclude[name] {
					empty = false // 排除目录占位：父目录不因它而空
					continue
				}
				children = append(children, dir+"/"+name)
			}
			if readErr == io.EOF || (readErr == nil && len(batch) == 0) {
				break
			}
			if readErr != nil {
				return
			}
		}

		if empty && dir != root && stErr == nil {
			mu.Lock()
			cands = append(cands, emptyDirCandidate{
				path:  dir,
				mtime: st.ModTime(),
			})
			mu.Unlock()
		}
		for _, child := range children {
			pending.Add(1)
			select {
			case dirCh <- child:
			default:
				processDir(child)
			}
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for d := range dirCh {
				processDir(d)
			}
		}()
	}
	pending.Add(1)
	select {
	case dirCh <- root:
	default:
		processDir(root)
	}
	wg.Wait()
	return cands, nil
}

// depth 返回路径分隔符数（深度近似；仅用于子先于父的排序）。
func depth(p string) int {
	n := 0
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			n++
		}
	}
	return n
}
