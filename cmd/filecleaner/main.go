// filecleaner 是 NFS 定时清理工具的入口。
// Wave 0：validate 骨架 + bench scan；其余子命令在后续波次接线。
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: filecleaner <subcommand> [flags]")
		fmt.Fprintln(os.Stderr, "subcommands: bench, validate")
		os.Exit(2)
	}
	var code int
	switch os.Args[1] {
	case "bench":
		if len(os.Args) < 3 || os.Args[2] != "scan" {
			fmt.Fprintln(os.Stderr, "usage: filecleaner bench scan --root <dir> [--stat-workers 1,4,8]")
			code = 2
			break
		}
		code = runBenchScan(os.Args[3:])
	case "validate":
		code = runValidate(os.Args[2:])
	case "once":
		code = runOnce(os.Args[2:])
	case "restore":
		code = runRestore(os.Args[2:])
	case "purge":
		code = runPurge(os.Args[2:])
	case "audit":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: filecleaner audit query|rebuild [flags]")
			code = 2
			break
		}
		switch os.Args[2] {
		case "query":
			code = runAuditQuery(os.Args[3:])
		case "rebuild":
			code = runAuditRebuild(os.Args[3:])
		default:
			fmt.Fprintf(os.Stderr, "filecleaner: unknown audit subcommand %q (query|rebuild)\n", os.Args[2])
			code = 2
		}
	case "daemon":
		code = runDaemon(os.Args[2:])
	case "sweep":
		code = runSweep(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "filecleaner: unknown subcommand %q\n", os.Args[1])
		code = 2
	}
	os.Exit(code)
}
