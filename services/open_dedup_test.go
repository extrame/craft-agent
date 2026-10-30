package services

import (
	"testing"
)

func TestOpenTarget(t *testing.T) {
	cases := []struct {
		name   string
		params string
		want   string
	}{
		{"正常路径", `{"path":"/tmp/a.dxf"}`, "/tmp/a.dxf"},
		{"path 为空", `{"path":""}`, ""},
		{"没有 path 字段", `{"ids":[1,2]}`, ""},
		{"不是对象", `[1,2]`, ""},
	}
	for _, c := range cases {
		got := openTarget([]byte(c.params))
		if got != c.want {
			t.Errorf("%s: openTarget(%s) = %q, want %q", c.name, c.params, got, c.want)
		}
	}
}

func TestSamePath(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{"完全一致", "/tmp/a.dxf", "/tmp/a.dxf", true},
		{"大小写不同（macOS 文件系统不区分）", "/tmp/A.dxf", "/tmp/a.dxf", true},
		{"多余斜杠", "/tmp//a.dxf", "/tmp/a.dxf", true},
		{"不同文件", "/tmp/a.dxf", "/tmp/b.dxf", false},
	}
	for _, c := range cases {
		if got := samePath(c.a, c.b); got != c.want {
			t.Errorf("%s: samePath(%q,%q) = %v, want %v", c.name, c.a, c.b, got, c.want)
		}
	}
}
