package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bcyup/filecleaner/internal/nfs"
)

// runBenchScan 实现 `filecleaner bench scan`：只读扫描基准。
// statWorkers 形如 "1,4,8,16,32,64"；--stat=false 时为纯 readdir 模式。
func runBenchScan(args []string) int {
	fs := flag.NewFlagSet("bench scan", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	root := fs.String("root", "", "root directory to scan (required)")
	workers := fs.String("stat-workers", "1,4,8,16,32,64", "comma-separated stat worker counts")
	stat := fs.Bool("stat", true, "fstatat per file (false = readdir-only)")
	dirWorkers := fs.Int("dir-workers", 8, "directory read workers")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *root == "" {
		fmt.Fprintln(os.Stderr, "bench scan: --root is required")
		return 2
	}

	var counts []int
	for _, part := range strings.Split(*workers, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n <= 0 {
			fmt.Fprintf(os.Stderr, "bench scan: invalid stat-workers entry %q\n", part)
			return 2
		}
		counts = append(counts, n)
	}
	if len(counts) == 0 {
		fmt.Fprintln(os.Stderr, "bench scan: no valid stat-workers")
		return 2
	}

	type runJSON struct {
		Root        string  `json:"root"`
		StatWorkers int     `json:"stat_workers"`
		DirWorkers  int     `json:"dir_workers"`
		Stat        bool    `json:"stat"`
		Files       int64   `json:"files"`
		Dirs        int64   `json:"dirs"`
		Errors      int64   `json:"errors"`
		ElapsedMS   int64   `json:"elapsed_ms"`
		FilesPerSec float64 `json:"files_per_sec"`
		StatP50MS   float64 `json:"stat_p50_ms"`
		StatP95MS   float64 `json:"stat_p95_ms"`
		StatP99MS   float64 `json:"stat_p99_ms"`
	}

	for _, n := range counts {
		start := time.Now()
		res, err := nfs.ScanTree(context.Background(), *root, nfs.ScanOptions{
			DirWorkers:  *dirWorkers,
			StatWorkers: n,
			Stat:        *stat,
			Exclude: map[string]bool{
				".filecleaner-trash": true,
			},
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "bench scan: %v\n", err)
			return 1
		}
		elapsed := time.Since(start)
		fps := 0.0
		if elapsed > 0 {
			fps = float64(res.Files) / elapsed.Seconds()
		}
		out, _ := json.Marshal(runJSON{
			Root: *root, StatWorkers: n, DirWorkers: *dirWorkers, Stat: *stat,
			Files: res.Files, Dirs: res.Dirs, Errors: res.Errors,
			ElapsedMS: elapsed.Milliseconds(), FilesPerSec: fps,
			StatP50MS: res.StatP50, StatP95MS: res.StatP95, StatP99MS: res.StatP99,
		})
		fmt.Println(string(out))
	}
	return 0
}
