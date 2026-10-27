package control

import (
	"context"
	"encoding/json"
	"fmt"
)

// Command 是应用命令表里的一条。
//
// 字段来自 engine.commands 的实测输出：id / label / menu / params / shortcut / enabled，
// 每个应用（只要同构）都会给出自己的完整命令集，所以 MCP 工具可以据此动态生成，
// 不为某个应用硬编码命令名。
type Command struct {
	ID       string   `json:"id"`
	Label    string   `json:"label"`
	Menu     []string `json:"menu"`
	Params   string   `json:"params"`
	Shortcut string   `json:"shortcut"`
	Enabled  bool     `json:"enabled"`
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
