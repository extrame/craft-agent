package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// 构建过的仓库：dist 里有 index.html，请求要原样交给文件服务器。
func TestPageHandlerPrefersBuiltFrontend(t *testing.T) {
	fsys := fstest.MapFS{
		"frontend/dist/index.html":    {Data: []byte("<title>real</title>")},
		"frontend/dist/assets/app.js": {Data: []byte("console.log(1)")},
	}
	built := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "built")
	})

	rec := httptest.NewRecorder()
	pageHandler(fsys, built).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Body.String() != "built" {
		t.Fatalf("带 index.html 时应走文件服务器，得到 %q", rec.Body.String())
	}
}

// fresh clone：只有 .keep，界面用说明页兜底，而不是一张 404。
func TestPageHandlerExplainsMissingFrontend(t *testing.T) {
	fsys := fstest.MapFS{"frontend/dist/.keep": {Data: nil}}
	built := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("没有 index.html 时不该调用文件服务器")
	})
	h := pageHandler(fsys, built)

	for _, path := range []string{"/", "/index.html"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s 应返回 200，得到 %d", path, rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "npm run build") || !strings.Contains(body, "not built") {
			t.Fatalf("%s 的说明页没写清楚怎么构建：%s", path, body)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("%s 的 Content-Type 是 %q", path, ct)
		}
	}

	// 静态资源以外的路径不该被说明页吞掉：否则前端框架的路由会拿到一张 HTML。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知资源应 404，得到 %d", rec.Code)
	}
}
