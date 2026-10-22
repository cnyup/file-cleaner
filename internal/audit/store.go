package audit

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"time"

	_ "modernc.org/sqlite"
)

// Store 是 JSONL 事件流的 SQLite 查询投影（可重建，不是事实源）。
// WAL 模式；单写（Import）多读（Query）。
type Store struct {
	db *sql.DB
}

// OpenStore 打开/创建投影库并确保 schema。
func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS events (
  id INTEGER PRIMARY KEY,
  ts INTEGER NOT NULL,
  run_id TEXT NOT NULL,
  policy_id TEXT NOT NULL,
  type TEXT NOT NULL,
  src TEXT NOT NULL DEFAULT '',
  trash TEXT NOT NULL DEFAULT '',
  ok INTEGER NOT NULL DEFAULT 0,
  err TEXT NOT NULL DEFAULT '',
  extra TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_event ON events(ts, run_id, type, src);
CREATE INDEX IF NOT EXISTS ix_state ON events(policy_id, src);
`)
	return err
}

// ImportJSONL 全量导入 JSONL（幂等：事件唯一键冲突即跳过）。
// 返回导入条数。截断尾行跳过不报错。
func (s *Store) ImportJSONL(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO events(ts,run_id,policy_id,type,src,trash,ok,err,extra) VALUES(?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	var n int64
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		extra, _ := json.Marshal(e.Extra)
		// 历史 JSONL 中 result 事件必含显式 ok（新写入契约）；缺 ok 的
		// 旧事件（omitempty 时代成功条目）按 false 处理——投影只信显式
		// 成功，宁可漏记不可误记（QueryMoved/MovedAt 语义不变）。
		if res, err := stmt.Exec(e.TS, e.RunID, e.PolicyID, e.Type, e.Src, e.Trash, boolInt(e.OK != nil && *e.OK), e.Err, string(extra)); err == nil {
			if ra, _ := res.RowsAffected(); ra > 0 {
				n++
			}
		}
	}
	if err := sc.Err(); err != nil {
		return n, fmt.Errorf("scan %s: %w", path, err)
	}
	return n, tx.Commit()
}

// MovedRow 是 moved 状态条目。
type MovedRow struct {
	RunID string
	Src   string
	Trash string
}

// QueryMoved 查询某策略当前处于 moved 状态（最新事件为 result+ok）的条目。
func (s *Store) QueryMoved(policyID string) ([]MovedRow, error) {
	rows, err := s.db.Query(`
SELECT run_id, src, trash FROM events e
WHERE e.id = (SELECT MAX(id) FROM events x WHERE x.src = e.src AND x.policy_id = e.policy_id AND x.type IN ('result','restore_result','purge_result'))
  AND e.policy_id = ? AND e.type = 'result' AND e.ok = 1
`, policyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MovedRow
	for rows.Next() {
		var r MovedRow
		if err := rows.Scan(&r.RunID, &r.Src, &r.Trash); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// FileState 返回条目的最终状态：moved/restored/purged/""（未知）。
// 锚定键：回收站路径（trash）——它是回收生命周期的唯一标识；
// 传入 srcPath 时自动换算为对应的 trash 路径再查询。
func (s *Store) FileState(trashPath string) (string, error) {
	var typ string
	var ok int
	err := s.db.QueryRow(`
SELECT type, ok FROM events e
WHERE e.trash = ?1 AND e.type IN ('result','restore_result','purge_result')
ORDER BY id DESC LIMIT 1
`, trashPath).Scan(&typ, &ok)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if ok != 1 {
		return "error", nil
	}
	switch typ {
	case "result":
		return "moved", nil
	case "restore_result":
		return "restored", nil
	case "purge_result":
		return "purged", nil
	}
	return "", nil
}

// OpenIntents 投影中的未闭环 intent（供启动核对）。
func (s *Store) OpenIntents() ([]MoveIntent, error) {
	rows, err := s.db.Query(`
SELECT run_id, policy_id,
       COALESCE(NULLIF(src,''), trash), trash FROM events e
WHERE e.type IN ('intent','restore_intent','purge_intent')
  AND NOT EXISTS (SELECT 1 FROM events r WHERE r.src = e.src AND r.trash = e.trash AND r.run_id = e.run_id AND r.type IN ('result','restore_result','purge_result'))
`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MoveIntent
	for rows.Next() {
		var m MoveIntent
		if err := rows.Scan(&m.RunID, &m.PolicyID, &m.Src, &m.Trash); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MovedAt 返回 trash 路径对应条目的入站时间（最新 result 事件 ts）。
// ok=false 表示投影无记录。
func (s *Store) MovedAt(trashPath string) (time.Time, bool, error) {
	var ts int64
	err := s.db.QueryRow(`
SELECT ts FROM events e
WHERE e.trash = ?1 AND e.type = 'result' AND e.ok = 1
ORDER BY id DESC LIMIT 1
`, trashPath).Scan(&ts)
	if err == sql.ErrNoRows {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return time.Unix(0, ts), true, nil
}

// Close 关闭投影。
func (s *Store) Close() error { return s.db.Close() }

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
