//go:build !windows

package chrome

import (
	"log"
	"os/exec"
	"syscall"
	"time"
)

// setProcessGroupAttr 设置 Unix 进程组属性
func setProcessGroupAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}
}

// cleanupProcess 终止 Unix 进程组
func cleanupProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err == nil {
		syscall.Kill(-pgid, syscall.SIGTERM)
		time.Sleep(1 * time.Second)
		syscall.Kill(-pgid, syscall.SIGKILL)
	} else {
		log.Printf("Failed to get pgid: %v, falling back to Process.Kill", err)
		cmd.Process.Kill()
	}
}
