package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bcyup/filecleaner/internal/audit"
	"github.com/bcyup/filecleaner/internal/config"
)

// runAuditRebuild 实现 `filecleaner audit rebuild [--check-open]`：
//   - 默认：删旧投影 → 从 audit.jsonl 全量重建 SQLite，报告导入条数；
//   - --check-open：列出未闭环 intent（有 intent 无 result 的悬案，
//     供崩溃后人工核对/闭环），不重建。
func runAuditRebuild(args []string) int {
	fs := flag.NewFlagSet("audit rebuild", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfgPath := fs.String("config", "config.yaml", "config file")
	checkOpen := fs.Bool("check-open", false, "list open (unclosed) intents instead of rebuilding")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "audit rebuild: %v\n", err)
		return 1
	}

	if *checkOpen {
		open, err := audit.FindOpenIntentsAll(cfg.StateDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "audit rebuild: %v\n", err)
			return 1
		}
		if len(open) == 0 {
			fmt.Println("no open intents")
			return 0
		}
		for _, it := range open {
			out, _ := json.Marshal(it)
			fmt.Println(string(out))
		}
		fmt.Fprintf(os.Stderr, "audit rebuild: %d open intent(s) — verify actual file location (src or trash), then append a result event to close\n", len(open))
		return 0
	}

	// 重建：旧投影移除后全量导入（幂等：事件唯一键防重；多分片聚合）
	for _, suffix := range []string{"audit.db", "audit.db-wal", "audit.db-shm"} {
		os.Remove(filepath.Join(cfg.StateDir, suffix))
	}
	st, err := audit.OpenStore(filepath.Join(cfg.StateDir, "audit.db"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "audit rebuild: %v\n", err)
		return 1
	}
	defer st.Close()
	files, err := audit.AuditFiles(cfg.StateDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "audit rebuild: %v\n", err)
		return 1
	}
	var total int64
	for _, f := range files {
		n, err := st.ImportJSONL(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "audit rebuild: import %s: %v\n", f, err)
			return 1
		}
		total += n
	}
	fmt.Printf("audit rebuild: rebuilt %d event(s) from %d audit file(s)\n", total, len(files))
	return 0
}
