package mcpserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name   string
		anchor docIdentity
		cur    docIdentity
		want   relation
	}{
		{
			name:   "同一份未保存文档",
			anchor: docIdentity{Title: "未命名-1"},
			cur:    docIdentity{Title: "未命名-1"},
			want:   relSame,
		},
		{
			name:   "同一份已保存文档",
			anchor: docIdentity{Title: "a", Path: "/tmp/a.deck"},
			cur:    docIdentity{Title: "a", Path: "/tmp/a.deck"},
			want:   relSame,
		},
		{
			name:   "首次保存：title 变成文件名",
			anchor: docIdentity{Title: "未命名-1"},
			cur:    docIdentity{Title: "a", Path: "/tmp/a.deck"},
			want:   relSaved,
		},
		{
			name:   "首次保存：title 未变",
			anchor: docIdentity{Title: "a"},
			cur:    docIdentity{Title: "a", Path: "/tmp/a.deck"},
			want:   relSaved,
		},
		{
			name:   "路径变了（另存为 / 切走 / 打开别的文件）：锚点跟随当前文档",
			anchor: docIdentity{Title: "a", Path: "/tmp/a.deck"},
			cur:    docIdentity{Title: "b", Path: "/tmp/b.deck"},
			want:   relSwitched,
		},
		{
			name:   "文件被关掉、开了个未保存的新文档：跟随",
			anchor: docIdentity{Title: "a", Path: "/tmp/a.deck"},
			cur:    docIdentity{Title: "未命名-1"},
			want:   relSwitched,
		},
		{
			name:   "未保存文档之间切换",
			anchor: docIdentity{Title: "未命名-1"},
			cur:    docIdentity{Title: "未命名-2"},
			want:   relSwitched,
		},
	}
	for _, tc := range cases {
		if got := classify(tc.anchor, tc.cur); got != tc.want {
			t.Errorf("%s: classify(%+v, %+v) = %q, want %q", tc.name, tc.anchor, tc.cur, got, tc.want)
		}
	}
}

func TestAnchorsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	doc := docIdentity{Title: "季度汇报", Path: "/Users/x/Docs/季度汇报.deck", Slides: 12}

	s := anchorStore{dataDir: dir}
	s.put("deckcraft", doc)

	// 新的 store（模拟下一个会话）从盘上读回同一份。
	next := anchorStore{dataDir: dir}
	got, ok := next.get("deckcraft")
	if !ok {
		t.Fatal("落盘的锚点没有被读回")
	}
	if got.Title != doc.Title || got.Path != doc.Path || got.Slides != doc.Slides {
		t.Fatalf("读回的身份不一致: %+v", got)
	}

	// drop 之后盘上也没有了。
	next.drop("deckcraft")
	back := anchorStore{dataDir: dir}
	if _, ok := back.get("deckcraft"); ok {
		t.Fatal("drop 之后锚点仍在盘上")
	}
}

func TestLoadAnchorsToleratesGarbage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "anchors.json")
	for _, bad := range []string{"", "not json", "null", `{"deckcraft": {"title": 3}}`} {
		if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := loadAnchors(dir); got == nil {
			t.Fatalf("loadAnchors(%q) 返回 nil，应该容忍坏文件", bad)
		}
	}
	_ = os.Remove(path)
	if got := loadAnchors(dir); got == nil {
		t.Fatal("文件不存在时也应返回空表，而不是 nil")
	}
}

func TestAnchorsFileIsPerApp(t *testing.T) {
	dir := t.TempDir()
	s := anchorStore{dataDir: dir}
	s.put("deckcraft", docIdentity{Title: "a", Path: "/tmp/a.deck"})
	s.put("figcraft", docIdentity{Title: "b"})

	if _, ok := s.get("deckcraft"); !ok {
		t.Fatal("deckcraft 锚点丢失")
	}
	s.drop("figcraft")
	if _, ok := s.get("figcraft"); ok {
		t.Fatal("figcraft 应已被解除")
	}
	if _, ok := s.get("deckcraft"); !ok {
		t.Fatal("drop 一个应用不应影响另一个")
	}

	raw, _ := os.ReadFile(filepath.Join(dir, "anchors.json"))
	var file anchorsFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("anchors.json 不是合法 JSON: %v", err)
	}
	if len(file) != 1 {
		t.Fatalf("盘上应只剩 1 个锚点，实际 %d", len(file))
	}
	if file["deckcraft"].AnchoredAt.IsZero() {
		t.Fatal("落盘的锚点应带时间")
	}
	_ = time.Now()
}
