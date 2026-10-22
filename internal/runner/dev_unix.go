//go:build linux || darwin

package runner

import (
	"os"
	"syscall"
)

func init() {
	stat = func(path string, out *osSt) error {
		var st syscall.Stat_t
		if err := syscall.Stat(path, &st); err != nil {
			return err
		}
		out.dev = uint64(st.Dev)
		return nil
	}
	_ = os.Getpid
}
