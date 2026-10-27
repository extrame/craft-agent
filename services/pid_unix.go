//go:build !windows

package services

import "syscall"

// pidAlive 用 signal 0 探进程活性：不发送任何东西，只查在不在。
// 权限不足（EPERM）也算活着——进程存在，只是不是我们的。
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, syscall.Signal(0))
	return err == nil || err == syscall.EPERM
}
