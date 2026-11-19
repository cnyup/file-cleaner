// emptydirs_streaming.go 巨目录流式空目录清理（2026-11-18，探针①-⑥结论落地）。
//
// 与 SweepEmptyDirs（两阶段：全树收集 → 深度排序 → 分批删除）的区别：
// 本引擎面向「单目录百万级直接子目录」的病态巨目录（生产 temp：
// 26,153,917 个直接子目录，实测白天 readdir 50/s、夜间 ~3.2k/s）。
//
// 流水线（顺序 cookie 链 + checkpoint 断点续扫 + 读删重叠）：
//
//	reader   getdents64 直读（per-entry d_off，探针同款）：Linux 标准库
//	         ReadDir 按整块缓冲推进偏移，ReadDir(n) 后越过未处理条目，
//	         不能当 cookie 用。每批（默认 500）落 checkpoint。
//	filter   dtype==dir → Lstat 取 mtime → 过年龄门进删除；young/文件
//	         条目只计数。同批条目互不为父子，并发处理。
//
// 安全语义（与 SweepEmptyDirs 对齐，三重保险留二重）：
//  1. mtime-only 年龄门（atime 是扫描者噪声，2026-10-20 实证）；
//  2. rmdir 内核兜底（ENOTEMPTY 物理不可能删掉带文件的目录）；
//  3. ~~删前 readdir 复查~~ 有意砍掉：reader 刚读过（间隔 < 一周期），
//     且 26M 目录各一次复查 = 另一场 2.3h 全量遍历（探针⑥速率倒推）。
//     竞态写入由 1+2 覆盖：业务正写的目录 mtime 必新；极端竞态由
//     ENOTEMPTY 兜底并计入 SkippedNonEmpty，下轮窗口续扫天然覆盖。
//
// 窗口语义：StopAt 到点优雅停止（处理完当前批、落 checkpoint）。
package nfs

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// EmptyDirsStreamingOptions 流式引擎参数。
type EmptyDirsStreamingOptions struct {
	// AgeGate mtime 年龄门（mtime-only，同 SweepEmptyDirs）。
	AgeGate time.Duration
	// Exclude 目录 basename 排除集（含回收站名），命中的子目录不删、只计数。
	Exclude map[string]bool
	// DryRun true 时只统计不删除。
	DryRun bool
	// OnRmdir 删除成功回调（审计挂点；nil = 无审计）。
	OnRmdir func(path string) error
	// CheckpointDir checkpoint 目录；空 = 内存 only（不可续扫）。
	CheckpointDir string
	// MaxEntries 本轮最多处理的条目数（测试/限流；<=0 不限）。
	MaxEntries int64
	// StopAt 到点优雅停止（窗口尾）；零值 = 不限。
	StopAt time.Time
	// Workers 批内并发（lstat+rmdir；<=0 取 8）。
	Workers int
	// BatchSize 每批条目数 = checkpoint 周期（<=0 取 500）。
	BatchSize int
	// StallTimeout 批处理时限（2026-11-19 生产首窗口教训：rmdir 挂死在
	// hard mount 上时 wg.Wait 永久阻塞，窗口熔断只防批间不防批内）。
	// 超时即放弃等待、Stopped=true 退出——卡死变可重试。<=0 不启用
	// （默认行为不变）；生产建议 10m。
	StallTimeout time.Duration
	// Now 可注入时钟（测试；nil = time.Now）。
	Now func() time.Time
}

// EmptyDirsStreamingStats 一轮流式清理统计。
type EmptyDirsStreamingStats struct {
	EntriesScanned  int64 // 本轮实际读取的目录条目数
	Resumed         int64 // checkpoint 恢复时跳过的已处理条目数
	Removed         int64 // 物理删除数（DryRun 时为「将删除」计数）
	SkippedYoung    int64 // mtime 未过年龄门
	SkippedNonEmpty int64 // rmdir ENOTEMPTY / root 下出现文件条目
	SkippedGone     int64 // lstat/rmdir ENOENT（他人已删）
	SkippedExclude  int64 // 命中排除集
	Errors          int64 // 其他错误
	Checkpointed    int64 // checkpoint 落盘次数
	Stalled         int64 // 批处理超时次数（StallTimeout 触发；服务端挂起的信号）
	Stopped         bool  // 到达 StopAt/MaxEntries/批超时 优雅或保护性退出（未完，可续）
	Elapsed         time.Duration
}

// streamingCheckpoint checkpoint 文件内容（单行 JSON，人可读可审计）。
type streamingCheckpoint struct {
	Root    string `json:"root"`
	Cookie  int64  `json:"cookie"`
	Scanned int64  `json:"scanned"`
	Removed int64  `json:"removed"`
	SavedAt string `json:"saved_at"`
}

const streamingDefaultBatch = 500

// SweepEmptyDirsStreaming 对 root 的直接子目录做一轮流式清理。
// root 自身不动；文件条目只计数；不递归（深层由后续轮次处理）。
func SweepEmptyDirsStreaming(root string, opts EmptyDirsStreamingOptions) (*EmptyDirsStreamingStats, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Workers <= 0 {
		opts.Workers = 8
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = streamingDefaultBatch
	}

	start := opts.Now()
	stats := &EmptyDirsStreamingStats{}
	defer func() { stats.Elapsed = opts.Now().Sub(start) }()

	// 恢复 checkpoint。
	var resumeCookie int64
	var resumedScanned int64
	if opts.CheckpointDir != "" {
		ck, err := loadStreamingCheckpoint(opts.CheckpointDir, root)
		if err != nil {
			return nil, err
		}
		if ck != nil {
			resumeCookie = ck.Cookie
			resumedScanned = ck.Scanned
			stats.Resumed = resumedScanned
		}
	}

	f, err := os.Open(root)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	reader := newDirEntryReader(f)
	if err := reader.seekCookie(resumeCookie); err != nil {
		return nil, fmt.Errorf("seek to cookie %d: %w", resumeCookie, err)
	}

	cutoff := opts.Now().Add(-opts.AgeGate)
	stopNow := func() bool { return !opts.StopAt.IsZero() && opts.Now().After(opts.StopAt) }

	var handled int64
	for {
		if stopNow() {
			stats.Stopped = true
			break
		}
		// MaxEntries 截取本批大小。
		want := opts.BatchSize
		if opts.MaxEntries > 0 && handled+int64(want) > opts.MaxEntries {
			want = int(opts.MaxEntries - handled)
			if want <= 0 {
				break
			}
		}
		batch, lastOff, eof, err := reader.next(want)
		if err != nil {
			return nil, fmt.Errorf("readdir %s: %w", root, err)
		}
		if len(batch) == 0 {
			// EOF（或 seek 后无残余）：完成轮不落盘（任务已完成无进度可续）。
			if opts.CheckpointDir != "" {
				_ = removeStreamingCheckpoint(opts.CheckpointDir, root)
			}
			break
		}

		// 批内并发处理（同层目录互不为父子）。
		// 2026-11-19 生产卡死教训补全：派发（sem 获取）与 worker（rmdir）
		// 任一环节挂起都会卡死批——超时保护必须罩住「派发+等待」整段，
		// 只罩 wg.Wait 不够（派发 goroutine 自身可阻塞在 sem）。
		var wg sync.WaitGroup
		sem := make(chan struct{}, opts.Workers)
		stallErr := false
		dispatch := func() {
			for _, e := range batch {
				if !e.isDir {
					// 文件条目：巨目录里出现文件说明业务在用 root——只计数。
					atomic.AddInt64(&stats.SkippedNonEmpty, 1)
					continue
				}
				if opts.Exclude[e.name] {
					atomic.AddInt64(&stats.SkippedExclude, 1)
					continue
				}
				full := root + "/" + e.name
				wg.Add(1)
				sem <- struct{}{}
				go func(full string) {
					defer wg.Done()
					defer func() { <-sem }()
					processStreamingEntry(full, cutoff, opts, stats)
				}(full)
			}
		}
		if opts.StallTimeout > 0 {
			done := make(chan struct{})
			go func() { dispatch(); wg.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(opts.StallTimeout):
				// 放弃等待：泄漏的 dispatch/worker goroutine 无害化——
				// 它们只写 atomic 计数器，函数返回后 stats 不再被读取方
				// 依赖（结果已快照）。不落本批 checkpoint（cookie 未推进，
				// 下轮重扫本批；rmdir 幂等，ENOENT 计 Gone，安全）。
				atomic.AddInt64(&stats.Stalled, 1)
				stats.Stopped = true
				stallErr = true
			}
		} else {
			dispatch()
			wg.Wait()
		}
		if stallErr {
			return stats, nil
		}

		handled += int64(len(batch))
		stats.EntriesScanned = handled

		// 落 checkpoint（每批一次，含最后一批；DryRun 同样落——
		// dry-run 续扫语义成立。完成轮的 checkpoint 由 eof 清理）。
		if opts.CheckpointDir != "" && lastOff > 0 {
			ck := streamingCheckpoint{
				Root:    root,
				Cookie:  lastOff,
				Scanned: resumedScanned + handled,
				Removed: stats.Removed,
				SavedAt: opts.Now().Format(time.RFC3339),
			}
			if err := saveStreamingCheckpoint(opts.CheckpointDir, ck); err != nil {
				atomic.AddInt64(&stats.Errors, 1)
			} else {
				atomic.AddInt64(&stats.Checkpointed, 1)
			}
		}

		if eof {
			// 读完且处理完：任务完成，checkpoint 自愈。
			if opts.CheckpointDir != "" {
				_ = removeStreamingCheckpoint(opts.CheckpointDir, root)
			}
			break
		}
	}
	return stats, nil
}

// processStreamingEntry 单条目：lstat 判龄 → OnRmdir → rmdir。
func processStreamingEntry(full string, cutoff time.Time, opts EmptyDirsStreamingOptions, stats *EmptyDirsStreamingStats) {
	st, err := os.Lstat(full)
	if err != nil {
		if os.IsNotExist(err) {
			atomic.AddInt64(&stats.SkippedGone, 1)
		} else {
			atomic.AddInt64(&stats.Errors, 1)
		}
		return
	}
	if st.ModTime().After(cutoff) {
		atomic.AddInt64(&stats.SkippedYoung, 1)
		return
	}
	if opts.DryRun {
		atomic.AddInt64(&stats.Removed, 1)
		return
	}
	if opts.OnRmdir != nil {
		if err := opts.OnRmdir(full); err != nil {
			atomic.AddInt64(&stats.Errors, 1)
			return
		}
	}
	if err := os.Remove(full); err != nil {
		if os.IsNotExist(err) {
			atomic.AddInt64(&stats.SkippedGone, 1)
		} else {
			atomic.AddInt64(&stats.SkippedNonEmpty, 1)
		}
		return
	}
	atomic.AddInt64(&stats.Removed, 1)
}

// loadStreamingCheckpoint 读 checkpoint；不存在返回 (nil, nil)；损坏按不存在处理
//（fail-open to rescan：删除幂等，重扫安全）。
func loadStreamingCheckpoint(dir, root string) (*streamingCheckpoint, error) {
	raw, err := os.ReadFile(checkpointPath(dir, root))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ck streamingCheckpoint
	if err := json.Unmarshal(raw, &ck); err != nil {
		return nil, nil
	}
	if ck.Root != root {
		return nil, nil
	}
	return &ck, nil
}

// saveStreamingCheckpoint 原子写：tmp + rename。
func saveStreamingCheckpoint(dir string, ck streamingCheckpoint) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	raw, err := json.Marshal(ck)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	p := checkpointPath(dir, ck.Root)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func removeStreamingCheckpoint(dir, root string) error {
	err := os.Remove(checkpointPath(dir, root))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func checkpointPath(dir, root string) string {
	b := []byte(root)
	for i := range b {
		if b[i] == '/' || b[i] == ':' || b[i] == ' ' {
			b[i] = '_'
		}
	}
	return dir + "/emptydirs-" + string(b) + ".json"
}
