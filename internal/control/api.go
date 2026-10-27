package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Command 是应用命令表里的一条。
//
// 字段来自三代应用的实测输出。**不同应用的编码不一样**，这里按最宽的那种解：
//
//	                DeckCraft        FigCraft          CADCraft        PdfCraft
//	menu            ["Format","Text"] ["Text/Adjust"]  ["File","New…"]  "File" / null
//	shortcut        "Cmd+B" / ""      "Cmd+Z" / null   "Ctrl+N"        23 条有，其余 null
//	enabled         bool              ——（给 undoable）  bool            bool
//	aliases         ——                ——                ["qnew"]        ——
//	params          字符串            字符串             字符串           字符串
//
// 解码必须逐字段兜底：一 张几百条的表，任何一条类型不符都会让整表解析失败，
// 界面上就变成「这个应用没有命令」。
type Command struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	Menu        MenuPath `json:"menu"`
	Params      string   `json:"params"`
	Shortcut    string   `json:"shortcut"`
	Aliases     []string `json:"aliases,omitempty"`
	Interactive bool     `json:"interactive,omitempty"`

	// Enabled 是「此刻能不能用」，是运行时上下文：没有文本选区时 format.* 就是 false。
	// 用指针是因为有的应用根本不报这个字段，那种情况和「明确不可用」不是一回事——
	// 解成 bool 会变成 597 条全 false，看起来像整个应用都残废了。
	Enabled *bool `json:"enabled,omitempty"`
	// Undoable 是 FigCraft 那类应用给的替代信息：这条命令是否进撤销栈。
	Undoable bool `json:"undoable,omitempty"`
}

// commandWire 是 JSON 层面的形状。字符串字段先收 RawMessage：
// 有的应用把 params 写成对象，直接解成 string 会连累整张表。
type commandWire struct {
	ID          string          `json:"id"`
	Label       json.RawMessage `json:"label"`
	Menu        MenuPath        `json:"menu"`
	Params      json.RawMessage `json:"params"`
	Shortcut    json.RawMessage `json:"shortcut"`
	Aliases     []string        `json:"aliases"`
	Interactive bool            `json:"interactive"`
	Enabled     *bool           `json:"enabled"`
	Undoable    bool            `json:"undoable"`
}

func (c *Command) UnmarshalJSON(b []byte) error {
	var w commandWire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	c.ID = w.ID
	c.Label = asText(w.Label)
	c.Menu = w.Menu
	c.Params = asText(w.Params)
	c.Shortcut = asText(w.Shortcut)
	c.Aliases = w.Aliases
	c.Interactive = w.Interactive
	c.Enabled = w.Enabled
	c.Undoable = w.Undoable
	return nil
}

// asText 把「本该是字符串」的字段尽量读成人能看的文本：
// null → 空串；字符串 → 原文；对象/数组 → 紧凑 JSON（总比丢掉强）。
func asText(raw json.RawMessage) string {
	body := bytes.TrimSpace(raw)
	if len(body) == 0 || string(body) == "null" {
		return ""
	}
	if body[0] == '"' {
		var s string
		if err := json.Unmarshal(body, &s); err == nil {
			return s
		}
		return ""
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, body); err != nil {
		return string(body)
	}
	return buf.String()
}

// MenuPath 是命令在应用菜单里的位置。
//
// 同一个语义有三种编码：数组（["Format","Text"]）、单个顶层菜单名（"Edit"）、null。
// 归一成切片，调用方只处理一种形状。
type MenuPath []string

func (m *MenuPath) UnmarshalJSON(b []byte) error {
	body := bytes.TrimSpace(b)
	if len(body) == 0 || string(body) == "null" {
		*m = nil
		return nil
	}
	if body[0] == '"' {
		var one string
		if err := json.Unmarshal(body, &one); err != nil {
			return err
		}
		if one == "" {
			*m = nil
		} else {
			*m = MenuPath{one}
		}
		return nil
	}
	var list []string
	if err := json.Unmarshal(body, &list); err != nil {
		return fmt.Errorf("menu 既不是字符串也不是数组：%s", clip(string(body), 40))
	}
	*m = list
	return nil
}

// Path 把一段菜单编码摊成真正的层级。
//
// 单段里可能又用 "/" 折了几层：FigCraft 给的是 ["Text/Adjust"]，
// 语义上仍是 Text › Adjust。按 "/" 再切一次，三代应用就都变成同一棵树。
func (m MenuPath) Path() []string {
	out := make([]string, 0, len(m))
	for _, seg := range m {
		for _, part := range strings.Split(seg, "/") {
			if name := strings.TrimSpace(part); name != "" {
				out = append(out, name)
			}
		}
	}
	return out
}

// clip 把过长的片段截断，避免把一整张命令表塞进错误信息。
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Group 是命令 id 的前缀（slide.new → slide），用来做界面分组与 MCP 工具归类。
func (c Command) Group() string {
	for i := 0; i < len(c.ID); i++ {
		if c.ID[i] == '.' {
			return c.ID[:i]
		}
	}
	return c.ID
}

// Commands 拉取完整命令表。这是「零适配接新应用」的支点。
func (c *Client) Commands(ctx context.Context) ([]Command, error) {
	raw, err := c.callKind(ctx, kindCommands, nil)
	if err != nil {
		return nil, err
	}
	var list []Command
	if err := json.Unmarshal(raw, &list); err == nil {
		return list, nil
	}
	// 有的实现把结果包在对象里（PdfCraft: {"commands":[…]}），做一次兼容。
	var wrapped struct {
		Commands []Command `json:"commands"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return nil, fmt.Errorf("命令表格式无法识别: %w", err)
	}
	return wrapped.Commands, nil
}

// Execute 执行一条命令。params 为 nil 时按空对象发送。
//
// 两个世代的参数形状不一样，不能共用一份：
//
//	engine.execute  {"command":"slide.new","params":{…}}
//	ui.command     {"id":"measure.info", …参数平铺在顶层}   ← PdfCraft 0.14
//
// 传错形状时服务端的报错是 "missing string parameter id"，看着像命令不存在，
// 实际上只是信封不对。这里按方法名分别组装。
func (c *Client) Execute(ctx context.Context, command string, params any) (json.RawMessage, error) {
	if params == nil {
		params = map[string]any{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if method := c.methods[kindExecute]; method != "" {
		return c.callLocked(ctx, method, execParams(method, command, params))
	}
	var lastErr error
	for _, method := range methodCandidates[kindExecute] {
		raw, err := c.callLocked(ctx, method, execParams(method, command, params))
		if err == nil {
			c.methods[kindExecute] = method
			return raw, nil
		}
		lastErr = err
		var re *RemoteError
		if !errors.As(err, &re) || !isUnknownMethod(re.Message) {
			return nil, err
		}
	}
	return nil, lastErr
}

// execParams 按方法名组装参数。
func execParams(method, command string, params any) map[string]any {
	if method != "ui.command" {
		return map[string]any{"command": command, "params": params}
	}
	out := map[string]any{"id": command}
	if m, ok := params.(map[string]any); ok {
		for k, v := range m {
			if k != "id" {
				out[k] = v
			}
		}
		return out
	}
	out["params"] = params
	return out
}

// Document 读取当前文档结构：幻灯片/图形/选区/路径。
func (c *Client) Document(ctx context.Context) (json.RawMessage, error) {
	return c.callKind(ctx, kindInspect, nil)
}

// UIState 读取界面状态：当前工具、窗口与画布尺寸、对话框、放映状态。
// 它也是最轻的存活探测。
func (c *Client) UIState(ctx context.Context) (json.RawMessage, error) {
	return c.callKind(ctx, kindState, nil)
}

// Ping 用一个便宜的方法确认应用还在响应。
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.UIState(ctx)
	return err
}

// Screenshot 让应用截一张窗口图。path 为空时协议返回 base64，这里统一要求落盘，
// 避免把几十 MB 的图片塞进模型上下文。
func (c *Client) Screenshot(ctx context.Context, path string) (json.RawMessage, error) {
	if path == "" {
		return nil, fmt.Errorf("截图必须给出落盘路径")
	}
	return c.Call(ctx, "ui.screenshot", map[string]any{"path": path})
}
