// Package proxy 决定这个进程出网走不走代理。
//
// 为什么不能只用 http.ProxyFromEnvironment：它读的是 HTTP_PROXY / HTTPS_PROXY 这类
// 环境变量，而 macOS 的代理设在 SystemConfiguration 里（系统设置 → 网络 → 代理），
// Windows 的设在注册表里，从 Finder / 开始菜单启动的图形程序两者都继承不到。
// 于是用户开着 ClashX 一类的系统代理时，我们依然直连 GitHub —— 在中国下载
// 上百兆的安装包，这是「几分钟」和「一直失败」的分界。
//
// 顺序照 craft-launcher：环境变量优先，其次系统设置，都没有就直连。PAC 与 WPAD
// 不支持 —— 那需要先下载脚本再解释执行，值不上一件事做一半。
package proxy

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Route 是出网要走的那条路，或者空（直连）。
type Route struct {
	// Host、Port 是代理地址。
	Host string
	Port string
	// Bypass 是不走代理的主机，形式同 NO_PROXY（example.com、*.example.com、10.*）。
	Bypass []string
	// Source 说明这条设置从哪来，只用于日志与界面。
	Source string
}

// URL 把设置变成代理地址；没有主机时返回 nil。
func (r Route) URL() *url.URL {
	if r.Host == "" {
		return nil
	}
	host := r.Host
	if r.Port != "" {
		host = net.JoinHostPort(r.Host, r.Port)
	}
	return &url.URL{Scheme: "http", Host: host}
}

// String 是给日志看的一句话，不含任何用户名密码。
func (r Route) String() string {
	if r.Host == "" {
		return "直连"
	}
	host := r.Host
	if r.Port != "" {
		host = net.JoinHostPort(r.Host, r.Port)
	}
	src := r.Source
	if src == "" {
		src = "系统代理"
	}
	return host + "（" + src + "）"
}

// Pick 选出这次进程要用的代理，以及一句说明。
//
// 返回 nil 表示直连 —— 这也是 http.Transport.Proxy 的合法取值。
func Pick() (func(*http.Request) (*url.URL, error), string) {
	if name, value := envProxy(); name != "" {
		return http.ProxyFromEnvironment, redact(value) + "（环境变量 " + name + "）"
	}
	sys, ok := system()
	if !ok {
		return nil, "直连（环境变量和系统设置里都没有代理）"
	}
	target := sys.URL()
	if target == nil {
		return nil, "直连（" + sys.Source + "里的代理地址用不了）"
	}
	return routeFunc(sys), sys.String()
}

// routeFunc 把一条代理设置变成 Transport 用的判定函数：本机和例外表里的主机直连。
func routeFunc(r Route) func(*http.Request) (*url.URL, error) {
	target := r.URL()
	return func(req *http.Request) (*url.URL, error) {
		host := req.URL.Hostname()
		// 本机地址永远直连：代理软件自己也在这台机器上，绕它一圈只会更慢。
		if isLoopback(host) || skipped(host, r.Bypass) {
			return nil, nil
		}
		return target, nil
	}
}

// envProxy 找出第一个设了值的代理环境变量。
func envProxy() (name, value string) {
	for _, key := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return key, v
		}
	}
	return "", ""
}

// redact 去掉 URL 里的用户名密码：说明要进日志，凭据不该跟着进去。
func redact(value string) string {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" {
		return value
	}
	return u.Redacted()
}

// isLoopback 判断主机名是不是本机。
func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// skipped 判断 host 是否落在 NO_PROXY 形式的例外表里。
//
// 支持 example.com（含子域）、*.example.com、.example.com、10.*、host:port、
// 以及 *（全部直连）。`<local>` 是 Windows 专有的「无点主机名」，没有对应写法，忽略。
func skipped(host string, patterns []string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, raw := range patterns {
		p := strings.ToLower(strings.TrimSpace(raw))
		if p == "" || p == "<local>" {
			continue
		}
		if p == "*" {
			return true
		}
		if h, _, err := net.SplitHostPort(p); err == nil {
			p = h
		}
		p = strings.TrimSuffix(p, ".")
		switch {
		case strings.HasPrefix(p, "*."):
			if host == p[2:] || strings.HasSuffix(host, p[1:]) {
				return true
			}
		case strings.HasSuffix(p, ".*"):
			if strings.HasPrefix(host, p[:len(p)-1]) {
				return true
			}
		case strings.HasPrefix(p, "."):
			if host == p[1:] || strings.HasSuffix(host, p) {
				return true
			}
		default:
			if host == p || strings.HasSuffix(host, "."+p) {
				return true
			}
		}
	}
	return false
}

// parseScutil 读 `scutil --proxy` 的输出。
//
// 形状大致是：
//
//	<dictionary> {
//	  ExceptionsList : <array> {
//	    0 : *.local
//	  }
//	  HTTPSEnable : 1
//	  HTTPSProxy : 127.0.0.1
//	  HTTPSPort : 7890
//	}
//
// HTTPS 优先（每个请求都是 https），没有再退到 HTTP；SOCKS 忽略。
func parseScutil(out string) (Route, bool) {
	values := map[string]string{}
	var bypass []string
	inArray := false
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if inArray {
			if strings.HasPrefix(line, "}") {
				inArray = false
				continue
			}
			if i := strings.Index(line, " : "); i >= 0 {
				if v := strings.TrimSpace(line[i+3:]); v != "" {
					bypass = append(bypass, v)
				}
			}
			continue
		}
		i := strings.Index(line, " : ")
		if i < 0 {
			continue
		}
		key, value := strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+3:])
		if key == "ExceptionsList" {
			inArray = true
			continue
		}
		values[key] = value
	}

	for _, kind := range []string{"HTTPS", "HTTP"} {
		if values[kind+"Enable"] != "1" {
			continue
		}
		host := values[kind+"Proxy"]
		// 带用户名、斜杠、空格的地址交给谁来用都不对，宁可不走代理。
		if host == "" || strings.ContainsAny(host, "@/ \t") {
			continue
		}
		return Route{Host: host, Port: values[kind+"Port"], Bypass: bypass, Source: "macOS 系统代理"}, true
	}
	return Route{}, false
}

// parseWindows 读 Windows 的 ProxyServer / ProxyOverride 两个值。
//
// ProxyServer 要么是 host:port（所有协议共用一个），要么是
// `http=h1:80;https=h2:443;socks=h4:1080` 这种分协议列表 —— 后者优先取 HTTPS。
// 只有 SOCKS 的写法不支持：Go 的 Transport 走不了 SOCKS 代理。
func parseWindows(server, override string) (Route, bool) {
	host, port, ok := windowsServer(server)
	if !ok {
		return Route{}, false
	}
	var bypass []string
	for _, field := range strings.FieldsFunc(override, func(r rune) bool { return r == ';' || r == ' ' }) {
		if v := strings.TrimSpace(field); v != "" && !strings.EqualFold(v, "<local>") {
			bypass = append(bypass, v)
		}
	}
	return Route{Host: host, Port: port, Bypass: bypass, Source: "Windows 代理设置"}, true
}

// windowsServer 从 ProxyServer 的值里取出主机与端口。
func windowsServer(value string) (host, port string, ok bool) {
	entries := strings.FieldsFunc(value, func(r rune) bool { return r == ';' || r == ' ' })
	if len(entries) == 0 {
		return "", "", false
	}
	chosen := ""
	hasScheme := false
	for _, e := range entries {
		if strings.Contains(e, "=") {
			hasScheme = true
		}
	}
	if hasScheme {
		for _, want := range []string{"https", "http"} {
			for _, e := range entries {
				k, v, found := strings.Cut(e, "=")
				if found && strings.EqualFold(strings.TrimSpace(k), want) {
					chosen = strings.TrimSpace(v)
					break
				}
			}
			if chosen != "" {
				break
			}
		}
		if chosen == "" {
			return "", "", false
		}
	} else {
		chosen = entries[0]
	}
	host, port, ok = splitHostPort(chosen)
	if !ok {
		return "", "", false
	}
	return host, port, validPort(port)
}

// validPort 只放行数字端口（空表示用默认端口）。
func validPort(port string) bool {
	if port == "" {
		return true
	}
	n, err := strconv.Atoi(port)
	return err == nil && n > 0 && n < 65536
}

// splitHostPort 接受 host:port、http://host:port/、[::1]:8080。
func splitHostPort(value string) (host, port string, ok bool) {
	rest := value
	if i := strings.Index(rest, "://"); i >= 0 {
		if !strings.EqualFold(rest[:i], "http") {
			return "", "", false
		}
		rest = rest[i+3:]
	}
	rest = strings.TrimSuffix(rest, "/")
	if rest == "" || strings.ContainsAny(rest, "@/?# \t") {
		return "", "", false
	}
	if strings.HasPrefix(rest, "[") {
		end := strings.Index(rest, "]")
		if end < 0 {
			return "", "", false
		}
		host = rest[:end+1]
		after := rest[end+1:]
		if after == "" {
			return host, "", true
		}
		if !strings.HasPrefix(after, ":") {
			return "", "", false
		}
		return host, after[1:], true
	}
	if h, p, found := strings.Cut(rest, ":"); found {
		if h == "" || strings.Contains(p, ":") {
			return "", "", false
		}
		return h, p, true
	}
	return rest, "", true
}
