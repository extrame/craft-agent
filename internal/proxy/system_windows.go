//go:build windows

package proxy

import "golang.org/x/sys/windows/registry"

// system 读 Windows「Internet 选项」里的代理设置。
//
// 这就是系统设置界面写进去的那一份，Clash、v2rayN 之类打开「系统代理」时改的也是它。
func system() (Route, bool) {
	key, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if err != nil {
		return Route{}, false
	}
	defer key.Close()

	enabled, _, err := key.GetIntegerValue("ProxyEnable")
	if err != nil || enabled == 0 {
		return Route{}, false
	}
	server, _, err := key.GetStringValue("ProxyServer")
	if err != nil {
		return Route{}, false
	}
	// 例外表缺失不影响使用，读不到就当没有。
	override, _, _ := key.GetStringValue("ProxyOverride")
	return parseWindows(server, override)
}
