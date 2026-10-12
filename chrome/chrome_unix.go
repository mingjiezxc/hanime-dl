//go:build !windows

package chrome

import (
	"errors"
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
	if err != nil {
		log.Printf("Failed to get pgid: %v, falling back to Process.Kill", err)
		killProcess(cmd)
		return
	}

	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
		log.Printf("Sending SIGTERM to process group %d failed: %v, falling back to Process.Kill", pgid, err)
		killProcess(cmd)
		return
	}

	time.Sleep(1 * time.Second)

	// 进程组通常已随 SIGTERM 全部退出，此时 SIGKILL 返回 ESRCH 属预期结果，
	// 不算失败，不打日志以免每次停机都产生噪音。
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		log.Printf("Sending SIGKILL to process group %d failed: %v", pgid, err)
	}
}

// killProcess 终止单个进程（拿不到进程组时的回退路径）
func killProcess(cmd *exec.Cmd) {
	if err := cmd.Process.Kill(); err != nil {
		log.Printf("Failed to kill Chrome process %d: %v", cmd.Process.Pid, err)
	}
}
