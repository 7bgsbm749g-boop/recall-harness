package acp

import (
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

func configureCommand(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return exec.Command("taskkill.exe", "/PID", fmt.Sprint(command.Process.Pid), "/T", "/F").Run()
	}
	command.WaitDelay = 2 * time.Second
}
