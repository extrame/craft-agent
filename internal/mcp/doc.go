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
	cmdFileClose  = "file.close"
	cmdWindowNext = "window.next"
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

// readDoc 读活动文档的身份。file.info 是唯一能拿到标题与路径的命令。
func (b *bridge) readDoc(ctx context.Context, c *control.Client) (docIdentity, error) {
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

// anchorNow 把当前文档记为本次会话的锚点。
func (b *bridge) anchorNow(doc docIdentity) {
	b.docMu.Lock()
	copied := doc
	b.anchor = &copied
	b.docMu.Unlock()
}

// anchorValue 返回当前锚点（副本）。
func (b *bridge) anchorValue() (docIdentity, bool) {
	b.docMu.Lock()
	defer b.docMu.Unlock()
	if b.anchor == nil {
		return docIdentity{}, false
	}
	return *b.anchor, true
}

// guardDoc 是所有写入路径的前置条件：确认还在同一次文档上。
//
// 没有锚点时自动锚定当前文档（会话第一次写入即锁定目标）；
// 锚点存在但当前活动文档不同时，直接拒绝执行并说明怎么改。
// 这是把「写错文档不报错」变成「写错文档立刻停」的唯一办法——
// 因为控制通道根本没有 document 参数可以显式指定。
func (b *bridge) guardDoc(ctx context.Context, c *control.Client) (docIdentity, error) {
	cur, err := b.readDoc(ctx, c)
	if err != nil {
		return docIdentity{}, fmt.Errorf("读取当前文档失败: %w", err)
	}
	anchored, ok := b.anchorValue()
	if !ok {
		b.anchorNow(cur)
		return cur, nil
	}
	if !anchored.same(cur) {
		return cur, fmt.Errorf("文档已切换，命令未发送：本次会话锚定的是 %s，当前活动文档是 %s。\n"+
			"控制通道没有 document 参数，所有写入都落在「当前文档」上，所以这次写入会被拒绝。\n"+
			"确认要改当前文档：craft_doc {action:\"use\", index:<文档序号>}，或 craft_doc {action:\"anchor\"} 就地重锚。",
			anchored, cur)
	}
	return cur, nil
}

// docEntry 是 craft_doc list 的一项。
type docEntry struct {
	Index int `json:"index"`
	docIdentity
	Active bool `json:"active"`
}

// listDocs 枚举所有打开的文档。
//
// 只能靠 window.next {index} 逐个切过去读 file.info：越界索引是静默 no-op，
// 所以「连续两次读到同一份身份」就是枚举结束。结束时切回原来的文档。
func (b *bridge) listDocs(ctx context.Context, c *control.Client) ([]docEntry, int, error) {
	start, err := b.readDoc(ctx, c)
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
		cur, err := b.readDoc(ctx, c)
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

func (b *bridge) registerDocTools(s *server.MCPServer) {
	s.AddTool(mcp.NewTool("craft_doc",
		mcp.WithDescription("Work with the DOCUMENT layer explicitly. The control channel has no document parameter: every write goes to whatever document is active, and file.new / the user's own window switch changes that silently. craft_doc makes it visible: info reads the active document, list enumerates every open document, use switches to one and anchors it, anchor pins the current one, release unpins. Every write tool refuses to run when the active document is not the anchored one."),
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
	action := strings.ToLower(strings.TrimSpace(req.GetString("action", "info")))
	start := time.Now()

	switch action {
	case "info":
		cur, err := b.readDoc(ctx, c)
		if err != nil {
			return errResult(err), nil
		}
		out := map[string]any{"app": app.ID, "document": cur}
		if a, ok := b.anchorValue(); ok {
			out["anchored"] = a
			out["drifted"] = !a.same(cur)
		} else {
			out["anchored"] = nil
			out["drifted"] = false
		}
		b.record(app.ID, "command", "craft_doc:info", true, cur.String(), time.Since(start))
		return mcp.NewToolResultJSON(out)

	case "list":
		entries, current, err := b.listDocs(ctx, c)
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
		if a, ok := b.anchorValue(); ok {
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
		cur, err := b.readDoc(ctx, c)
		if err != nil {
			return errResult(err), nil
		}
		if want, ok := argString(req, "title"); ok && want != "" && cur.Title != want {
			return errResult(fmt.Errorf("已切到文档 %d，但标题是 %q 而不是 %q；未改变锚点", index, cur.Title, want)), nil
		}
		b.anchorNow(cur)
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
		cur, err := b.readDoc(ctx, c)
		if err != nil {
			return errResult(err), nil
		}
		b.anchorNow(cur)
		b.record(app.ID, "command", "craft_doc:new", true, cur.String(), time.Since(start))
		return mcp.NewToolResultJSON(map[string]any{
			"app":      app.ID,
			"index":    created,
			"document": cur,
			"anchored": true,
		})

	case "close":
		cur, err := b.readDoc(ctx, c)
		if err != nil {
			return errResult(err), nil
		}
		if _, err := b.exec(ctx, c, cmdFileClose, nil); err != nil {
			return errResult(err), nil
		}
		after, _ := b.readDoc(ctx, c)
		b.anchorNow(after)
		b.record(app.ID, "command", "craft_doc:close", true, cur.String(), time.Since(start))
		return mcp.NewToolResultJSON(map[string]any{
			"app":      app.ID,
			"closed":   cur,
			"document": after,
			"anchored": true,
		})

	case "anchor":
		cur, err := b.readDoc(ctx, c)
		if err != nil {
			return errResult(err), nil
		}
		b.anchorNow(cur)
		b.record(app.ID, "command", "craft_doc:anchor", true, cur.String(), time.Since(start))
		return mcp.NewToolResultJSON(map[string]any{"app": app.ID, "anchored": cur})

	case "release":
		b.docMu.Lock()
		b.anchor = nil
		b.docMu.Unlock()
		b.record(app.ID, "command", "craft_doc:release", true, "", time.Since(start))
		return mcp.NewToolResultJSON(map[string]any{"app": app.ID, "anchored": nil})

	default:
		return errResult(fmt.Errorf("未知 action %q；可选 info|list|use|new|close|anchor|release", action)), nil
	}
}
