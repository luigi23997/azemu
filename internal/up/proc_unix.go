//go:build !windows

package up

import (
	"os/exec"
	"syscall"
)

// command runs in its own process group, so Ctrl-C reaches only azemu, which then stops the group.
func command(shell string, argv []string) *exec.Cmd {
	if shell != "" {
		argv = []string{"sh", "-c", shell}
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

func terminate(cmd *exec.Cmd, force bool) {
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	syscall.Kill(-cmd.Process.Pid, sig)
}
