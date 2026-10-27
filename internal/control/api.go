package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
)

// Command 是应用命令表里的一条。
//
// 字段来自 engine.commands 的实测输出。**不同应用的编码不一样**，这里按最宽的那种解：
//
//	                DeckCraft        FigCraft
//	menu            ["Format","Text"] "Edit" / null
//	shortcut        "Cmd+B" / ""      "Cmd+Z" / null
//	enabled         bool              ——（没有这个字段，给的是 undoable）
//
// 早先 menu 按 []string 硬解，FigCraft 的命令表整体解析失败：一张 597 条的表，
// 一条 json 类型不符就把整个 engine.commands 打回来。
type Command struct {
	ID       string   `json:"id"`
	Label    string   `json:"label"`
	Menu     MenuPath `json:"menu"`
	Params   string   `json:"params"`
	Shortcut string   `json:"shortcut"`
	// Enabled 是「此刻能不能用」，是运行时上下文：没有文本选区时 format.* 就是 false。
	// 用指针是因为有的应用根本不报这个字段，那种情况和「明确不可用」不是一回事——
	// 解成 bool 会变成 597 条全 false，看起来像整个应用都残废了。
	Enabled *bool `json:"enabled,omitempty"`
	// Undoable 是 FigCraft 那类应用给的替代信息：这条命令是否进撤销栈。
	Undoable bool `json:"undoable,omitempty"`
}

// MenuPath 是命令在应用菜单里的位置。
//
// 同一个语义有三种编码：数组（["Format","Text"]）、单个顶层菜单名（"Edit"）、null。
// 归一成切片，调用方只处理一种形状。字符串不按 ">" 拆：FigCraft 给的就是顶层菜单名，
// 拆一个不存在的分隔符只会丢信息，而匹配是子串匹配，不拆也能命中。
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
	raw, err := c.Call(ctx, "engine.commands", nil)
	if err != nil {
		return nil, err
	}
	var list []Command
	if err := json.Unmarshal(raw, &list); err == nil {
		return list, nil
	}
	// 少数实现把结果包在对象里，做一次兼容。
	var wrapped struct {
		Commands []Command `json:"commands"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return nil, fmt.Errorf("命令表格式无法识别: %w", err)
	}
	return wrapped.Commands, nil
}

// Execute 执行一条命令。params 为 nil 时按空对象发送。
func (c *Client) Execute(ctx context.Context, command string, params any) (json.RawMessage, error) {
	if params == nil {
		params = map[string]any{}
	}
	return c.Call(ctx, "engine.execute", map[string]any{
		"command": command,
		"params":  params,
	})
}

// Document 读取当前文档结构：幻灯片/图形/选区/路径。
func (c *Client) Document(ctx context.Context) (json.RawMessage, error) {
	return c.Call(ctx, "document.inspect", nil)
}

// UIState 读取界面状态：当前工具、窗口与画布尺寸、对话框、放映状态。
// 它也是最轻的存活探测。
func (c *Client) UIState(ctx context.Context) (json.RawMessage, error) {
	return c.Call(ctx, "ui.inspect", nil)
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
