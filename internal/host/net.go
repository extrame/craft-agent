package host

import (
	"fmt"
	"net"
	"strconv"
)

// portFree 判断本机回环地址上的端口能否被绑定。
// 比「探测端口是否被监听」更可靠：抢到再放开，顺带覆盖了只绑 0.0.0.0 的情况。
func portFree(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

func splitHostPort(addr string) (string, string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", "", err
	}
	if _, err := strconv.Atoi(port); err != nil {
		return "", "", err
	}
	return host, port, nil
}
