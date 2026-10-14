//go:build !windows && !js

package host

import (
	"os/exec"
	"syscall"
)

func newProcessGroup(shell string, argv []string) *exec.Cmd {
	if shell != "" {
		argv = []string{"sh", "-c", shell}
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

func terminateProc(cmd *exec.Cmd, force bool) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	syscall.Kill(-cmd.Process.Pid, sig)
}
