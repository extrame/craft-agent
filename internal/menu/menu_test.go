package menu

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"craft-agent/internal/control"
	"craft-agent/internal/craft"
)

func app(t *testing.T, id string) craft.App {
	t.Helper()
	a, ok := craft.ByID(id)
	if !ok {
		t.Fatalf("清单里没有 %s", id)
	}
	return a
}

// DeckCraft 的编码：数组路径，最多三层；还有大量命令完全不报菜单。
func TestBuildArrayPaths(t *testing.T) {
	cmds := []control.Command{
		{ID: "format.bold", Label: "Bold", Menu: control.MenuPath{"Format", "Text"}, Shortcut: "Cmd+B"},
		{ID: "format.italic", Label: "Italic", Menu: control.MenuPath{"Format", "Text"}},
		{ID: "format.align.left", Label: "Left", Menu: control.MenuPath{"Format", "Paragraph", "Align"}},
		{ID: "slide.new", Label: "New Slide", Menu: control.MenuPath{"Home"}},
		{ID: "file.templates", Label: "Templates"},
		{ID: "file.render", Label: "Render"},
		{ID: "edit.deselect", Label: "Deselect"},
	}
	m := Build(app(t, "deckcraft"), "0.11.1", cmds)

	if m.Commands != 7 || m.Mapped != 4 || m.Grouped != 3 {
		t.Fatalf("计数不对: commands=%d mapped=%d grouped=%d", m.Commands, m.Mapped, m.Grouped)
	}
	// 兜底是按 id 前缀分组的：file.* 一桶，edit.* 一桶。
	if len(m.Sections) != 4 {
		t.Fatalf("顶层应有 Format/Home + file/edit 两个兜底分组，得到 %d: %+v", len(m.Sections), names(m.Sections))
	}
	// 真实菜单在前，兜底分组在后。
	if m.Sections[0].Name != "Format" || m.Sections[1].Name != "Home" {
		t.Fatalf("真实菜单顺序不对: %v", names(m.Sections))
	}
	if !m.Sections[2].Grouped || m.Sections[2].Name != "file" {
		t.Fatalf("兜底分组应在最后且标记 grouped: %+v", m.Sections[2])
	}
	if m.Sections[2].Count != 2 {
		t.Fatalf("file 兜底分组应合并 file.templates 与 file.render，得到 %d", m.Sections[2].Count)
	}

	text, ok := m.Find("Format", "Text")
	if !ok {
		t.Fatal("找不到 Format › Text")
	}
	if text.Count != 2 || len(text.Items) != 2 {
		t.Fatalf("Format › Text 应有 2 条，得到 %d/%d", text.Count, len(text.Items))
	}
	if text.Path[0] != "Format" || text.Path[1] != "Text" {
		t.Fatalf("路径不对: %v", text.Path)
	}
	align, ok := m.Find("format", "paragraph", "align")
	if !ok || align.Count != 1 {
		t.Fatalf("大小写不敏感查找失败: %+v", align)
	}
}

// FigCraft 的编码：一层数组，但单段里用 / 折了层。
func TestBuildSlashInsideSegment(t *testing.T) {
	cmds := []control.Command{
		{ID: "text.alignLeft", Label: "Align Left", Menu: control.MenuPath{"Text/Alignment"}},
		{ID: "text.uppercase", Label: "Uppercase", Menu: control.MenuPath{"Text/Case"}},
		{ID: "object.group", Label: "Group", Menu: control.MenuPath{"Object/Boolean groups"}},
		{ID: "node.create", Label: "Create node"},
	}
	m := Build(app(t, "figcraft"), "0.11.2", cmds)

	text, ok := m.Find("Text")
	if !ok {
		t.Fatal("单段带 / 的菜单没有折成层级")
	}
	if len(text.Children) != 2 || text.Count != 2 {
		t.Fatalf("Text 下应有 2 个子菜单 / 2 条命令，得到 %d/%d", len(text.Children), text.Count)
	}
	if _, ok := m.Find("Text", "Case"); !ok {
		t.Fatal("Text › Case 丢失")
	}
	if _, ok := m.Find("Object", "Boolean groups"); !ok {
		t.Fatal("Object › Boolean groups 丢失")
	}
	if m.Grouped != 1 || m.Sections[len(m.Sections)-1].Name != "node" {
		t.Fatalf("无菜单的命令应按 id 前缀兜底，得到 %v", names(m.Sections))
	}
}

// PdfCraft 的编码：一层字符串，且大多数命令没有菜单。
func TestBuildTopLevelString(t *testing.T) {
	cmds := []control.Command{
		{ID: "file.open", Label: "Open…", Menu: control.MenuPath{"File"}},
		{ID: "file.pin_folder", Label: "Pin folder", Menu: control.MenuPath{"File"}},
		{ID: "measure.distance", Label: "Measure distance"},
	}
	m := Build(app(t, "pdfcraft"), "0.14.0", cmds)
	if len(m.Sections) != 2 {
		t.Fatalf("应有两节，得到 %v", names(m.Sections))
	}
	if m.Sections[0].Name != "File" || m.Sections[0].Count != 2 || len(m.Sections[0].Items) != 2 {
		t.Fatalf("File 节不对: %+v", m.Sections[0])
	}
	if !m.Sections[1].Grouped || m.Sections[1].Name != "measure" {
		t.Fatalf("兜底节不对: %+v", m.Sections[1])
	}
}

// 一个节点的 Count 必须是整个子树，否则界面上的数字会骗人。
func TestCountsAreSubtree(t *testing.T) {
	cmds := []control.Command{
		{ID: "a.x", Menu: control.MenuPath{"A", "B", "C"}},
		{ID: "a.y", Menu: control.MenuPath{"A", "B"}},
		{ID: "a.z", Menu: control.MenuPath{"A"}},
	}
	m := Build(app(t, "deckcraft"), "", cmds)
	if m.Sections[0].Count != 3 {
		t.Fatalf("A 应为 3，得到 %d", m.Sections[0].Count)
	}
	b, _ := m.Find("A", "B")
	if b.Count != 2 || len(b.Items) != 1 {
		t.Fatalf("A › B 应有 2 条（1 条自己的 + 1 条子节点），得到 count=%d items=%d", b.Count, len(b.Items))
	}
}

// 命令表里同一个菜单路径反复出现时，不能建出重复节点。
func TestRepeatedPathReusesNode(t *testing.T) {
	cmds := []control.Command{
		{ID: "x.one", Menu: control.MenuPath{"Edit", "Select"}},
		{ID: "x.two", Menu: control.MenuPath{"Edit", "Select"}},
		{ID: "x.three", Menu: control.MenuPath{"Edit"}},
	}
	m := Build(app(t, "deckcraft"), "", cmds)
	if len(m.Sections) != 1 || len(m.Sections[0].Children) != 1 {
		t.Fatalf("节点重复: %+v", m.Sections)
	}
	if m.Sections[0].Count != 3 || m.Sections[0].Children[0].Count != 2 {
		t.Fatalf("计数错误: %d/%d", m.Sections[0].Count, m.Sections[0].Children[0].Count)
	}
}

func TestSearch(t *testing.T) {
	cmds := []control.Command{
		{ID: "format.bold", Label: "Bold", Menu: control.MenuPath{"Format", "Text"}, Shortcut: "Cmd+B"},
		{ID: "slide.new", Label: "新建幻灯片", Menu: control.MenuPath{"Home"}},
		{ID: "qnew", Label: "Quick new", Aliases: []string{"qnew"}},
	}
	m := Build(app(t, "deckcraft"), "", cmds)

	if got := m.Search("bold", 10); len(got) != 1 || got[0].ID != "format.bold" {
		t.Fatalf("按 id/标签搜索失败: %+v", got)
	}
	if got := m.Search("幻灯片", 10); len(got) != 1 || got[0].ID != "slide.new" {
		t.Fatalf("中文标签搜索失败: %+v", got)
	}
	if got := m.Search("format", 10); len(got) != 1 {
		t.Fatalf("按菜单路径搜索失败: %+v", got)
	}
	if got := m.Search("cmd+b", 10); len(got) != 1 {
		t.Fatalf("按快捷键搜索失败: %+v", got)
	}
	if got := m.Search("qnew", 10); len(got) != 1 {
		t.Fatalf("按别名搜索失败: %+v", got)
	}
	if got := m.Search("nothing-matches-this", 10); len(got) != 0 {
		t.Fatalf("不该有结果: %+v", got)
	}
}

// 落盘 → 读回，模型必须一模一样：界面启动时靠的就是这条路。
func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	cmds := []control.Command{
		{ID: "format.bold", Label: "Bold", Menu: control.MenuPath{"Format", "Text"}, Shortcut: "Cmd+B"},
		{ID: "file.render", Label: "Render"},
	}
	m := Build(app(t, "deckcraft"), "0.11.1", cmds)
	if err := store.save(m); err != nil {
		t.Fatalf("落盘失败: %v", err)
	}

	again := NewStore(dir)
	if err := again.Load(); err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	got, ok := again.Get("deckcraft")
	if !ok {
		t.Fatal("读回后拿不到菜单")
	}
	if got.Commands != 2 || got.Mapped != 1 || got.Grouped != 1 {
		t.Fatalf("读回后计数不对: %+v", got)
	}
	if len(got.Sections) != 2 || got.Sections[0].Name != "Format" {
		t.Fatalf("读回后结构不对: %+v", names(got.Sections))
	}
	if got.Sections[0].Children[0].Items[0].Shortcut != "Cmd+B" {
		t.Fatalf("读回后丢了快捷键: %+v", got.Sections[0].Children[0].Items[0])
	}
}

// 缓存命中就不该再打应用；版本变了必须重新拉。
func TestCommandsCacheByVersion(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	app := app(t, "deckcraft")

	// 手工塞一份缓存，模拟「上次预加载过」。
	store.entries["deckcraft"] = &entry{
		menu:     Build(app, "0.11.1", []control.Command{{ID: "a.b"}}),
		commands: []control.Command{{ID: "a.b"}},
		at:       time.Now(),
	}

	fake := &fakeFetcher{cmds: []control.Command{{ID: "a.b"}, {ID: "c.d"}}}

	// 缓存够新、版本一致：不该去问应用。
	cmds, m, err := store.Commands(context.Background(), fake, app, "0.11.1", time.Hour)
	if err != nil || len(cmds) != 1 || m.Commands != 1 {
		t.Fatalf("缓存未命中: %v %+v", err, m)
	}
	if fake.calls != 0 {
		t.Fatalf("命中缓存却请求了应用 %d 次", fake.calls)
	}

	// 版本变了：必须重新拉一次。
	cmds, m, err = store.Commands(context.Background(), fake, app, "0.13.0", time.Hour)
	if err != nil {
		t.Fatalf("刷新失败: %v", err)
	}
	if fake.calls != 1 || len(cmds) != 2 || m.Version != "0.13.0" {
		t.Fatalf("版本变化没有刷新: calls=%d cmds=%d version=%s", fake.calls, len(cmds), m.Version)
	}

	// 文件命名要能一眼看出是哪个应用。
	raw, err := os.ReadFile(filepath.Join(dir, "menus", "deckcraft.json"))
	if err != nil {
		t.Fatalf("菜单文件不存在: %v", err)
	}
	var onDisk Menu
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("菜单文件不是合法 JSON: %v", err)
	}
	if onDisk.App != "deckcraft" {
		t.Fatalf("文件内容不对: %+v", onDisk)
	}
}

func TestSummariesMarkStale(t *testing.T) {
	store := NewStore(t.TempDir())
	store.entries["deckcraft"] = &entry{menu: Build(app(t, "deckcraft"), "0.11.1", []control.Command{{ID: "a.b", Menu: control.MenuPath{"File"}}})}
	sums := store.Summaries(func(a craft.App) string {
		if a.ID == "deckcraft" {
			return "0.12.0"
		}
		return ""
	})
	if len(sums) != 1 || !sums[0].Stale {
		t.Fatalf("版本变化没有标记 stale: %+v", sums)
	}
	if sums[0].Commands != 1 || sums[0].Sections != 1 {
		t.Fatalf("摘要数字不对: %+v", sums[0])
	}
}

// fakeFetcher 记录被调用次数，用来断言「缓存该不该被用」。
type fakeFetcher struct {
	cmds  []control.Command
	calls int
	err   error
}

func (f *fakeFetcher) Commands(context.Context) ([]control.Command, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.cmds, nil
}

func names(nodes []*Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Name)
	}
	return out
}
