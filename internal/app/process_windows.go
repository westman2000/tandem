//go:build windows

package app

import (
	"os"
	"os/exec"
	"strconv"
)

func isolateProcess(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		_ = exec.Command("taskkill", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F").Run()
		return cmd.Process.Kill()
	}
}
