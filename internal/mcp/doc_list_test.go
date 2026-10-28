package mcpserver

import (
	"encoding/json"
	"testing"
)

func TestParseDocList(t *testing.T) {
	// FigCraft 0.11.2 实测形状（2026-10-28）。
	raw := json.RawMessage(`[{"active":false,"dirty":false,"index":0,"name":"a","path":"/tmp/a.figcraft"},
		{"active":true,"dirty":true,"index":1,"name":"新版网站设计","path":"/Users/x/新版网站设计.figcraft"}]`)
	entries, active, err := parseDocList(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("应解析出 2 项，实际 %d", len(entries))
	}
	if !active.Active || active.Name != "新版网站设计" || active.Path != "/Users/x/新版网站设计.figcraft" || !active.Dirty {
		t.Fatalf("活动项取错: %+v", active)
	}

	// 没有 active 标记时取第一项。
	raw2 := json.RawMessage(`[{"index":0,"name":"b","path":"/tmp/b.figcraft"}]`)
	_, active2, err := parseDocList(raw2)
	if err != nil {
		t.Fatal(err)
	}
	if active2.Name != "b" {
		t.Fatalf("无 active 标记时应取第一项: %+v", active2)
	}

	// 空列表与坏形状都要报错，不能静默当成功。
	for _, bad := range []json.RawMessage{json.RawMessage(`[]`), json.RawMessage(`{"x":1}`)} {
		if _, _, err := parseDocList(bad); err == nil {
			t.Fatalf("坏返回 %s 应报错", bad)
		}
	}
}
