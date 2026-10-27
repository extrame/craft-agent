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

// 这个文件是「封装层」：把控制通道里那些必须靠隐式选区、必须按固定顺序发的命令，
// 收敛成一次工具调用。
//
// 为什么需要它：实测 DeckCraft 0.11.1 的 format.* 一族没有 ids 参数，只作用于当前选区；
// 而选区又不会被上一条命令保留。于是「给一个文本框设字体」这件事，在裸通道上要发：
//
//	edit.select → text.edit → text.selectAll → format.fontDialog → format.align → text.exit
//
// 六条，且每条都可能静默失败（跨页 edit.select 返回 {"selected":[]} 而不是报错）。
// 上面这些命令 id 是 craft 套件控制通道的公共词汇，不是给某一个应用写死的适配：
// 用之前先向应用要命令表核对，缺哪条就明确告诉模型「这个应用没有，改用 craft_run」。
const (
	cmdSelect        = "edit.select"
	cmdTextEdit      = "text.edit"
	cmdTextExit      = "text.exit"
	cmdTextSelectAll = "text.selectAll"
	cmdTextSet       = "text.set"
	cmdTextGet       = "text.get"
	cmdTextBox       = "insert.textBox"
	cmdShapeBounds   = "shape.setBounds"
	cmdShapeInspect  = "shape.inspect"
	cmdFontDialog    = "format.fontDialog"
	cmdAlign         = "format.align"
	cmdParagraph     = "format.paragraph"
	cmdAnchor        = "format.anchor"
	cmdBullets       = "format.bullets"
	cmdFormatState   = "format.state"
	cmdSlideGo       = "slide.go"
	cmdSlideNew      = "slide.new"
	cmdSlideDelete   = "slide.delete"
	cmdSlideDup      = "slide.duplicate"
	cmdSlideMove     = "slide.move"
	cmdSlideInspect  = "slide.inspect"
	cmdDocInspect    = "document.inspect"
)

func (b *bridge) registerComposeTools(s *server.MCPServer) {
	s.AddTool(mcp.NewTool("deckcraft_page",
		mcp.WithDescription("DeckCraft only. Page/slide operations with the current page guaranteed afterwards. action=add creates a page (and moves to it), goto switches, delete/duplicate/move adjust the deck. Returns the deck outline so you never have to guess where the caret is. Prefer this over raw slide.new: slide.new does not promise to leave you on the new page in every build."),
		mcp.WithString("app", mcp.Required(), mcp.Description("App id.")),
		mcp.WithString("action", mcp.Required(), mcp.Description("add | goto | delete | duplicate | move")),
		mcp.WithNumber("index", mcp.Description("Target page index for goto/delete/duplicate, or the source index for move. For add: omit to append at the end.")),
		mcp.WithNumber("to", mcp.Description("Destination index, for action=move only.")),
		mcp.WithString("layout", mcp.Description("Layout for add: title | titleAndContent | titleOnly | sectionHeader | blank | twoContent | comparison | contentWithCaption | pictureWithCaption.")),
		mcp.WithString("title", mcp.Description("Optional title text for add.")),
		mcp.WithString("body", mcp.Description("Optional body text for add.")),
	), b.handlePage)

	s.AddTool(mcp.NewTool("deckcraft_text",
		mcp.WithDescription("DeckCraft only. Create or restyle a text box in ONE call. This wraps the six-command dance the raw channel needs (select → text.edit → selectAll → format → align → exit) and verifies the result with the app's own format.state plus a page check (verified.onSlide). Give 'id' to restyle an existing text shape, or omit it to insert a new one. Only pass the style keys you actually want changed. Refused when the active document is not the one this session anchored — see craft_doc."),
		mcp.WithString("app", mcp.Required(), mcp.Description("App id.")),
		mcp.WithNumber("id", mcp.Description("Existing text shape id. Omit to insert a new text box.")),
		mcp.WithString("text", mcp.Description("Text content. Required when inserting; when updating, omit to keep the current text.")),
		mcp.WithNumber("slide", mcp.Description("Page index for the new box. Omit to use the current page. Ignored when id is given (the shape's own page is used).")),
		mcp.WithAny("rect", mcp.Description("Box as [x, y, w, h] in slide points (a 960x540 deck). Omit to let the app auto-place and auto-size.")),
		mcp.WithString("font", mcp.Description("Font family, e.g. 'Helvetica Neue', 'PingFang SC'.")),
		mcp.WithNumber("size", mcp.Description("Font size in points.")),
		mcp.WithBoolean("bold", mcp.Description("Bold on/off.")),
		mcp.WithBoolean("italic", mcp.Description("Italic on/off.")),
		mcp.WithBoolean("underline", mcp.Description("Underline on/off.")),
		mcp.WithString("color", mcp.Description("Text colour: #RRGGBB, accent1..6, tx1/tx2, bg1/bg2.")),
		mcp.WithString("align", mcp.Description("left | center | right | justify.")),
		mcp.WithString("anchor", mcp.Description("Vertical anchor inside the box: top | middle | bottom.")),
		mcp.WithNumber("lineSpacing", mcp.Description("Line spacing as a multiple, e.g. 1.5.")),
		mcp.WithNumber("spaceAfter", mcp.Description("Space after paragraph, in points.")),
		mcp.WithBoolean("bullets", mcp.Description("Bulleted list on/off.")),
		mcp.WithString("autofit", mcp.Description("none | shrink | resize. shrink keeps the box size and scales text down, resize grows the box.")),
	), b.handleText)

	s.AddTool(mcp.NewTool("deckcraft_style",
		mcp.WithDescription("DeckCraft only. Restyle existing text shapes by id — one call for many shapes. It finds each shape's page, switches there, builds a real text selection and applies the formatting, then reports the formatting the app actually applied (from format.state) plus any shape that failed. Use this instead of a chain of format.* commands, which need a text selection you cannot see."),
		mcp.WithString("app", mcp.Required(), mcp.Description("App id.")),
		mcp.WithAny("ids", mcp.Required(), mcp.Description("Shape ids to restyle, as an array of integers. Cross-page ids are allowed; each is handled on its own page.")),
		mcp.WithString("font", mcp.Description("Font family.")),
		mcp.WithNumber("size", mcp.Description("Font size in points.")),
		mcp.WithBoolean("bold", mcp.Description("Bold on/off.")),
		mcp.WithBoolean("italic", mcp.Description("Italic on/off.")),
		mcp.WithBoolean("underline", mcp.Description("Underline on/off.")),
		mcp.WithString("color", mcp.Description("Text colour: #RRGGBB or a theme reference.")),
		mcp.WithString("align", mcp.Description("left | center | right | justify.")),
		mcp.WithString("anchor", mcp.Description("Vertical anchor: top | middle | bottom.")),
		mcp.WithNumber("lineSpacing", mcp.Description("Line spacing as a multiple.")),
		mcp.WithNumber("spaceAfter", mcp.Description("Space after paragraph, in points.")),
		mcp.WithBoolean("bullets", mcp.Description("Bulleted list on/off.")),
	), b.handleStyle)

	s.AddTool(mcp.NewTool("craft_batch",
		mcp.WithDescription("Run several commands in one call, in order, and get a per-step result. This is the fix for 'one command per HTTP round trip': a whole slide's worth of work fits in one call. Pass slide to pin the page first — insert.* has no page parameter and silently follows the active page. Steps stop at the first failure by default so you never half-apply a layout silently. Each step is still its own undo step for the user."),
		mcp.WithString("app", mcp.Required(), mcp.Description("App id.")),
		mcp.WithAny("steps", mcp.Required(), mcp.Description("Array of {command, params}. command is a command id from craft_commands; params is that command's parameter object (omit or {} for none).")),
		mcp.WithNumber("slide", mcp.Description("Pin this page before the steps run. Use it whenever the steps contain inserts: insert.* has no page parameter and silently follows the active page.")),
		mcp.WithBoolean("stopOnError", mcp.Description("Stop at the first failing step. Default true; remaining steps are then reported as skipped.")),
		mcp.WithBoolean("verify", mcp.Description("After the steps, read the document and include the deck outline (page count, current page, shapes per page). Default false.")),
	), b.handleBatch)
}

/* ---------------- 页面 ---------------- */

func (b *bridge) handlePage(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	app, c, err := b.openReq(ctx, req)
	if err != nil {
		return errResult(err), nil
	}
	if err := deckOnly(app, "deckcraft_page"); err != nil {
		return errResult(err), nil
	}
	doc, _, err := b.guardDoc(ctx, app, c)
	if err != nil {
		return errResult(err), nil
	}
	action := strings.ToLower(strings.TrimSpace(req.GetString("action", "add")))
	index := req.GetInt("index", -1)

	start := time.Now()
	note := action
	var applied any

	switch action {
	case "add":
		if err := b.requireCommands(ctx, app, c, cmdSlideNew); err != nil {
			return errResult(err), nil
		}
		params := map[string]any{}
		if v, ok := argString(req, "layout"); ok && v != "" {
			params["layout"] = v
		}
		if v, ok := argString(req, "title"); ok && v != "" {
			params["title"] = v
		}
		if v, ok := argString(req, "body"); ok && v != "" {
			params["body"] = v
		}
		if index >= 0 {
			params["at"] = index
		}
		raw, err := b.exec(ctx, c, cmdSlideNew, params)
		if err != nil {
			b.record(app.ID, "command", "deckcraft_page:add", false, err.Error(), time.Since(start))
			return errResult(err), nil
		}
		created := intField(raw, "index")
		// 不信任 slide.new 会停在新页：显式切一次，后面 insert 才落到对的地方。
		if err := b.goSlide(ctx, c, created); err != nil {
			return errResult(fmt.Errorf("新页已创建（第 %d 页）但切页失败: %w", created, err)), nil
		}
		applied = map[string]any{"createdIndex": created, "createdId": intField(raw, "id")}
		note = fmt.Sprintf("add #%d", created)

	case "goto", "delete", "duplicate", "move":
		if index < 0 {
			return errResult(fmt.Errorf("action=%s 需要 index", action)), nil
		}
		id := cmdSlideGo
		params := map[string]any{"index": index}
		switch action {
		case "delete":
			id = cmdSlideDelete
		case "duplicate":
			id = cmdSlideDup
		case "move":
			id = cmdSlideMove
			to := req.GetInt("to", -1)
			if to < 0 {
				return errResult(fmt.Errorf("action=move 需要 to")), nil
			}
			params = map[string]any{"from": index, "to": to}
		}
		if err := b.requireCommands(ctx, app, c, id); err != nil {
			return errResult(err), nil
		}
		raw, err := b.exec(ctx, c, id, params)
		if err != nil {
			b.record(app.ID, "command", "deckcraft_page:"+action, false, err.Error(), time.Since(start))
			return errResult(err), nil
		}
		applied = decodeObject(raw)
		// 复制/移动会改变当前页，读完大纲再交给调用方。
		note = fmt.Sprintf("%s #%d", action, index)

	default:
		return errResult(fmt.Errorf("未知 action %q；可选 add|goto|delete|duplicate|move", action)), nil
	}

	outline, err := b.outline(ctx, c)
	if err != nil {
		return errResult(err), nil
	}
	// 加/删页会改变页数：身份里的 slides 要用操作之后的值，否则调用方看到的是旧快照。
	if fresh, err := b.readDoc(ctx, c); err == nil {
		doc = fresh
	}
	b.record(app.ID, "command", "deckcraft_page:"+note, true, "", time.Since(start))
	return mcp.NewToolResultJSON(map[string]any{
		"app":      app.ID,
		"document": doc,
		"applied":  applied,
		"deck":     outline,
	})
}

/* ---------------- 文本 ---------------- */

func (b *bridge) handleText(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	app, c, err := b.openReq(ctx, req)
	if err != nil {
		return errResult(err), nil
	}
	if err := deckOnly(app, "deckcraft_text"); err != nil {
		return errResult(err), nil
	}
	if err := b.requireCommands(ctx, app, c, cmdTextBox, cmdSelect, cmdTextEdit, cmdTextSelectAll, cmdFontDialog, cmdFormatState); err != nil {
		return errResult(err), nil
	}
	doc, _, err := b.guardDoc(ctx, app, c)
	if err != nil {
		return errResult(err), nil
	}

	start := time.Now()
	text, _ := argString(req, "text")
	id := req.GetInt("id", 0)
	rect, hasRect := argFloat4(req, "rect")
	slide := -1

	if id > 0 {
		// 已有形状：先找到它在哪一页——跨页的 id 命令会静默失败。
		slide, err = b.locateShape(ctx, c, id)
		if err != nil {
			return errResult(err), nil
		}
		if err := b.goSlide(ctx, c, slide); err != nil {
			return errResult(err), nil
		}
		if text != "" {
			if _, err := b.exec(ctx, c, cmdTextSet, map[string]any{"id": id, "text": text}); err != nil {
				b.record(app.ID, "command", "deckcraft_text", false, err.Error(), time.Since(start))
				return errResult(err), nil
			}
		}
	} else {
		if strings.TrimSpace(text) == "" {
			return errResult(fmt.Errorf("新建文本框需要 text；改已有文本框请传 id")), nil
		}
		sl := req.GetInt("slide", -1)
		if sl >= 0 {
			if err := b.goSlide(ctx, c, sl); err != nil {
				return errResult(err), nil
			}
			slide = sl
		} else {
			cur, _, err := b.currentSlide(ctx, c)
			if err != nil {
				return errResult(err), nil
			}
			slide = cur
		}
		params := map[string]any{"text": text}
		if hasRect {
			params["rect"] = rect
		}
		raw, err := b.exec(ctx, c, cmdTextBox, params)
		if err != nil {
			b.record(app.ID, "command", "deckcraft_text", false, err.Error(), time.Since(start))
			return errResult(err), nil
		}
		id = intField(raw, "id")
		if id == 0 {
			return errResult(fmt.Errorf("insert.textBox 没有返回形状 id，无法继续排版")), nil
		}
	}

	state, err := b.styleShape(ctx, c, id, styleFromArgs(req))
	if err != nil {
		b.record(app.ID, "command", "deckcraft_text", false, err.Error(), time.Since(start))
		return errResult(fmt.Errorf("形状 %d 已建立，但排版失败: %w", id, err)), nil
	}

	// 显式给了尺寸就按尺寸定住：insert.textBox 的高度是按文本算出来的，
	// 不锁死的话多行文本会把框撑开，版面对不上。
	if hasRect && rect[3] > 0 {
		params := map[string]any{"x": rect[0], "y": rect[1], "w": rect[2], "h": rect[3], "ids": []int{id}}
		if _, err := b.exec(ctx, c, cmdShapeBounds, params); err != nil {
			return errResult(fmt.Errorf("排版完成，但定尺寸失败: %w", err)), nil
		}
	}
	if mode, ok := argString(req, "autofit"); ok && mode != "" {
		_, _ = b.exec(ctx, c, "format.autofit", map[string]any{"mode": mode})
	}

	out := map[string]any{
		"app":      app.ID,
		"document": doc,
		"id":       id,
		"slide":    slide,
		"applied":  state, // 来自 format.state：应用自己报的生效值，不是我们以为的值
	}
	// 形状到底落在哪一页，只能回读那一页的形状列表来证明。
	// insert.* 没有 slide 参数，「当前页」是隐式的，这一步就是它唯一的凭据。
	out["verified"] = map[string]any{"slide": slide, "onSlide": b.slideHasShape(ctx, c, slide, id)}
	if got, err := b.exec(ctx, c, cmdTextGet, map[string]any{"id": id}); err == nil {
		out["text"] = decodeObject(got)["text"]
	}
	if box, err := b.exec(ctx, c, cmdShapeInspect, map[string]any{"id": id}); err == nil {
		out["box"] = decodeObject(box)["box"]
	}
	b.record(app.ID, "command", fmt.Sprintf("deckcraft_text #%d", id), true, summarize(map[string]any{"slide": slide, "text": text}), time.Since(start))
	return mcp.NewToolResultJSON(out)
}

func (b *bridge) handleStyle(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	app, c, err := b.openReq(ctx, req)
	if err != nil {
		return errResult(err), nil
	}
	if err := deckOnly(app, "deckcraft_style"); err != nil {
		return errResult(err), nil
	}
	if err := b.requireCommands(ctx, app, c, cmdSelect, cmdTextEdit, cmdTextSelectAll, cmdFontDialog, cmdFormatState); err != nil {
		return errResult(err), nil
	}
	ids := req.GetIntSlice("ids", nil)
	if len(ids) == 0 {
		return errResult(fmt.Errorf("需要 ids（形状 id 数组）")), nil
	}
	doc, _, err := b.guardDoc(ctx, app, c)
	if err != nil {
		return errResult(err), nil
	}

	start := time.Now()
	style := styleFromArgs(req)
	done := make([]map[string]any, 0, len(ids))
	failed := make([]map[string]any, 0)

	for _, id := range ids {
		slide, err := b.locateShape(ctx, c, id)
		if err != nil {
			failed = append(failed, map[string]any{"id": id, "error": err.Error()})
			continue
		}
		if err := b.goSlide(ctx, c, slide); err != nil {
			failed = append(failed, map[string]any{"id": id, "slide": slide, "error": err.Error()})
			continue
		}
		state, err := b.styleShape(ctx, c, id, style)
		if err != nil {
			failed = append(failed, map[string]any{"id": id, "slide": slide, "error": err.Error()})
			continue
		}
		done = append(done, map[string]any{"id": id, "slide": slide, "applied": state})
	}

	b.record(app.ID, "command", fmt.Sprintf("deckcraft_style n=%d", len(ids)), len(failed) == 0,
		fmt.Sprintf("ok=%d failed=%d", len(done), len(failed)), time.Since(start))
	return mcp.NewToolResultJSON(map[string]any{
		"app":      app.ID,
		"document": doc,
		"styled":   done,
		"failed":   failed,
		"summary":  fmt.Sprintf("%d 个形状已排版，%d 个失败", len(done), len(failed)),
	})
}

/* ---------------- 批量 ---------------- */

type batchStep struct {
	Command string          `json:"command"`
	Params  json.RawMessage `json:"params"`
}

func (b *bridge) handleBatch(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	app, c, err := b.openReq(ctx, req)
	if err != nil {
		return errResult(err), nil
	}
	rawSteps, ok := req.GetArguments()["steps"]
	if !ok {
		return errResult(fmt.Errorf("需要 steps 数组")), nil
	}
	encoded, err := json.Marshal(rawSteps)
	if err != nil {
		return errResult(fmt.Errorf("steps 无法解析: %w", err)), nil
	}
	var steps []batchStep
	if err := json.Unmarshal(encoded, &steps); err != nil {
		return errResult(fmt.Errorf("steps 必须是 [{command, params}] 数组: %w", err)), nil
	}
	if len(steps) == 0 {
		return errResult(fmt.Errorf("steps 为空")), nil
	}
	var doc docIdentity
	var guardNotes []string
	// craft_batch 是通用工具，会用在没有文档层的应用上（FigCraft 没有 file.info）。
	// 前提不成立时跳过守卫并注明，而不是让批次死在 unknown command 上。
	docCapable := b.hasDocModel(ctx, app, c)
	if docCapable {
		doc, guardNotes, err = b.guardDoc(ctx, app, c)
		if err != nil {
			return errResult(err), nil
		}
	}
	// 批次里有 insert.* 时，「当前页」是隐式的：调用方能钉页就钉住，别让整批落到别的页。
	if slide := req.GetInt("slide", -1); slide >= 0 {
		if err := b.goSlide(ctx, c, slide); err != nil {
			return errResult(err), nil
		}
	}

	// 批次里若混入文档级命令，执行完的活动文档会变：记下来，别让调用方以为还在原来那份。
	docCommands := make([]map[string]any, 0, 2)
	for i, step := range steps {
		switch strings.TrimSpace(step.Command) {
		case cmdFileNew, cmdFileClose, cmdWindowNext, "file.open", "file.adopt", "file.revert":
			docCommands = append(docCommands, map[string]any{"index": i, "command": step.Command})
		}
	}

	stopOnError := req.GetBool("stopOnError", true)
	start := time.Now()
	results := make([]map[string]any, 0, len(steps))
	failed := 0
	stopped := false

	for i, step := range steps {
		cmd := strings.TrimSpace(step.Command)
		if cmd == "" {
			failed++
			results = append(results, map[string]any{"index": i, "ok": false, "error": "缺少 command"})
			if stopOnError {
				stopped = true
				break
			}
			continue
		}
		var params any
		if len(step.Params) > 0 && string(step.Params) != "null" {
			_ = json.Unmarshal(step.Params, &params)
		}
		began := time.Now()
		out, err := b.exec(ctx, c, cmd, params)
		entry := map[string]any{"index": i, "command": cmd, "ms": time.Since(began).Milliseconds()}
		if err != nil {
			failed++
			entry["ok"] = false
			entry["error"] = err.Error()
			results = append(results, entry)
			if stopOnError {
				stopped = true
				// 剩下的步骤如实标成未执行，而不是假装跳过得很干净。
				for j := i + 1; j < len(steps); j++ {
					results = append(results, map[string]any{"index": j, "command": steps[j].Command, "ok": false, "skipped": true})
				}
				break
			}
			continue
		}
		entry["ok"] = true
		if len(out) > 0 {
			entry["result"] = json.RawMessage(out)
		}
		results = append(results, entry)
	}

	payload := map[string]any{
		"app":       app.ID,
		"ok":        failed == 0,
		"steps":     results,
		"failed":    failed,
		"total":     len(steps),
		"stopped":   stopped,
		"elapsedMs": time.Since(start).Milliseconds(),
	}
	if docCapable {
		payload["document"] = doc
		if len(guardNotes) > 0 {
			payload["guardNotes"] = guardNotes
		}
	} else {
		payload["document"] = nil
		payload["guard"] = "none"
		payload["note"] = docGuardNote
	}
	if len(docCommands) > 0 && docCapable {
		payload["documentCommands"] = docCommands
		after, docErr := b.readDoc(ctx, c)
		if docErr == nil {
			if !after.same(doc) {
				payload["documentChanged"] = after
				b.anchorNow(app, after)
			} else {
				payload["document"] = after
			}
		}
	}
	if req.GetBool("verify", false) {
		if outline, err := b.outline(ctx, c); err == nil {
			payload["deck"] = outline
		}
		// verify 的语义是「跑完给我一个可信的状态」，所以身份也重新读一次。
		if fresh, err := b.readDoc(ctx, c); err == nil {
			payload["document"] = fresh
		}
	}
	b.record(app.ID, "command", fmt.Sprintf("craft_batch n=%d", len(steps)), failed == 0,
		fmt.Sprintf("失败 %d", failed), time.Since(start))
	return mcp.NewToolResultJSON(payload)
}

/* ---------------- 内部 ---------------- */

func (b *bridge) openReq(ctx context.Context, req mcp.CallToolRequest) (craft.App, *control.Client, error) {
	app, err := b.resolveApp(req)
	if err != nil {
		return craft.App{}, nil, err
	}
	client, err := b.opts.Host.Ensure(ctx, app.ID)
	if err != nil {
		return craft.App{}, nil, err
	}
	return app, client, nil
}

func (b *bridge) exec(ctx context.Context, c *control.Client, command string, params any) (json.RawMessage, error) {
	return c.Execute(ctx, command, params)
}

// requireCommands 核对应用是否真的有这些命令。
// 包装层依赖固定语义的命令，但「有没有」由应用自己说了算 —— 缺了就明确拒绝，
// 不让模型在一条不存在的命令上反复试。
func (b *bridge) requireCommands(ctx context.Context, app craft.App, c *control.Client, ids ...string) error {
	set, ok := b.commandSet(ctx, app, c)
	if !ok {
		return nil // 拿不到命令表时放行，让真正的调用去暴露问题
	}
	missing := make([]string, 0, len(ids))
	for _, id := range ids {
		if !set[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("%s 的命令表里没有 %s，这个封装对它不适用；改用 craft_commands 查它自己的命令，再用 craft_run 逐条执行",
		app.Name, strings.Join(missing, "、"))
}

func (b *bridge) commandSet(ctx context.Context, app craft.App, c *control.Client) (map[string]bool, bool) {
	b.capMu.Lock()
	set, ok := b.caps[app.ID]
	b.capMu.Unlock()
	if ok {
		return set, len(set) > 0
	}
	commands, _, err := b.commands(ctx, app, c)
	if err != nil {
		return nil, false
	}
	set = make(map[string]bool, len(commands))
	for _, cmd := range commands {
		// 只看「有没有这条命令」，不看 enabled。
		// 实测 enabled 是运行时上下文：没有文本选区时 format.* 与 text.selectAll
		// 都报 false，但那不代表应用不支持它们。按 enabled 过滤会把能用的命令判死。
		set[cmd.ID] = true
	}
	b.capMu.Lock()
	b.caps[app.ID] = set
	b.capMu.Unlock()
	return set, true
}

// outline 是「现在文档长什么样」的紧凑视图：页数、当前页、每页的标题与形状数。
// 复合工具每次返回它，模型就不用为了确认落页再单独调一轮 craft_inspect。
func (b *bridge) outline(ctx context.Context, c *control.Client) (map[string]any, error) {
	raw, err := b.exec(ctx, c, cmdDocInspect, nil)
	if err != nil {
		return nil, err
	}
	doc := decodeObject(raw)
	cur := -1
	if sel, ok := doc["selection"].(map[string]any); ok {
		cur = toInt(sel["slide"])
	}
	pages := make([]map[string]any, 0)
	if list, ok := doc["slides"].([]any); ok {
		for _, item := range list {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			pages = append(pages, map[string]any{
				"index":  toInt(m["index"]),
				"id":     toInt(m["id"]),
				"title":  m["title"],
				"layout": m["layout"],
				"shapes": toInt(m["shapes"]),
			})
		}
	}
	return map[string]any{
		"current": cur,
		"count":   len(pages),
		"pages":   pages,
		"title":   doc["title"],
		"size":    doc["slideSize"],
	}, nil
}

func (b *bridge) currentSlide(ctx context.Context, c *control.Client) (int, map[string]any, error) {
	out, err := b.outline(ctx, c)
	if err != nil {
		return -1, nil, err
	}
	return toInt(out["current"]), out, nil
}

// locateShape 找出形状在哪一页。先把当前页试掉，避免每次都从第一页扫。
func (b *bridge) locateShape(ctx context.Context, c *control.Client, id int) (int, error) {
	doc, err := b.exec(ctx, c, cmdDocInspect, nil)
	if err != nil {
		return -1, err
	}
	d := decodeObject(doc)
	cur := -1
	if sel, ok := d["selection"].(map[string]any); ok {
		cur = toInt(sel["slide"])
	}
	total := len(toSlice(d["slides"]))

	if cur >= 0 && b.slideHasShape(ctx, c, cur, id) {
		return cur, nil
	}
	for i := 0; i < total; i++ {
		if i == cur {
			continue
		}
		if b.slideHasShape(ctx, c, i, id) {
			return i, nil
		}
	}
	return -1, fmt.Errorf("文档里没有形状 %d（可能已被删除）；先 craft_inspect 确认 id", id)
}

func (b *bridge) slideHasShape(ctx context.Context, c *control.Client, index, id int) bool {
	raw, err := b.exec(ctx, c, cmdSlideInspect, map[string]any{"index": index})
	if err != nil {
		return false
	}
	for _, item := range toSlice(decodeObject(raw)["shapes"]) {
		if m, ok := item.(map[string]any); ok && toInt(m["id"]) == id {
			return true
		}
	}
	return false
}

func (b *bridge) goSlide(ctx context.Context, c *control.Client, index int) error {
	cur, _, err := b.currentSlide(ctx, c)
	if err != nil {
		return err
	}
	if cur == index {
		return nil
	}
	if _, err := b.exec(ctx, c, cmdSlideGo, map[string]any{"index": index}); err != nil {
		return fmt.Errorf("切到第 %d 页失败: %w", index, err)
	}
	return nil
}

/* ---------------- 参数与 JSON 小工具 ---------------- */

// styleShape 给一个形状的文本套格式。
//
// 前置：形状必须在当前页（调用方负责切页）。style 只放「这次真的要改」的键，
// 不传的项一律不动。返回 format.state —— 应用自己报的生效值，这就是
// 「不静默成功」：拿不到就说拿不到，而不是返回 null 装作成功。
func (b *bridge) styleShape(ctx context.Context, c *control.Client, id int, style map[string]any) (map[string]any, error) {
	sel, err := b.exec(ctx, c, cmdSelect, map[string]any{"ids": []int{id}})
	if err != nil {
		return nil, err
	}
	// edit.select 跨页时不报错，只回空数组 —— 这里必须自己检查。
	if picked := intSlice(sel, "selected"); !containsInt(picked, id) {
		return nil, fmt.Errorf("选不中形状 %d（它不在当前页，或已被删除）；未做任何改动", id)
	}
	if _, err := b.exec(ctx, c, cmdTextEdit, map[string]any{"id": id}); err != nil {
		return nil, fmt.Errorf("无法进入文本编辑（形状 %d 可能不是文本框）: %w", id, err)
	}
	// 无论后面成败，都要退出编辑态，否则应用的焦点会卡在文本里。
	defer func() { _, _ = b.exec(context.WithoutCancel(ctx), c, cmdTextExit, nil) }()

	if _, err := b.exec(ctx, c, cmdTextSelectAll, nil); err != nil {
		return nil, err
	}
	if params := fontParams(style); len(params) > 0 {
		if _, err := b.exec(ctx, c, cmdFontDialog, params); err != nil {
			return nil, err
		}
	}
	if align, ok := style["align"]; ok {
		if _, err := b.exec(ctx, c, cmdAlign, map[string]any{"align": align}); err != nil {
			return nil, err
		}
	}
	if params := paragraphParams(style); len(params) > 0 {
		if _, err := b.exec(ctx, c, cmdParagraph, params); err != nil {
			return nil, err
		}
	}
	if anchor, ok := style["anchor"]; ok {
		if _, err := b.exec(ctx, c, cmdAnchor, map[string]any{"anchor": anchor}); err != nil {
			return nil, err
		}
	}
	if on, ok := style["bullets"]; ok {
		if _, err := b.exec(ctx, c, cmdBullets, map[string]any{"on": on}); err != nil {
			return nil, err
		}
	}

	state, err := b.exec(ctx, c, cmdFormatState, nil)
	if err != nil {
		return nil, fmt.Errorf("格式已发出但读回生效值失败: %w", err)
	}
	return decodeObject(state), nil
}

// styleFromArgs 只挑出这次传了的排版键。缺省的键不出现在 map 里，
// 这样 styleShape 就不会用零值去覆盖用户没要求改的属性。
func styleFromArgs(req mcp.CallToolRequest) map[string]any {
	style := map[string]any{}
	args := req.GetArguments()
	for _, key := range []string{"font", "size", "color", "bold", "italic", "underline", "align", "anchor", "lineSpacing", "spaceAfter", "spaceBefore", "bullets"} {
		if v, ok := args[key]; ok && v != nil {
			style[key] = v
		}
	}
	return style
}

// fontParams 把统一的排版参数翻成 format.fontDialog 的键名。
// 一次 fontDialog 就能设完字体/字号/加粗/斜体/下划线/颜色 —— 这是唯一一个
// 支持批量字符属性的命令，单发 format.font / format.size 只会更慢。
func fontParams(style map[string]any) map[string]any {
	params := map[string]any{}
	if v, ok := style["font"]; ok {
		params["family"] = v
	}
	if v, ok := style["size"]; ok {
		params["size"] = v
	}
	if v, ok := style["color"]; ok {
		params["color"] = v
	}
	if v, ok := style["bold"]; ok {
		params["bold"] = v
	}
	if v, ok := style["italic"]; ok {
		params["italic"] = v
	}
	if v, ok := style["underline"]; ok {
		if on, isBool := v.(bool); isBool {
			if on {
				params["underline"] = "sng"
			} else {
				params["underline"] = "none"
			}
		} else {
			params["underline"] = v
		}
	}
	return params
}

func paragraphParams(style map[string]any) map[string]any {
	params := map[string]any{}
	if v, ok := style["lineSpacing"]; ok {
		params["lineSpacing"] = v
	}
	if v, ok := style["spaceAfter"]; ok {
		params["spaceAfter"] = v
	}
	if v, ok := style["spaceBefore"]; ok {
		params["spaceBefore"] = v
	}
	return params
}

func argString(req mcp.CallToolRequest, key string) (string, bool) {
	v, ok := req.GetArguments()[key]
	if !ok || v == nil {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func argBool(req mcp.CallToolRequest, key string) (bool, bool) {
	v, ok := req.GetArguments()[key]
	if !ok || v == nil {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

func argFloat4(req mcp.CallToolRequest, key string) ([4]float64, bool) {
	var out [4]float64
	v, ok := req.GetArguments()[key]
	if !ok || v == nil {
		return out, false
	}
	list, ok := v.([]any)
	if !ok || len(list) != 4 {
		return out, false
	}
	for i := range out {
		f, ok := toFloat(list[i])
		if !ok {
			return out, false
		}
		out[i] = f
	}
	return out, true
}

func decodeObject(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return map[string]any{}
	}
	return m
}

func toSlice(v any) []any {
	list, _ := v.([]any)
	return list
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func toInt(v any) int {
	if f, ok := toFloat(v); ok {
		return int(f)
	}
	return 0
}

func intField(raw json.RawMessage, key string) int {
	return toInt(decodeObject(raw)[key])
}

func intSlice(raw json.RawMessage, key string) []int {
	out := make([]int, 0)
	for _, item := range toSlice(decodeObject(raw)[key]) {
		out = append(out, toInt(item))
	}
	return out
}

func containsInt(list []int, want int) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// deckOnly 保证以 deckcraft_ 开头的工具只作用于 DeckCraft。
//
// 名字里带 deckcraft 就得说到做到：幻灯片、页、文本框这套模型是 DeckCraft 的，
// 别的应用要么没有，要么没实测过。与其让它跑到一半、以一句 "unknown command"
// 或静默的空选区收场，不如现在就告诉模型该往哪走 —— craft_run 永远能兜底。
func deckOnly(app craft.App, tool string) error {
	if app.ID == craft.DeckCraftID {
		return nil
	}
	return fmt.Errorf("%s 只用于 DeckCraft（幻灯片模型），对 %s 不成立。请改用 craft_run 执行它自己的命令；不确定命令 id 时先 craft_commands 查表", tool, app.Name)
}
