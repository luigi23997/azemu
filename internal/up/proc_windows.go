package up

import (
	"os/exec"
	"strconv"
	"syscall"
)

// command runs in its own process group, so Ctrl-C reaches only azemu, which then stops the tree.
// A shell command line goes to cmd.exe verbatim: Go's argument quoting is not cmd.exe's.
func command(shell string, argv []string) *exec.Cmd {
	attr := &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	if shell != "" {
		argv = []string{"cmd.exe"}
		attr.CmdLine = `cmd.exe /d /s /c "` + shell + `"`
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = attr
	return cmd
}

// terminate kills the whole tree: func.exe -> the Functions host -> the language worker.
// ponytail: always forced; console processes ignore taskkill's polite WM_CLOSE.
func terminate(cmd *exec.Cmd, force bool) {
	exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
}
