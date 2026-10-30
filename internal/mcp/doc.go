package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"craft-agent/internal/control"
	"craft-agent/internal/craft"
)

// 这个文件解决「文档层」的隐式状态。
//
// 实测事实（DeckCraft 0.11.1）：
//   - file.new 返回 {"document": 1, "slides": 0}，说明服务端有多文档概念；
//   - 但 document 组只有 document.inspect 一条，且只能读「当前」文档；
//   - 没有任何命令接受 document 参数，所有写入都作用于活动文档；
//   - 唯一的切换手段是 window.next {index} —— 它其实就是缺位的 document.activate，
//     越界索引是静默 no-op（不报错、不切页）。
//
// 于是「活动文档」成了一个看不见的全局变量：用户手动新建/切换文档后，
// agent 的下一条 insert 会安静地写进另一份文档。这里做的三件事：
//
//  1. craft_doc      把文档层显式化：info / list / use / new / close / anchor / release。
//  2. 写入前守卫      每条写入命令前比对文档指纹，锚定过的文档变了就直接报错，不发命令。
//  3. 写入后回报      每个写工具都带回 {document, slide, shape}，调用方可以自证写对了地方。
const (
	cmdFileInfo   = "file.info"
	cmdFileNew    = "file.new"
	cmdFileOpen   = "file.open"
	cmdFileClose  = "file.close"
	cmdWindowNext = "window.next"
	cmdDocList    = "doc.list"
	// CADCraft 0.13：AutoCAD 系命名。身份读 drawing.inspect（与 file.info 同形），
	// 按路径打开文件是 `open`（实测接受 {"path": …}）。
	cmdDrawingInspect = "drawing.inspect"
	cmdOpen           = "open"
)

// 文档身份来源。DeckCraft 这代用 file.info（只报当前文档）；
// FigCraft 0.11.2 没有 file.info，但 doc.list 直接返回全部打开文档
// （name/path/dirty/active），信息反而更全 —— 实测（2026-10-28）：
//
//	[{"active":true,"dirty":true,"index":0,"name":"新版网站设计",
//	  "path":"/Users/…/新版网站设计.figcraft"}]
//
// CADCraft 0.13 两个都没有，但 drawing.inspect 返回的字段与 file.info 同形
// （title/path/dirty，外加 layers/extents 等 CAD 状态）—— 实测（2026-10-30）：
//
//	{"dirty":false,"path":null,"title":"Drawing1", …}
const (
	srcFileInfo       = "file.info"
	srcDocList        = "doc.list"
	srcDrawingInspect = "drawing.inspect"
)

// maxDocs 是枚举窗口的上限。超过这个数量的文档没人会指望 agent 去管。
const maxDocs = 24

// docIdentity 是文档的可辨识身份。
//
// 控制通道不发布文档 id，file.info 只给 title / path，所以指纹只能由这两项组成。
// slides 与 dirty 会随编辑变化，只用于展示，不参与比对。
type docIdentity struct {
	Title  string `json:"title"`
	Path   string `json:"path"`
	Slides int    `json:"slides"`
	Dirty  bool   `json:"dirty"`
}

func (d docIdentity) same(other docIdentity) bool {
	return d.Title == other.Title && d.Path == other.Path
}

func (d docIdentity) String() string {
	if d.Path != "" {
		return fmt.Sprintf("%s（%s）", d.Title, d.Path)
	}
	return fmt.Sprintf("%s（未保存）", d.Title)
}

// readDoc 读活动文档的身份，按应用的身份来源分派。
func (b *bridge) readDoc(ctx context.Context, app craft.App, c *control.Client) (docIdentity, error) {
	switch b.identitySource(ctx, app, c) {
	case srcDocList:
		return b.readDocViaDocList(ctx, c)
	case srcDrawingInspect:
		return b.readDocViaDrawingInspect(ctx, c)
	default:
		return b.readDocViaFileInfo(ctx, c)
	}
}

// readDocViaFileInfo 是 file.info 来源：只报当前文档。
func (b *bridge) readDocViaFileInfo(ctx context.Context, c *control.Client) (docIdentity, error) {
	raw, err := c.Execute(ctx, cmdFileInfo, nil)
	if err != nil {
		return docIdentity{}, err
	}
	var info struct {
		Title  string `json:"title"`
		Path   string `json:"path"`
		Slides int    `json:"slides"`
		Dirty  bool   `json:"dirty"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return docIdentity{}, fmt.Errorf("file.info 返回无法解析: %w", err)
	}
	return docIdentity{Title: info.Title, Path: info.Path, Slides: info.Slides, Dirty: info.Dirty}, nil
}

// docListEntry 是 doc.list 返回的一项（FigCraft 0.11.2 实测形状）。
type docListEntry struct {
	Index  int    `json:"index"`
	Name   string `json:"name"`
	Path   string `json:"path"`
	Dirty  bool   `json:"dirty"`
	Active bool   `json:"active"`
}

// parseDocList 把 doc.list 的返回压成 (全部条目, 活动项)。
// 空列表返回错误；没有 active 标记时取第一项 —— 列表为空才说明没有文档。
func parseDocList(raw json.RawMessage) ([]docListEntry, docListEntry, error) {
	var entries []docListEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		// 有的实现把数组包在对象里，做一次兼容。
		var wrapped struct {
			Documents []docListEntry `json:"documents"`
		}
		if err2 := json.Unmarshal(raw, &wrapped); err2 != nil || wrapped.Documents == nil {
			return nil, docListEntry{}, fmt.Errorf("doc.list 返回无法解析: %w", err)
		}
		entries = wrapped.Documents
	}
	if len(entries) == 0 {
		return nil, docListEntry{}, fmt.Errorf("doc.list 返回空列表：没有打开的文档")
	}
	active := entries[0]
	for _, e := range entries {
		if e.Active {
			active = e
			break
		}
	}
	return entries, active, nil
}

// readDocViaDocList 是 doc.list 来源：取 active 项（或第一项）当活动文档。
func (b *bridge) readDocViaDocList(ctx context.Context, c *control.Client) (docIdentity, error) {
	raw, err := c.Execute(ctx, cmdDocList, nil)
	if err != nil {
		return docIdentity{}, err
	}
	_, active, err := parseDocList(raw)
	if err != nil {
		return docIdentity{}, err
	}
	return docIdentity{Title: active.Name, Path: active.Path, Dirty: active.Dirty}, nil
}

// readDocViaDrawingInspect 是 drawing.inspect 来源（CADCraft 0.13）。
// 返回字段与 file.info 同形（title/path/dirty），path 为 null 时解成空串。
func (b *bridge) readDocViaDrawingInspect(ctx context.Context, c *control.Client) (docIdentity, error) {
	raw, err := c.Execute(ctx, cmdDrawingInspect, nil)
	if err != nil {
		return docIdentity{}, err
	}
	var info struct {
		Title string `json:"title"`
		Path  string `json:"path"`
		Dirty bool   `json:"dirty"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return docIdentity{}, fmt.Errorf("drawing.inspect 返回无法解析: %w", err)
	}
	return docIdentity{Title: info.Title, Path: info.Path, Dirty: info.Dirty}, nil
}

// hasDocModel 报告该应用是否真的提供文档身份。
//
// 文档上下文是 agent 端维护的，但「锚点比对」这一半依赖应用能回答「我在哪一份里」。
// 来源有三条：file.info（DeckCraft 这代）、doc.list（FigCraft 0.11.2）、
// drawing.inspect（CADCraft 0.13）。一条都没有的应用走降级模式：
// 守卫跳过，结果里如实注明 guard=none。
func (b *bridge) hasDocModel(ctx context.Context, app craft.App, c *control.Client) bool {
	return b.identitySource(ctx, app, c) != ""
}

// identitySource 报告该应用用哪条命令读「当前文档身份」。
// 命令表拿不到时保守按 file.info 处理 —— 那时真正的执行也活不成，
// 报原始错误比静默跳过守卫更接近真相。
func (b *bridge) identitySource(ctx context.Context, app craft.App, c *control.Client) string {
	set, ok := b.commandSet(ctx, app, c)
	if !ok {
		return srcFileInfo
	}
	if set[cmdFileInfo] {
		return srcFileInfo
	}
	if set[cmdDocList] {
		return srcDocList
	}
	if set[cmdDrawingInspect] {
		return srcDrawingInspect
	}
	return ""
}

const docGuardNote = "该应用没有可读文档身份的命令（file.info / doc.list / drawing.inspect 都不存在），" +
	"文档守卫不生效：写入始终作用于当前活动文档，切换文档后请先用该应用的 inspect 类命令确认落点。"

// anchorNow 把当前文档记为该应用的锚点（内存 + 落盘，见 anchor.go）。
func (b *bridge) anchorNow(app craft.App, doc docIdentity) {
	b.docMu.Lock()
	defer b.docMu.Unlock()
	b.anchors.put(app.ID, doc)
}

// anchorValue 返回当前锚点（副本）：本会话声明过的优先，否则读上个会话落盘的。
func (b *bridge) anchorValue(app craft.App) (docIdentity, bool) {
	b.docMu.Lock()
	defer b.docMu.Unlock()
	return b.anchors.get(app.ID)
}

// docEntry 是 craft_doc list 的一项。
type docEntry struct {
	Index int `json:"index"`
	docIdentity
	Active bool `json:"active"`
}

// listDocs 枚举所有打开的文档，按应用的身份来源分派。
func (b *bridge) listDocs(ctx context.Context, app craft.App, c *control.Client) ([]docEntry, int, error) {
	switch b.identitySource(ctx, app, c) {
	case srcDocList:
		return b.listDocsViaDocList(ctx, c)
	case srcDrawingInspect:
		// CADCraft 只报得出当前这一份（drawing.inspect 没有 index/列表），
		// 枚举没有依据，如实给单条，别拿 window.next 去碰运气 ——
		// 它没有这条命令，切窗口的尝试只会白费。
		cur, err := b.readDocViaDrawingInspect(ctx, c)
		if err != nil {
			return nil, -1, err
		}
		return []docEntry{{Index: 0, docIdentity: cur, Active: true}}, 0, nil
	default:
		return b.listDocsViaWindows(ctx, c)
	}
}

// listDocsViaWindows 是 file.info 来源的枚举：靠 window.next 逐个切过去读。
//
// 只能这么干：file.info 只报当前文档，window.next {index} 是唯一的切换手段 ——
// 越界索引是静默 no-op，所以「连续两次读到同一份身份」就是枚举结束。
// 结束时切回原来的文档。
func (b *bridge) listDocsViaWindows(ctx context.Context, c *control.Client) ([]docEntry, int, error) {
	start, err := b.readDocViaFileInfo(ctx, c)
	if err != nil {
		return nil, -1, err
	}

	entries := make([]docEntry, 0, 4)
	prev := docIdentity{}
	startIndex := -1
	for i := 0; i < maxDocs; i++ {
		if _, err := c.Execute(ctx, cmdWindowNext, map[string]any{"index": i}); err != nil {
			break
		}
		cur, err := b.readDocViaFileInfo(ctx, c)
		if err != nil {
			break
		}
		if i > 0 && cur.same(prev) {
			// 越界：window.next 什么都没做，枚举到此为止。
			break
		}
		entries = append(entries, docEntry{Index: i, docIdentity: cur})
		if cur.same(start) {
			startIndex = i
		}
		prev = cur
	}

	// 切回原来那份，别把人家的窗口留在别处。
	if startIndex >= 0 {
		_, _ = c.Execute(ctx, cmdWindowNext, map[string]any{"index": startIndex})
	}
	for i := range entries {
		entries[i].Active = entries[i].Index == startIndex
	}
	return entries, startIndex, nil
}

// listDocsViaDocList 是 doc.list 来源的枚举：一条命令，零切换。
func (b *bridge) listDocsViaDocList(ctx context.Context, c *control.Client) ([]docEntry, int, error) {
	raw, err := c.Execute(ctx, cmdDocList, nil)
	if err != nil {
		return nil, -1, err
	}
	entries, _, err := parseDocList(raw)
	if err != nil {
		return nil, -1, err
	}
	out := make([]docEntry, 0, len(entries))
	current := -1
	for _, e := range entries {
		out = append(out, docEntry{
			Index:       e.Index,
			docIdentity: docIdentity{Title: e.Name, Path: e.Path, Dirty: e.Dirty},
			Active:      e.Active,
		})
		if e.Active {
			current = e.Index
		}
	}
	if current < 0 {
		// 没有 active 标记时与 readDocViaDocList 同口径：第一项视为当前。
		current = entries[0].Index
	}
	return out, current, nil
}

func (b *bridge) registerDocTools(s *server.MCPServer) {
	s.AddTool(mcp.NewTool("craft_doc",
		mcp.WithDescription("Work with the DOCUMENT layer explicitly. The control channel has no document parameter: every write goes to whatever document is active, and file.new / the user's own window switch changes that silently. craft_doc makes it visible: info reads the active document, list enumerates every open document, use switches to one and anchors it, anchor pins the current one, release unpins. The anchor always follows the active document: when the user or you open/switch/save-as another file, subsequent writes go to that file (reported in guardNotes); only when no document can be read (app restarted) does the guard re-open the anchored file by path."),
		mcp.WithString("app", mcp.Required(), mcp.Description("App id.")),
		mcp.WithString("action", mcp.Required(), mcp.Description("info | list | use | new | close | anchor | release")),
		mcp.WithNumber("index", mcp.Description("Document index for action=use (0-based, as returned by list/new).")),
		mcp.WithString("title", mcp.Description("Optional expected title for action=use: after switching, fail if the document is not this one.")),
		mcp.WithBoolean("blank", mcp.Description("For action=new: create an empty deck. Default true.")),
		mcp.WithString("theme", mcp.Description("For action=new: theme name.")),
		mcp.WithString("template", mcp.Description("For action=new: starter template name.")),
	), b.handleDoc)
}

func (b *bridge) handleDoc(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	app, c, err := b.openReq(ctx, req)
	if err != nil {
		return errResult(err), nil
	}
	// craft_doc 的每个 action 都要从 file.info 读身份。应用没有这条命令时
	// 明确说明文档层对它不成立，而不是让六种 action 各自报 unknown command。
	if !b.hasDocModel(ctx, app, c) {
		return errResult(fmt.Errorf("%s 不提供文档身份，craft_doc 对它不适用。%s", app.Name, docGuardNote)), nil
	}
	action := strings.ToLower(strings.TrimSpace(req.GetString("action", "info")))
	start := time.Now()

	switch action {
	case "info":
		cur, err := b.readDoc(ctx, app, c)
		if err != nil {
			return errResult(err), nil
		}
		out := map[string]any{"app": app.ID, "document": cur}
		if a, ok := b.anchorValue(app); ok {
			out["anchored"] = a
			out["drifted"] = !a.same(cur)
		} else {
			out["anchored"] = nil
			out["drifted"] = false
		}
		b.record(app.ID, "command", "craft_doc:info", true, cur.String(), time.Since(start))
		return mcp.NewToolResultJSON(out)

	case "list":
		entries, current, err := b.listDocs(ctx, app, c)
		if err != nil {
			return errResult(err), nil
		}
		payload := map[string]any{
			"app":       app.ID,
			"documents": entries,
			"count":     len(entries),
			"current":   current,
			"note":      "枚举过程会真实切换窗口再切回，界面上会看到一次闪动；越界索引是静默 no-op，所以连续两次读到同一文档即判定结束。",
		}
		if a, ok := b.anchorValue(app); ok {
			payload["anchored"] = a
		}
		b.record(app.ID, "command", "craft_doc:list", true, fmt.Sprintf("docs=%d", len(entries)), time.Since(start))
		return mcp.NewToolResultJSON(payload)

	case "use":
		index := req.GetInt("index", -1)
		if index < 0 {
			return errResult(fmt.Errorf("action=use 需要 index（craft_doc action=list 可以枚举）")), nil
		}
		if _, err := c.Execute(ctx, cmdWindowNext, map[string]any{"index": index}); err != nil {
			return errResult(fmt.Errorf("切换到文档 %d 失败: %w", index, err)), nil
		}
		cur, err := b.readDoc(ctx, app, c)
		if err != nil {
			return errResult(err), nil
		}
		if want, ok := argString(req, "title"); ok && want != "" && cur.Title != want {
			return errResult(fmt.Errorf("已切到文档 %d，但标题是 %q 而不是 %q；未改变锚点", index, cur.Title, want)), nil
		}
		b.anchorNow(app, cur)
		b.record(app.ID, "command", "craft_doc:use", true, fmt.Sprintf("#%d %s", index, cur.String()), time.Since(start))
		return mcp.NewToolResultJSON(map[string]any{
			"app":      app.ID,
			"index":    index,
			"document": cur,
			"anchored": true,
			"note":     "window.next 越界是静默 no-op：如果标题不是预期的，说明 index 不存在。",
		})

	case "new":
		params := map[string]any{"blank": req.GetBool("blank", true)}
		if v, ok := argString(req, "theme"); ok && v != "" {
			params["theme"] = v
		}
		if v, ok := argString(req, "template"); ok && v != "" {
			params["template"] = v
		}
		raw, err := b.exec(ctx, c, cmdFileNew, params)
		if err != nil {
			return errResult(err), nil
		}
		created := intField(raw, "document")
		cur, err := b.readDoc(ctx, app, c)
		if err != nil {
			return errResult(err), nil
		}
		b.anchorNow(app, cur)
		b.record(app.ID, "command", "craft_doc:new", true, cur.String(), time.Since(start))
		return mcp.NewToolResultJSON(map[string]any{
			"app":      app.ID,
			"index":    created,
			"document": cur,
			"anchored": true,
		})

	case "close":
		cur, err := b.readDoc(ctx, app, c)
		if err != nil {
			return errResult(err), nil
		}
		if _, err := b.exec(ctx, c, cmdFileClose, nil); err != nil {
			return errResult(err), nil
		}
		after, _ := b.readDoc(ctx, app, c)
		b.anchorNow(app, after)
		b.record(app.ID, "command", "craft_doc:close", true, cur.String(), time.Since(start))
		return mcp.NewToolResultJSON(map[string]any{
			"app":      app.ID,
			"closed":   cur,
			"document": after,
			"anchored": true,
		})

	case "anchor":
		cur, err := b.readDoc(ctx, app, c)
		if err != nil {
			return errResult(err), nil
		}
		b.anchorNow(app, cur)
		b.record(app.ID, "command", "craft_doc:anchor", true, cur.String(), time.Since(start))
		return mcp.NewToolResultJSON(map[string]any{"app": app.ID, "anchored": cur})

	case "release":
		b.docMu.Lock()
		b.anchors.drop(app.ID)
		b.docMu.Unlock()
		b.record(app.ID, "command", "craft_doc:release", true, "", time.Since(start))
		return mcp.NewToolResultJSON(map[string]any{"app": app.ID, "anchored": nil})

	default:
		return errResult(fmt.Errorf("未知 action %q；可选 info|list|use|new|close|anchor|release", action)), nil
	}
}
