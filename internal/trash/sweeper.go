// sweeper：.retired/.purging 桶的后台物理删除。
//
// 流程（每桶）：.retired/<bucket> --rename--> .purging/<bucket>（崩溃标记）
// → 逐文件 unlink（moveSem 限速）→ 自底向上 rmdir → 桶消失。
// 崩溃恢复：启动 SweepOnce 发现 .purging 残留即续删（幂等）。
// 活跃桶（trash 根下的日期桶）永不触碰。
package trash

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// SweepOnce 执行一轮巡检：处理全部 .retired/.purging 桶。
// 返回删除的文件数与目录数，并落一条 sweep_result 审计事件（含
// files/dirs/duration_ms）。每轮必写事件（含空轮）——物理删除是全系统
// 唯一不可逆操作，"跑过且无事"与"没跑"必须可区分（2026-10-29 巡检：
// 骨架桶 files=0 曾使 SweepLoop 静默且零审计，排查花了 8 轮探针）。
// 不持策略锁（物理删除不阻塞主清理/retire；桶级安全由 rename 原子性保证）。
// 心跳：注入 SetHeartbeat 后，长删期间每 5 分钟（首条立即）打一行进度
// ——消除长跑期间 journal 静默盲区（2026-11-01 晨报：video 1h55m 长跑，
// 轮首/轮尾两行之间零观测，ocr 后位策略整轮等位静默）。未注入则零开销。
func (m *Mover) SweepOnce() (int, int, error) {
	start := nowForTest()
	var files, dirs int
	// 心跳：原子计数由 deleteTree 逐条累加，ticker goroutine 只读快照。
	// 计数与返回值同源（同一计数器），不做二次对账。
	var progress sweepProgress
	var beatStop, beatDone chan struct{}
	if beat := m.heartbeat; beat != nil {
		beatStop = make(chan struct{})
		beatDone = make(chan struct{})
		go sweepHeartbeat(beat, m.policyID, &progress, beatStop, beatDone)
	}
	stopBeat := func() {
		if beatStop != nil {
			close(beatStop)
			<-beatDone
		}
	}

	n, dn, err := m.sweepDir(m.layout.PurgingDir(), true, &progress)
	files += n
	dirs += dn
	if err == nil {
		// 2) 处理 .retired：逐桶 rename 进 .purging 后删除
		n2, dn2, err2 := m.sweepDir(m.layout.RetiredDir(), false, &progress)
		files += n2
		dirs += dn2
		err = err2
	}
	stopBeat()
	// sweep_result 每轮必落盘（失败轮 ok:false + err；写失败不吞删除
	// 结果——事件是留痕，只读快照，绝非删除的前置条件）。
	m.writeSweepResult(start, files, dirs, err)
	return files, dirs, err
}
// writeSweepResult 落一条 sweep_result 事件（经 audit 导出 API）。
// 留痕用途：不进入 FindOpenIntents / restore 投影（store.go 查询不含
// 此类型），写失败不吞删除结果——事件是留痕，不是删除的前置条件。
func (m *Mover) writeSweepResult(start time.Time, files, dirs int, sweepErr error) {
	if m.audit == nil {
		return
	}
	runID := "sweep-" + start.Format("20060102-150405")
	msg := ""
	ok := sweepErr == nil
	if !ok {
		msg = sweepErr.Error()
	}
	_ = m.audit.WriteSweepResult(runID, m.policyID, ok, msg, int64(files), int64(dirs), nowForTest().Sub(start).Milliseconds())
}

// sweepDir 处理一个控制目录下的全部桶。progress 非 nil 时逐桶累加进度。
func (m *Mover) sweepDir(dir string, alreadyPurging bool, progress *sweepProgress) (int, int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	totalFiles := 0
	totalDirs := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		bucket := e.Name()
		src := filepath.Join(dir, bucket)
		var target string
		if alreadyPurging {
			target = src // 已在删除区，直接续删
		} else {
			if err := os.MkdirAll(m.layout.PurgingDir(), 0o755); err != nil {
				return totalFiles, totalDirs, err
			}
			target = filepath.Join(m.layout.PurgingDir(), bucket)
			if err := os.Rename(src, target); err != nil {
				return totalFiles, totalDirs, fmt.Errorf("sweep: stage %s: %w", bucket, err)
			}
		}
		n, dn, err := m.deleteTree(target, progress)
		totalFiles += n
		totalDirs += dn
		if err != nil {
			return totalFiles, totalDirs, err
		}
	}
	return totalFiles, totalDirs, nil
}

// deleteTree 逐文件 unlink + 自底向上 rmdir。返回删除文件数与目录数。
// progress 非 nil 时逐文件累加（桶内进度可见，2026-11-11 观察项 1）——
// 旧实现只在整桶删完后累加一次，单桶 645k files / 133 分钟期间心跳恒 0。
func (m *Mover) deleteTree(root string, progress *sweepProgress) (int, int, error) {
	// 后序遍历：先文件后目录。
	var walk func(dir string) (int, int, error)
	walk = func(dir string) (int, int, error) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return 0, 0, err
		}
		n := 0
		dn := 0
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			if e.IsDir() {
				sub, subd, err := walk(p)
				if err != nil {
					return n, dn, err
				}
				n += sub
				dn += subd
				continue
			}
			if e.Type()&fs.ModeSymlink != 0 {
				// 符号链接：unlink 链接本身（不跟随）
				if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
					return n, dn, err
				}
				n++
				if progress != nil {
					progress.add(1, 0)
				}
				continue
			}
			// unlink 带限速（moveSem）保证不冲垮 NFS
			if m.moveSem != nil {
				m.moveSem <- struct{}{}
			}
			err := os.Remove(p)
			if m.moveSem != nil {
				<-m.moveSem
			}
			if err != nil && !os.IsNotExist(err) {
				return n, dn, err
			}
			n++
			if progress != nil {
				progress.add(1, 0)
			}
		}
		if dir != root {
			if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
				return n, dn, err
			}
			dn++
			if progress != nil {
				progress.add(0, 1)
			}
		}
		return n, dn, nil
	}
	n, dn, err := walk(root)
	if err != nil {
		return n, dn, err
	}
	if err := os.Remove(root); err != nil && !os.IsNotExist(err) {
		return n, dn, err
	}
	dn++ // 桶根
	if progress != nil {
		progress.add(0, 1)
	}
	return n, dn, nil
}

// nowForTest 测试时钟注入点。
var nowForTest = time.Now

// sweepProgress 心跳进度计数（原子；deleteTree 桶级累加，ticker 只读快照）。
// 低 32 位 files、高 32 位 dirs 打包进单个 atomic.Uint64——桶级粒度足够
// 消除长跑盲区（桶内删除以 NFS 速率进行，秒级进度无额外价值），且单计数
// 器使"心跳计数与返回值同源"可静态核验。
type sweepProgress struct{ v atomic.Uint64 }

func (p *sweepProgress) add(files, dirs uint32) {
	p.v.Add(uint64(files) | uint64(dirs)<<32)
}

func (p *sweepProgress) load() (files, dirs uint32) {
	v := p.v.Load()
	return uint32(v), uint32(v >> 32)
}

// sweepHeartbeat 心跳 goroutine 主体：首条立即、此后每 5 分钟打一行。
// stopBeat 由 SweepOnce 关闭；beatDone 由本函数关闭（同步回收 goroutine）。
func sweepHeartbeat(beat func(string), policyID string, progress *sweepProgress, stop, done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	emit := func() {
		f, d := progress.load()
		beat(fmt.Sprintf("sweep: policy %s: in progress — %d files, %d dirs deleted so far", policyID, f, d))
	}
	emit() // 首条立即：长跑开始即有观测点，不等第一个 5 分钟
	for {
		select {
		case <-ticker.C:
			emit()
		case <-stop:
			return
		}
	}
}

// heartbeatInterval 心跳周期（测试可缩短以观测桶内进度采样）。
var heartbeatInterval = 5 * time.Minute
