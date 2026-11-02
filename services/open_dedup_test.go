package services

import (
	"testing"
)

func TestParseOpenParams(t *testing.T) {
	cases := []struct {
		name      string
		params    string
		wantPath  string
		wantForce bool
	}{
		{"正常路径", `{"path":"/tmp/a.dxf"}`, "/tmp/a.dxf", false},
		{"force 开", `{"path":"/tmp/a.dxf","force":true}`, "/tmp/a.dxf", true},
		{"newWindow 是 force 的别名", `{"path":"/tmp/a.dxf","newWindow":true}`, "/tmp/a.dxf", true},
		{"path 为空", `{"path":""}`, "", false},
		{"没有 path 字段", `{"ids":[1,2]}`, "", false},
		{"不是对象", `[1,2]`, "", false},
	}
	for _, c := range cases {
		got := parseOpenParams([]byte(c.params))
		if got.path != c.wantPath || got.force != c.wantForce {
			t.Errorf("%s: parseOpenParams(%s) = {%q %v}, want {%q %v}",
				c.name, c.params, got.path, got.force, c.wantPath, c.wantForce)
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
