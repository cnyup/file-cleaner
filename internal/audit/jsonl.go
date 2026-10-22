// Package audit 实现批量预提交审计：JSONL 事件流是唯一事实源。
//
// 事务协议（核心不变量）：
//   intent append → fsync → 本批文件操作 → result append → fsync
// 任何时刻崩溃，盘上要么只有 intent（未执行或执行了一部分），
// 要么 intent+result 齐全；不存在"文件已动但盘上无 intent"的窗口。
// 启动时 FindOpenIntents 列出未闭环 intent 供核对。
package audit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// 事件类型。
const (
	EventIntent  = "intent"
	EventResult  = "result"
	EventSkipped = "skipped_agg"
	EventRunOn   = "run_started"
	EventRunOff  = "run_finished"
)

// Event 是 JSONL 审计事件。
// OK 为 *bool：result 类事件必须显式落 "ok":true/false（审计取证按
// ok:false 检索失败记录）；intent/边界事件为 nil，不落 ok 字段。
type Event struct {
	TS       int64          `json:"ts"`
	RunID    string         `json:"run_id"`
	PolicyID string         `json:"policy_id"`
	Type     string         `json:"type"`
	Src      string         `json:"src,omitempty"`
	Trash    string         `json:"trash,omitempty"`
	OK       *bool          `json:"ok,omitempty"`
	Err      string         `json:"err,omitempty"`
	Extra    map[string]any `json:"extra,omitempty"`
}

func okPtr(b bool) *bool { return &b }

// MoveIntent 一次移动的意图。
type MoveIntent struct {
	RunID    string
	PolicyID string
	Src      string
	Trash    string
	Inode    uint64
}

// JSONL 是追加型审计 writer（并发安全：CommitMoves 串行化）。
type JSONL struct {
	mu   sync.Mutex
	f    *os.File
	path string
}

// OpenJSONL 打开（必要时创建）审计文件。
func OpenJSONL(path string) (*JSONL, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return nil, fmt.Errorf("audit: open %s: %w", path, err)
	}
	return &JSONL{f: f, path: path}, nil
}

// Path 返回审计文件路径。
func (j *JSONL) Path() string { return j.path }

// CommitMoves 执行批量事务协议。action 返回 per-file 结果切片
// （与 intents 等长，nil=该文件成功）；result 逐条按物理结果落盘——
// 批内部分成功不被误记失败，err 归因到各自文件。返回首个非 nil 错误
// 作为聚合错误。任一阶段审计写失败 → 立即返回，action 不执行。
func (j *JSONL) CommitMoves(intents []MoveIntent, action func() []error) error {
	if len(intents) == 0 {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()

	// 1) intent 批量落盘 + fsync
	if err := j.appendLocked(intentsToEvents(intents, EventIntent)); err != nil {
		return fmt.Errorf("audit: persist intents: %w", err)
	}

	// 2) 执行本批动作
	perFile := action()

	// 3) result 落盘 + fsync（即使失败也要写，供恢复核对）
	evs := intentsToEvents(intents, EventResult)
	for i := range evs {
		if i < len(perFile) && perFile[i] != nil {
			evs[i].OK = okPtr(false)
			evs[i].Err = perFile[i].Error()
		} else {
			evs[i].OK = okPtr(true)
		}
	}
	if err := j.appendLocked(evs); err != nil {
		return fmt.Errorf("audit: persist results: %w", err)
	}
	for _, e := range perFile {
		if e != nil {
			return e
		}
	}
	return nil
}

// RmdirIntent 一次空目录删除的意图。
type RmdirIntent struct {
	RunID    string
	PolicyID string
	Path     string
}

// CommitRmdirs rmdir 批量事务（与 CommitMoves 同协议：intent 批落盘 →
// action → result per-dir 按物理结果落盘）。rmdir 无 inode 核验需求
// （物理安全由 ENOTEMPTY 内核兜底）；审计写失败 → action 不执行。
func (j *JSONL) CommitRmdirs(intents []RmdirIntent, action func() []error) error {
	if len(intents) == 0 {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()

	intentEvents := make([]Event, len(intents))
	for i, it := range intents {
		intentEvents[i] = Event{
			TS: nowUnixNano(), RunID: it.RunID, PolicyID: it.PolicyID,
			Type: "rmdir_intent", Src: it.Path,
		}
	}
	if err := j.appendLocked(intentEvents); err != nil {
		return fmt.Errorf("audit: persist rmdir intents: %w", err)
	}

	perDir := action()

	resultEvents := make([]Event, len(intents))
	for i, it := range intents {
		resultEvents[i] = Event{
			TS: nowUnixNano(), RunID: it.RunID, PolicyID: it.PolicyID,
			Type: "rmdir_result", Src: it.Path,
		}
		if i < len(perDir) && perDir[i] != nil {
			resultEvents[i].OK = okPtr(false)
			resultEvents[i].Err = perDir[i].Error()
		} else {
			resultEvents[i].OK = okPtr(true)
		}
	}
	if err := j.appendLocked(resultEvents); err != nil {
		return fmt.Errorf("audit: persist rmdir results: %w", err)
	}
	for _, e := range perDir {
		if e != nil {
			return e
		}
	}
	return nil
}

// RunStarted / RunFinished / AppendSkippedAgg 运行边界与聚合事件。
func (j *JSONL) RunStarted(runID, policyID, policyHash string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	_ = j.appendLocked([]Event{{
		TS: nowUnixNano(), RunID: runID, PolicyID: policyID, Type: EventRunOn,
		Extra: map[string]any{"policy_hash": policyHash},
	}})
}

func (j *JSONL) RunFinished(runID, policyID string, totals map[string]int64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	_ = j.appendLocked([]Event{{
		TS: nowUnixNano(), RunID: runID, PolicyID: policyID, Type: EventRunOff,
		Extra: map[string]any{"totals": totals},
	}})
}

func (j *JSONL) AppendSkippedAgg(runID, policyID string, byReason map[string]int64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	_ = j.appendLocked([]Event{{
		TS: nowUnixNano(), RunID: runID, PolicyID: policyID, Type: EventSkipped,
		Extra: map[string]any{"by_reason": byReason},
	}})
}

// Sync 强制 fsync（测试与关闭前用）。
func (j *JSONL) Sync() {
	j.mu.Lock()
	defer j.mu.Unlock()
	_ = j.f.Sync()
}

// CommitRestore 恢复事务的 intent 部分（action 由 trash 层执行后调用
// WriteRestoreResult 闭环）。srcPath=原路径，trashPath=回收站路径——
// 字段语义与 move 事件严格对齐。
func (j *JSONL) CommitRestore(runID, policyID, srcPath, trashPath string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.appendLocked([]Event{{
		TS: nowUnixNano(), RunID: runID, PolicyID: policyID, Type: "restore_intent",
		Src: srcPath, Trash: trashPath,
	}})
}

// CommitPurge 物理删除事务的 intent 部分。
func (j *JSONL) CommitPurge(runID, policyID, trashPath string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.appendLocked([]Event{{
		TS: nowUnixNano(), RunID: runID, PolicyID: policyID, Type: "purge_intent",
		Trash: trashPath,
	}})
}

// WriteRestoreResult / WritePurgeResult 供 trash 层完成事务闭环。
func (j *JSONL) WriteRestoreResult(runID, policyID, trashPath, srcPath string, ok bool, errMsg string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.appendLocked([]Event{{
		TS: nowUnixNano(), RunID: runID, PolicyID: policyID, Type: "restore_result",
		Trash: trashPath, Src: srcPath, OK: okPtr(ok), Err: errMsg,
	}})
}

func (j *JSONL) WritePurgeResult(runID, policyID, trashPath string, ok bool, errMsg string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.appendLocked([]Event{{
		TS: nowUnixNano(), RunID: runID, PolicyID: policyID, Type: "purge_result",
		Trash: trashPath, OK: okPtr(ok), Err: errMsg,
	}})
}

// CommitPurgeBatch 批量 purge 事务：一批 intent（一次 fsync）→ action
// （调用方并发 unlink）→ 一批 result（一次 fsync）。与 CommitMoves 同构：
// action 返回 per-file 结果，result 逐条按物理结果落盘。
func (j *JSONL) CommitPurgeBatch(runID, policyID string, trashPaths []string, action func() []error) error {
	if len(trashPaths) == 0 {
		return nil
	}
	j.mu.Lock()
	intents := make([]Event, len(trashPaths))
	for i, p := range trashPaths {
		intents[i] = Event{TS: nowUnixNano(), RunID: runID, PolicyID: policyID, Type: "purge_intent", Trash: p}
	}
	if err := j.appendLocked(intents); err != nil {
		j.mu.Unlock()
		return fmt.Errorf("audit: persist purge intents: %w", err)
	}
	j.mu.Unlock()

	perFile := action()

	j.mu.Lock()
	defer j.mu.Unlock()
	results := make([]Event, len(trashPaths))
	for i, p := range trashPaths {
		results[i] = Event{TS: nowUnixNano(), RunID: runID, PolicyID: policyID, Type: "purge_result", Trash: p}
		if i < len(perFile) && perFile[i] != nil {
			results[i].OK = okPtr(false)
			results[i].Err = perFile[i].Error()
		} else {
			results[i].OK = okPtr(true)
		}
	}
	if err := j.appendLocked(results); err != nil {
		return fmt.Errorf("audit: persist purge results: %w", err)
	}
	for _, e := range perFile {
		if e != nil {
			return e
		}
	}
	return nil
}

// Close 关闭审计文件。
func (j *JSONL) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.f.Close()
}

// appendLocked 追加事件并 fsync（调用方持锁）。
func (j *JSONL) appendLocked(evs []Event) error {
	for _, e := range evs {
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if _, err := j.f.Write(append(b, '\n')); err != nil {
			return err
		}
	}
	return j.f.Sync()
}

// FindOpenIntents 扫描审计文件，返回未闭环（有 intent 无 result）的条目。
// 截断的尾行（崩溃残留）跳过。
func FindOpenIntents(path string) ([]MoveIntent, error) {
	open := map[string]MoveIntent{}
	if err := scanOpenIntents(path, open); err != nil {
		return nil, err
	}
	out := make([]MoveIntent, 0, len(open))
	for _, v := range open {
		out = append(out, v)
	}
	return out, nil
}

// scanOpenIntents 扫描单个审计文件，把 intent/result 差集累积进 open
// （跨文件复用：intent 与 result 可能落在不同分片）。
func scanOpenIntents(path string, open map[string]MoveIntent) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue // 截断尾行
		}
		key := e.RunID + "|" + e.Src
		switch e.Type {
		case EventIntent:
			open[key] = MoveIntent{RunID: e.RunID, PolicyID: e.PolicyID, Src: e.Src, Trash: e.Trash}
		case EventResult:
			delete(open, key)
		}
	}
	return sc.Err()
}

func intentsToEvents(intents []MoveIntent, typ string) []Event {
	evs := make([]Event, len(intents))
	for i, it := range intents {
		evs[i] = Event{
			TS: nowUnixNano(), RunID: it.RunID, PolicyID: it.PolicyID,
			Type: typ, Src: it.Src, Trash: it.Trash,
		}
	}
	return evs
}
