//go:build windows

package services

import "os"

// pidAlive 在 Windows 上没有 signal 0：FindProcess 成功就认为活着。
// 偶发误报的代价是界面多显示一条「运行中」，比误报「已退出」安全。
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.FindProcess(pid)
	return err == nil
}
