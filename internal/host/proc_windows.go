//go:build windows && !js

package host

import (
	"os/exec"
	"strconv"
	"syscall"
)

func newProcessGroup(shell string, argv []string) *exec.Cmd {
	attr := &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	if shell != "" {
		argv = []string{"cmd.exe"}
		attr.CmdLine = `cmd.exe /d /s /c "` + shell + `"`
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = attr
	return cmd
}

func terminateProc(cmd *exec.Cmd, force bool) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
}
