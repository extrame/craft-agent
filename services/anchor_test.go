package services

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"craft-agent/internal/host"
	"craft-agent/internal/journal"
)

// newAnchorService 起一个指向临时目录的绑定层：不碰真实进程，
// 只用它的 DataDir 与 journal。测试关注的是 anchors.json 的读写口径。
func newAnchorService(t *testing.T) *AppService {
	t.Helper()
	dir := t.TempDir()
	h, err := host.New(host.Options{DataDir: dir})
	if err != nil {
		t.Fatalf("host.New: %v", err)
	}
	t.Cleanup(func() { h.Close() })
	j, err := journal.Open(dir)
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	s := NewAppService(h, j)
	s.rel = nil // 测试不查版本；Anchors/ReleaseAnchor 也不该碰它
	return s
}

func writeAnchors(t *testing.T, dir string, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "anchors.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAnchorsReadsAndOrders(t *testing.T) {
	dir := t.TempDir()
	h, err := host.New(host.Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	j, err := journal.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := NewAppService(h, j)

	now := time.Now().Format(time.RFC3339)
	// 一个套件内（deckcraft）+ 一个套件外的遗留（goneapp 是编造的 id，走遗留桶）。
	writeAnchors(t, dir, `{
	  "goneapp": {"title":"遗留","path":"/tmp/old.docx","anchoredAt":"`+now+`"},
	  "deckcraft": {"title":"季度汇报","path":"/tmp/汇报.deck","anchoredAt":"`+now+`"}
	}`)

	got := s.Anchors()
	if len(got) != 2 {
		t.Fatalf("应读到 2 条锚点，实际 %d", len(got))
	}
	// 套件内的应用按 Suite 顺序在前，遗留（goneapp 是编造的 id，走遗留桶）在后。
	// 这里只断言相对顺序与内容，不假设 Suite 里谁排第一。
	deckIdx := -1
	for i, a := range got {
		if a.App == "deckcraft" {
			deckIdx = i
		}
	}
	if deckIdx == -1 {
		t.Fatal("deckcraft 锚点丢失")
	}
	if got[len(got)-1].App != "goneapp" {
		t.Fatalf("套件外的遗留应排在最后: %+v", got)
	}
	d := got[deckIdx]
	if d.AppName != "DeckCraft" || d.Title != "季度汇报" || d.Path != "/tmp/汇报.deck" {
		t.Fatalf("字段读错: %+v", d)
	}
	if !d.Saved {
		t.Fatal("带 path 的锚点应标记 Saved")
	}
	// /tmp/汇报.deck 不存在 → FileExists=false。
	if d.FileExists {
		t.Fatal("不存在的文件不应标记 FileExists")
	}
	// 遗留桶里查不到显示名，AppName 保持原 id。
	for _, a := range got {
		if a.App == "goneapp" && a.AppName != "goneapp" {
			t.Fatalf("套件外的应用 AppName 应保持原 id，实际 %q", a.AppName)
		}
	}

	// 损坏文件当「没有」。
	writeAnchors(t, dir, "not json")
	if got := s.Anchors(); got != nil {
		t.Fatalf("坏文件应返回 nil，实际 %v", got)
	}
}

func TestAnchorsMissingFile(t *testing.T) {
	s := newAnchorService(t)
	if got := s.Anchors(); got != nil {
		t.Fatalf("没有锚点文件应返回 nil，实际 %v", got)
	}
}

func TestReleaseAnchorRemovesOnlyOne(t *testing.T) {
	dir := t.TempDir()
	h, err := host.New(host.Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	j, err := journal.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := NewAppService(h, j)

	writeAnchors(t, dir, `{
	  "deckcraft": {"title":"a","path":"/tmp/a.deck","anchoredAt":"2026-10-27T10:00:00Z"},
	  "figcraft": {"title":"b","path":"","anchoredAt":"2026-10-27T10:00:00Z"}
	}`)

	if err := s.ReleaseAnchor("deckcraft"); err != nil {
		t.Fatalf("ReleaseAnchor: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "anchors.json"))
	if err != nil {
		t.Fatal("解除后文件应仍在（figcraft 还有锚点）")
	}
	var left map[string]json.RawMessage
	if err := json.Unmarshal(raw, &left); err != nil {
		t.Fatal(err)
	}
	if _, ok := left["deckcraft"]; ok {
		t.Fatal("deckcraft 锚点应已被解除")
	}
	if _, ok := left["figcraft"]; !ok {
		t.Fatal("figcraft 锚点不应被连带解除")
	}

	// 最后一个也解除后，文件整个删掉——不留空壳。
	if err := s.ReleaseAnchor("figcraft"); err != nil {
		t.Fatalf("ReleaseAnchor(figcraft): %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "anchors.json")); !os.IsNotExist(err) {
		t.Fatal("全部解除后 anchors.json 应被删除")
	}

	// 解除不存在的锚点是幂等的。
	if err := s.ReleaseAnchor("figcraft"); err != nil {
		t.Fatalf("重复解除应幂等: %v", err)
	}
}
