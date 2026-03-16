//go:build !unix

package runtime

import "os/exec"

func configureLocalProcessGroup(cmd *exec.Cmd) {}

func killLocalProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
