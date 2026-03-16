//go:build unix

package runtime

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func configureLocalProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killLocalProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}

	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) && err != syscall.ESRCH {
		return err
	}
	return nil
}
