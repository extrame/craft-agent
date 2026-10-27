//go:build darwin

package proxy

import (
	"context"
	"os/exec"
	"time"
)

// system 读 macOS 的网络代理设置。
//
// scutil 是系统自带的，输出稳定；也可以在 Go 里直接调 SystemConfiguration 的 C API，
// 但那要开 cgo，而这里每次安装只读一次，起一个进程更省事。
func system() (Route, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/scutil", "--proxy").Output()
	if err != nil {
		return Route{}, false
	}
	return parseScutil(string(out))
}
