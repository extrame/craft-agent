package services

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveTransition(t *testing.T) {
	cases := []struct {
		name       string
		anchorPath string
		curPath    string
		want       string
	}{
		{"首次保存：锚点无身份，当前有了", "", "/tmp/a.deck", "/tmp/a.deck"},
		{"两边都未保存", "", "", ""},
		{"已保存且一致", "/tmp/a.deck", "/tmp/a.deck", ""},
		{"锚点有身份而当前不同：归守卫，轮询不抢", "/tmp/a.deck", "/tmp/b.deck", ""},
		{"当前反而没有 path：归守卫", "/tmp/a.deck", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := saveTransition(tc.anchorPath, tc.curPath); got != tc.want {
				t.Fatalf("saveTransition(%q, %q) = %q, want %q", tc.anchorPath, tc.curPath, got, tc.want)
			}
		})
	}
}

func TestUpgradeAnchor(t *testing.T) {
	s, dir := newRuntimeService(t)

	anchoredAt := time.Date(2026, 10, 27, 9, 30, 0, 0, time.UTC)
	other, _ := json.Marshal(map[string]any{
		"title": "遗留文档", "path": "/tmp/legacy.deck",
		"slides": 3, "dirty": true, "anchoredAt": anchoredAt,
	})
	target, _ := json.Marshal(map[string]any{
		"title": "未命名", "path": "",
		"slides": 2, "dirty": true, "anchoredAt": anchoredAt,
	})
	raw, _ := json.MarshalIndent(map[string]json.RawMessage{
		"deckcraft": target,
		"goneapp":   other,
	}, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "anchors.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	info := fileInfo{Title: "季度汇报", Path: "/tmp/quarterly.deck", Slides: 5}
	if !s.upgradeAnchor("deckcraft", info) {
		t.Fatal("第一次升级应该成功")
	}

	// 读回：目标条目升级、anchoredAt 保留；无关条目原样。
	var file map[string]json.RawMessage
	b, err := os.ReadFile(filepath.Join(dir, "anchors.json"))
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(b, &file) != nil || len(file) != 2 {
		t.Fatalf("锚点文件应仍有 2 个条目: %s", b)
	}
	var entry struct {
		Title      string    `json:"title"`
		Path       string    `json:"path"`
		Slides     int       `json:"slides"`
		Dirty      bool      `json:"dirty"`
		AnchoredAt time.Time `json:"anchoredAt"`
	}
	if json.Unmarshal(file["deckcraft"], &entry) != nil {
		t.Fatal("目标条目应可解析")
	}
	if entry.Title != "季度汇报" || entry.Path != "/tmp/quarterly.deck" || entry.Slides != 5 || entry.Dirty {
		t.Fatalf("目标条目升级不完整: %+v", entry)
	}
	if !entry.AnchoredAt.Equal(anchoredAt) {
		t.Fatalf("anchoredAt 被改动了: %v", entry.AnchoredAt)
	}
	var legacy struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(file["goneapp"], &legacy) != nil || legacy.Path != "/tmp/legacy.deck" {
		t.Fatalf("无关条目被改动: %s", file["goneapp"])
	}

	// 第二次：已有文件身份，不再覆盖。
	if s.upgradeAnchor("deckcraft", fileInfo{Title: "x", Path: "/tmp/other.deck"}) {
		t.Fatal("已有文件身份时不应再升级")
	}

	// 锚点不在：安静返回 false。
	if s.upgradeAnchor("nope", info) {
		t.Fatal("不存在的锚点不应升级")
	}
}

func TestUpgradeAnchorNoFile(t *testing.T) {
	s, _ := newRuntimeService(t)
	if s.upgradeAnchor("deckcraft", fileInfo{Path: "/tmp/a.deck"}) {
		t.Fatal("没有锚点文件时不应升级")
	}
}
