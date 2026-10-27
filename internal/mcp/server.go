// Package mcpserver 把 Craft 套件的控制通道包装成一个 MCP 服务器。
//
// 设计取向：不硬编码任何一个应用的命令。启动后向应用询问 engine.commands，
// 拿到它自己的完整命令表（DeckCraft 0.11.1 有 377 条），再用它来生成工具与文档。
// 这样 WordCraft / GridCraft 装上来就能用，不需要改这个包。
//
// 工具分成两层：
//   - 元工具（默认全部暴露）：列应用、连接、读文档、跑命令、搜命令、截图。
//   - 命令工具（可选暴露）：把某条命令直接变成一个工具，省掉模型查表的一步。
//     默认关闭 —— 一个应用就有几百条命令，全开会把上下文塞满。
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"craft-agent/internal/control"
	"craft-agent/internal/craft"
	"craft-agent/internal/host"
	"craft-agent/internal/journal"
)

// 包内别名：把控制通道的类型带进来，调用处不必到处写 control.Xxx。
type (
	controlClient  = control.Client
	controlCommand = control.Command
)

// ToolMode 控制命令工具暴露的粒度。
type ToolMode string

const (
	// ModeMinimal 只暴露元工具。推荐默认值。
	ModeMinimal ToolMode = "minimal"
	// ModeCore 额外暴露每个应用的主干命令组（slide / text / shape / file 等）。
	ModeCore ToolMode = "core"
	// ModeFull 把每条可用命令都变成工具。命令多的应用会撑爆上下文。
	ModeFull ToolMode = "full"
)

// Options 是服务器装配参数。
type Options struct {
	Host           *host.Host
	Apps           []craft.App
	Version        string
	Mode           ToolMode
	MaxToolsPerApp int // 命令工具上限，0 表示不限制

	// Journal 是跨进程流水：agent 的每个写操作都要在这里留痕，
	// 界面才知道「刚才谁改了什么」。
	Journal *journal.Journal
	Session string
}

// Serve 启动 stdio MCP 服务器，直到 ctx 结束或客户端断开。
func Serve(ctx context.Context, opts Options) error {
	if opts.Host == nil {
		return fmt.Errorf("缺少 host")
	}
	if opts.Version == "" {
		opts.Version = "0.1.0"
	}
	if opts.Mode == "" {
		opts.Mode = ModeMinimal
	}
	if len(opts.Apps) == 0 {
		opts.Apps = craft.Suite
	}
	if opts.Session == "" {
		opts.Session = fmt.Sprintf("mcp:%d", os.Getpid())
	}

	s := server.NewMCPServer(
		"craft-agent",
		opts.Version,
		server.WithToolCapabilities(true),
		server.WithInstructions(instructions(opts.Apps)),
	)

	b := &bridge{opts: opts, caps: map[string]map[string]bool{}}
	b.registerMetaTools(s)
	b.registerDocTools(s)
	b.registerComposeTools(s)

	// 会话起止进流水：界面上「现在有几个 AI 会话连着」靠这两条。
	b.record("", "session-start", "", true, strings.Join(appNames(opts.Apps), ", "), 0)
	defer b.record("", "session-end", "", true, "", 0)

	// 已经开着的实例先接管，省掉模型第一次调用时的启动等待。
	// 失败不报错：app 没装是常态。
	for _, app := range opts.Apps {
		go func(a craft.App) {
			if !a.Installed() {
				return
			}
			_ = opts.Host.Connect(ctx, a.ID)
		}(app)
	}

	go func() {
		<-ctx.Done()
		opts.Host.Close()
	}()

	return server.ServeStdio(s)
}

func instructions(apps []craft.App) string {
	names := make([]string, 0, len(apps))
	for _, a := range apps {
		names = append(names, a.ID)
	}
	return fmt.Sprintf(`Craft Agent drives local Craft suite apps (photo, vector, deck, word, grid, CAD…) through their JSON control channels.

Available apps in this session: %s.

Two ways in, and the composite tools come first:

A. Composite tools — one call does a whole intent, and the result is verified by the app itself:
   craft_doc        — the DOCUMENT layer (any app): info / list / use / new / close / anchor / release.
   deckcraft_page   — DeckCraft only. add/switch/delete/duplicate/move a page; always leaves you on a known page.
   deckcraft_text   — DeckCraft only. Create or restyle a text box (font, size, bold, colour, align, anchor,
                      bullets) in ONE call.
   deckcraft_style  — DeckCraft only. Restyle existing shapes by id, across pages, with per-shape reporting.
   craft_batch      — any app: run many commands in one call, stop at the first failure; pass slide to pin the page.
   Prefer these whenever the intent matches. The deckcraft_* tools exist because the channel has no command
   that means "typeset this text box": it only has primitives (insert.textBox, format.*) that act on a text
   selection you cannot see and that does not survive a call. On any app other than DeckCraft they are
   refused by design — use craft_run with that app's own command table.

B. Raw channel — for anything the composites do not cover:
   1. craft_apps — what is installed and already connected.
   2. craft_open — connect; launches the app when nothing is running.
   3. craft_inspect — read the document before editing, so ids and layouts are real.
   4. craft_commands — search the app's own command table for a command id and its parameter doc.
   5. craft_run — execute one command.

Rules that save you a lot of pain:
- THREE layers of context are implicit: document, then page, then shape. Only shapes carry explicit ids.
  The document has no parameter at all: every write goes to the active document. Each write tool therefore
  anchors the active document on first use and REFUSES to run if it changed — the user may have opened another
  document. When a write is refused for that reason, call craft_doc (list → use) instead of working around it.
- insert.* has no page parameter; it follows the active page. Call deckcraft_page action=goto (or craft_batch slide=N)
  before inserting, and trust the returned verified.onSlide to prove where the shape landed.
- Ids are only valid on the page the shape lives on. Read the deck before using an id you got earlier.
- Never build a layout out of dozens of craft_run calls; put them in craft_batch with verify=true.
- Every write reports document + slide + shape back. Check it instead of assuming the write went where you meant.`, strings.Join(names, ", "))
}

type bridge struct {
	opts Options

	// 每个应用的命令表缓存：封装层用它核对「这个应用到底有没有这些命令」。
	capMu sync.Mutex
	caps  map[string]map[string]bool

	// 文档锚点。控制通道没有 document 参数，写入只能落到「当前文档」，
	// 所以这里记住会话开始时的那一份，写入前比对，变了就拒绝执行。
	docMu  sync.Mutex
	anchor *docIdentity
}

func (b *bridge) registerMetaTools(s *server.MCPServer) {
	s.AddTool(mcp.NewTool("craft_apps",
		mcp.WithDescription("List every app in the Craft suite with its install path, connection state and control channel. Call this first to see what is available."),
	), b.handleApps)

	s.AddTool(mcp.NewTool("craft_open",
		mcp.WithDescription("Connect to a Craft app. If no instance is running it launches the app with its control channel enabled. Returns the endpoint and version."),
		mcp.WithString("app", mcp.Required(), mcp.Description("App id, e.g. deckcraft, wordcraft, gridcraft.")),
		mcp.WithBoolean("relaunch", mcp.Description("Stop an existing connection and launch a fresh process. Default false.")),
	), b.handleOpen)

	s.AddTool(mcp.NewTool("craft_inspect",
		mcp.WithDescription("Read the app's current document: pages/slides, objects with their ids and boxes, selection, file path. Do this before editing."),
		mcp.WithString("app", mcp.Required(), mcp.Description("App id.")),
	), b.handleInspect)

	s.AddTool(mcp.NewTool("craft_run",
		mcp.WithDescription("Execute one command in an app. The command id comes from that app's own command table (see craft_commands). One command per call; each is a single undo step for the user."),
		mcp.WithString("app", mcp.Required(), mcp.Description("App id.")),
		mcp.WithString("command", mcp.Required(), mcp.Description("Command id, e.g. slide.new or shape.insert.")),
		mcp.WithAny("params", mcp.Description("Command parameters as a JSON object. Shape depends on the command.")),
	), b.handleRun)

	s.AddTool(mcp.NewTool("craft_commands",
		mcp.WithDescription("Search an app's command table. Returns id, label, menu path, shortcut and the parameter documentation for each match."),
		mcp.WithString("app", mcp.Required(), mcp.Description("App id.")),
		mcp.WithString("query", mcp.Description("Substring to match against command id, label or menu. Empty returns the most common groups.")),
		mcp.WithNumber("limit", mcp.Description("Maximum results. Default 40.")),
	), b.handleCommands)

	s.AddTool(mcp.NewTool("craft_screenshot",
		mcp.WithDescription("Render a PNG of the app window to disk and return its path. Use it to check what the user sees after a change."),
		mcp.WithString("app", mcp.Required(), mcp.Description("App id.")),
		mcp.WithString("path", mcp.Description("Absolute path for the PNG. Defaults to a file in the agent data directory.")),
	), b.handleScreenshot)

	s.AddTool(mcp.NewTool("craft_close",
		mcp.WithDescription("Disconnect from an app. If this agent launched the process, it is terminated."),
		mcp.WithString("app", mcp.Required(), mcp.Description("App id.")),
	), b.handleClose)
}

// ---- handlers ----

func (b *bridge) handleApps(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	allowed := map[string]bool{}
	for _, a := range b.opts.Apps {
		allowed[a.ID] = true
	}
	statuses := b.opts.Host.Status()
	filtered := make([]host.Status, 0, len(statuses))
	for _, st := range statuses {
		if allowed[st.App.ID] {
			filtered = append(filtered, st)
		}
	}
	return mcp.NewToolResultJSON(map[string]any{
		"apps": filtered,
		"count": map[string]int{
			"total":     len(filtered),
			"installed": countInstalled(filtered),
			"connected": countConnected(filtered),
		},
	})
}

func (b *bridge) handleOpen(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	app, err := b.resolveApp(req)
	if err != nil {
		return errResult(err), nil
	}
	if req.GetBool("relaunch", false) {
		b.opts.Host.Stop(app.ID)
		if err := b.opts.Host.Launch(ctx, app.ID); err != nil {
			return errResult(err), nil
		}
	}
	if _, err := b.opts.Host.Ensure(ctx, app.ID); err != nil {
		return errResult(err), nil
	}
	client := b.opts.Host.Client(app.ID)
	if client == nil {
		return errResult(fmt.Errorf("%s 已连接但通道不可用", app.Name)), nil
	}
	commands, cmdErr := client.Commands(ctx)
	out := map[string]any{
		"app":      app.ID,
		"name":     app.Name,
		"endpoint": client.Endpoint().String(),
		"control":  app.DescribeControl(),
	}
	if cmdErr == nil {
		out["commandCount"] = len(commands)
		out["groups"] = groupSummary(commands)
	} else {
		out["commandError"] = cmdErr.Error()
	}
	return mcp.NewToolResultJSON(out)
}

func (b *bridge) handleInspect(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	client, err := b.clientFor(ctx, req)
	if err != nil {
		return errResult(err), nil
	}
	raw, err := client.Document(ctx)
	if err != nil {
		return errResult(err), nil
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return mcp.NewToolResultText(string(raw)), nil
	}
	// 结构本身不带文档身份：控制通道没有 document 参数，只有 file.info 知道我在哪一份里。
	// 这里把它并进结果，调用方才不会把另一份文档的内容当成这一份。
	if doc, err := b.readDoc(ctx, client); err == nil {
		out["document"] = doc
		if anchored, ok := b.anchorValue(); ok {
			out["anchored"] = anchored
			out["drifted"] = !anchored.same(doc)
		}
	}
	return mcp.NewToolResultJSON(out)
}

func (b *bridge) handleRun(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	app, err := b.resolveApp(req)
	if err != nil {
		return errResult(err), nil
	}
	client, err := b.opts.Host.Ensure(ctx, app.ID)
	if err != nil {
		return errResult(err), nil
	}
	command := strings.TrimSpace(req.GetString("command", ""))
	if command == "" {
		return errResult(fmt.Errorf("缺少 command")), nil
	}
	params := req.GetArguments()["params"]

	// 文档级命令本身就是「我要换一份文档」的意思：执行后重锚，而不是拦下来。
	docCommand := command == cmdFileNew || command == cmdWindowNext || command == cmdFileClose
	if !docCommand && !readOnlyCommand(command) {
		// 写入前守卫：锚定的文档变了就不发命令，避免安静地写进另一份。
		if _, err := b.guardDoc(ctx, client); err != nil {
			b.record(app.ID, "command", command, false, err.Error(), 0)
			return errResult(err), nil
		}
	}

	start := time.Now()
	raw, err := client.Execute(ctx, command, params)
	elapsed := time.Since(start)
	if err != nil {
		b.record(app.ID, "command", command, false, err.Error(), elapsed)
		return errResult(err), nil
	}
	b.record(app.ID, "command", command, true, summarize(params), elapsed)

	doc, docErr := b.readDoc(ctx, client)
	if docErr == nil && docCommand {
		b.anchorNow(doc)
	}
	payload := map[string]any{"app": app.ID, "command": command}
	if docErr == nil {
		payload["document"] = doc
	}
	if len(raw) > 0 {
		payload["result"] = json.RawMessage(raw)
	} else {
		payload["ok"] = true
	}
	return mcp.NewToolResultJSON(payload)
}

// readOnlyCommand 判断一条原始命令是否只读。只读命令不受文档守卫限制：
// 用户切了文档之后，agent 正需要靠 inspect / info 去发现这件事。
func readOnlyCommand(id string) bool {
	for _, suffix := range []string{".inspect", ".get", ".state", ".list", ".info", ".render", ".templates", ".commands", ".query", ".search"} {
		if strings.HasSuffix(id, suffix) {
			return true
		}
	}
	switch id {
	case "engine.commands", "document.inspect", "app.qat", "app.links", "commands.list":
		return true
	}
	return false
}

func (b *bridge) handleCommands(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	client, err := b.clientFor(ctx, req)
	if err != nil {
		return errResult(err), nil
	}
	commands, err := client.Commands(ctx)
	if err != nil {
		return errResult(err), nil
	}
	query := strings.ToLower(strings.TrimSpace(req.GetString("query", "")))
	limit := req.GetInt("limit", 40)
	if limit <= 0 {
		limit = 40
	}
	matches := make([]controlCommand, 0, limit)
	for _, c := range commands {
		// 不过滤 enabled：它是运行时上下文（没有文本选区时 format.* 就是 false），
		// 不是「这条命令不存在」。过滤掉会让模型永远搜不到格式类命令。
		if query != "" && !matchesQuery(c, query) {
			continue
		}
		matches = append(matches, c)
		if len(matches) >= limit {
			break
		}
	}
	return mcp.NewToolResultJSON(map[string]any{
		"total":   len(commands),
		"query":   query,
		"results": matches,
		"groups":  groupSummary(commands),
	})
}

func (b *bridge) handleScreenshot(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	app, err := b.resolveApp(req)
	if err != nil {
		return errResult(err), nil
	}
	client, err := b.opts.Host.Ensure(ctx, app.ID)
	if err != nil {
		return errResult(err), nil
	}
	path := strings.TrimSpace(req.GetString("path", ""))
	if path == "" {
		path = filepath.Join(os.TempDir(), "craft-agent-shot.png")
	}
	start := time.Now()
	raw, err := client.Screenshot(ctx, path)
	elapsed := time.Since(start)
	if err != nil {
		b.record(app.ID, "command", "ui.screenshot", false, err.Error(), elapsed)
		return errResult(err), nil
	}
	b.record(app.ID, "command", "ui.screenshot", true, path, elapsed)
	return mcp.NewToolResultText(string(raw)), nil
}

func (b *bridge) handleClose(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	app, err := b.resolveApp(req)
	if err != nil {
		return errResult(err), nil
	}
	b.opts.Host.Stop(app.ID)
	return mcp.NewToolResultJSON(map[string]any{"app": app.ID, "closed": true})
}

// ---- 工具helpers ----

// resolveApp 解析 app 参数。只配置了一个应用时允许省略。
func (b *bridge) resolveApp(req mcp.CallToolRequest) (craft.App, error) {
	id := strings.ToLower(strings.TrimSpace(req.GetString("app", "")))
	if id == "" {
		if len(b.opts.Apps) == 1 {
			return b.opts.Apps[0], nil
		}
		return craft.App{}, fmt.Errorf("缺少 app；可选值: %s", b.appIDs())
	}
	for _, a := range b.opts.Apps {
		if a.ID == id {
			return a, nil
		}
	}
	return craft.App{}, fmt.Errorf("本会话未启用应用 %q；可选值: %s", id, b.appIDs())
}

func (b *bridge) clientFor(ctx context.Context, req mcp.CallToolRequest) (*controlClient, error) {
	app, err := b.resolveApp(req)
	if err != nil {
		return nil, err
	}
	return b.opts.Host.Ensure(ctx, app.ID)
}

func (b *bridge) appIDs() string {
	ids := make([]string, 0, len(b.opts.Apps))
	for _, a := range b.opts.Apps {
		ids = append(ids, a.ID)
	}
	return strings.Join(ids, ", ")
}

// record 把一次动作写进跨进程流水。写失败不影响调用本身。
func (b *bridge) record(appID, kind, command string, ok bool, detail string, elapsed time.Duration) {
	b.opts.Journal.Append(journal.Entry{
		Source:   "mcp",
		Session:  b.opts.Session,
		AppID:    appID,
		Kind:     kind,
		Command:  command,
		OK:       ok,
		Detail:   detail,
		Duration: elapsed.Milliseconds(),
	})
}

func appNames(apps []craft.App) []string {
	names := make([]string, 0, len(apps))
	for _, a := range apps {
		names = append(names, a.Name)
	}
	return names
}

// summarize 把命令参数压成一行，供流水展示；太长的截断。
func summarize(params any) string {
	if params == nil {
		return ""
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return ""
	}
	s := string(raw)
	if s == "{}" || s == "null" {
		return ""
	}
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

func errResult(err error) *mcp.CallToolResult {
	msg := err.Error()
	if hint := hintFor(msg); hint != "" {
		msg += "\n" + hint
	}
	return mcp.NewToolResultError(msg)
}

// hintFor 把通道的含糊报错翻译成「下一步该做什么」。
// 这些原始错误都是实测碰到的：报错本身不区分「没选区」和「焦点在缩略图面板」，
// agent 只能反复试；补一句提示能省掉整轮试错。
func hintFor(msg string) string {
	switch {
	case strings.Contains(msg, "select text or a shape"):
		return "提示：格式命令作用于当前文本选区。craft_run 只发一条命令、不会替你建选区；DeckCraft 上改用 deckcraft_style（自动 select → text.edit → selectAll → 套格式）；其他应用只能自己先发 edit.select → text.edit → text.selectAll 建立选区。"
	case strings.Contains(msg, "no presentation open"):
		return "提示：还没有打开的文档。DeckCraft 上用 deckcraft_page action=add 建页；其他应用用 craft_run 执行 file.new。"
	case strings.Contains(msg, "not available right now: no slides"):
		return "提示：文档里还没有页。DeckCraft 上用 deckcraft_page action=add。"
	case strings.HasPrefix(msg, "engine.execute 被拒绝: no shape"):
		return "提示：这个形状不在当前页。id 只在它所在的页有效，用 craft_inspect 先看清页号，DeckCraft 上再用 deckcraft_page 切页。"
	}
	return ""
}

func countInstalled(list []host.Status) int {
	n := 0
	for _, s := range list {
		if s.Installed {
			n++
		}
	}
	return n
}

func countConnected(list []host.Status) int {
	n := 0
	for _, s := range list {
		if s.Connected {
			n++
		}
	}
	return n
}

func matchesQuery(c controlCommand, q string) bool {
	if strings.Contains(strings.ToLower(c.ID), q) ||
		strings.Contains(strings.ToLower(c.Label), q) {
		return true
	}
	for _, m := range c.Menu {
		if strings.Contains(strings.ToLower(m), q) {
			return true
		}
	}
	return false
}

// groupSummary 统计每个前缀有多少条命令，给模型一个「这个应用能干什么」的粗视图。
func groupSummary(commands []controlCommand) map[string]int {
	groups := map[string]int{}
	for _, c := range commands {
		groups[groupOf(c.ID)]++
	}
	return groups
}

func groupOf(id string) string {
	if i := strings.IndexByte(id, '.'); i > 0 {
		return id[:i]
	}
	return id
}
