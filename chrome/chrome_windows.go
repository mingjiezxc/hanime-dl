//go:build windows

package chrome

import "os/exec"

// setProcessGroupAttr Windows 下无需设置进程组属性
func setProcessGroupAttr(cmd *exec.Cmd) {
	// Windows 使用默认进程管理
}

// cleanupProcess 终止 Windows 进程
func cleanupProcess(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		cmd.Process.Kill()
	}
}
