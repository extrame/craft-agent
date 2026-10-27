//go:build !darwin && !windows

package proxy

// system 在 Linux 与 BSD 上不读系统设置：桌面环境的代理最终也是落进环境变量，
// 而环境变量在 Pick 里已经先看过了。
func system() (Route, bool) {
	return Route{}, false
}
