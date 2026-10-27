package proxy

import (
	"net/http"
	"net/url"
	"testing"
)

const scutilSample = `<dictionary> {
  ExceptionsList : <array> {
    0 : *.local
    1 : 169.254/16
  }
  FTPPassive : 1
  HTTPEnable : 0
  HTTPSEnable : 1
  HTTPSPort : 7890
  HTTPSProxy : 127.0.0.1
  SOCKSEnable : 0
}
`

func TestParseScutil(t *testing.T) {
	r, ok := parseScutil(scutilSample)
	if !ok {
		t.Fatal("应当读出一个代理")
	}
	if r.Host != "127.0.0.1" || r.Port != "7890" {
		t.Fatalf("地址不对：%s:%s", r.Host, r.Port)
	}
	if len(r.Bypass) != 2 || r.Bypass[0] != "*.local" {
		t.Fatalf("例外表不对：%v", r.Bypass)
	}
	if r.URL().String() != "http://127.0.0.1:7890" {
		t.Fatalf("URL 不对：%s", r.URL())
	}
}

func TestParseScutilFallbacks(t *testing.T) {
	// HTTPS 没开、HTTP 开着：用 HTTP 那条。
	httpOnly := `<dictionary> {
  HTTPEnable : 1
  HTTPProxy : proxy.corp
  HTTPPort : 3128
  HTTPSEnable : 0
}
`
	r, ok := parseScutil(httpOnly)
	if !ok || r.Host != "proxy.corp" || r.Port != "3128" {
		t.Fatalf("应当退回 HTTP 代理：%v %v", r, ok)
	}

	// 只有 SOCKS：不支持，直连。
	if _, ok := parseScutil("<dictionary> {\n  SOCKSEnable : 1\n  SOCKSProxy : 127.0.0.1\n  SOCKSPort : 1080\n}\n"); ok {
		t.Fatal("只配了 SOCKS 时应当当作没有代理")
	}

	// 地址里带凭据/斜杠：不能用。
	if _, ok := parseScutil("<dictionary> {\n  HTTPSEnable : 1\n  HTTPSProxy : user:pw@x\n}\n"); ok {
		t.Fatal("带凭据的地址应当被拒")
	}

	if _, ok := parseScutil(""); ok {
		t.Fatal("空输出应当当作没有代理")
	}
}

func TestParseWindows(t *testing.T) {
	r, ok := parseWindows("127.0.0.1:7890", "*.local;10.*;<local>")
	if !ok || r.Host != "127.0.0.1" || r.Port != "7890" {
		t.Fatalf("单个地址解析不对：%v %v", r, ok)
	}
	if len(r.Bypass) != 2 || r.Bypass[0] != "*.local" || r.Bypass[1] != "10.*" {
		t.Fatalf("例外表不对：%v", r.Bypass)
	}

	// 分协议列表：优先 HTTPS。
	r, ok = parseWindows("http=h1:80;https=h2:443;socks=h4:1080", "")
	if !ok || r.Host != "h2" || r.Port != "443" {
		t.Fatalf("应当取 https 那条：%v %v", r, ok)
	}

	for _, bad := range []string{"socks=h4:1080", "socks5://h4:1080", "user:pw@proxy:8080", "proxy:notaport", ""} {
		if _, ok := parseWindows(bad, ""); ok {
			t.Fatalf("%q 应当被拒", bad)
		}
	}
}

func TestSkipped(t *testing.T) {
	patterns := []string{"*.local", "10.*", "example.com", ".corp", "host:8080", "<local>", ""}
	cases := map[string]bool{
		"nas.local":                     true,
		"10.1.2.3":                      true,
		"example.com":                   true,
		"api.example.com":               true,
		"notexample.com":                false,
		"db.corp":                       true,
		"host":                          true,
		"github.com":                    false,
		"objects.githubusercontent.com": false,
	}
	for host, want := range cases {
		if got := skipped(host, patterns); got != want {
			t.Fatalf("%s：得到 %v，期望 %v", host, got, want)
		}
	}
	if !skipped("anything", []string{"*"}) {
		t.Fatal("* 应当全放行")
	}
}

func TestRouteFuncBypassesLoopbackAndExceptions(t *testing.T) {
	f := routeFunc(Route{Host: "127.0.0.1", Port: "7890", Bypass: []string{"*.local"}})
	for _, tc := range []struct {
		raw  string
		want bool // 是否走代理
	}{
		{"https://github.com/x", true},
		{"http://localhost:1/", false},
		{"http://127.0.0.1:8787/", false},
		{"https://nas.local/", false},
	} {
		u, err := url.Parse(tc.raw)
		if err != nil {
			t.Fatal(err)
		}
		got, err := f(&http.Request{URL: u})
		if err != nil {
			t.Fatal(err)
		}
		if (got != nil) != tc.want {
			t.Fatalf("%s：得到 %v，期望走代理=%v", tc.raw, got, tc.want)
		}
		if got != nil && got.Host != "127.0.0.1:7890" {
			t.Fatalf("%s：代理地址不对 %s", tc.raw, got)
		}
	}
}

func TestPickPrefersEnvironment(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://user:pw@127.0.0.1:7890")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("ALL_PROXY", "")

	_, note := Pick()
	// 说明里要标出「来自环境变量」，且不能把口令写进去。
	if !contains(note, "环境变量") {
		t.Fatalf("说明应当指出代理来自环境变量：%q", note)
	}
	if contains(note, "pw") {
		t.Fatalf("说明里不该出现口令：%q", note)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
