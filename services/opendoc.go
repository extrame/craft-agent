package services

// 文档打开分发：系统「打开方式」或 CLI 把一个文档路径交给 Craft Agent，
// 这里把它送到对应的 Craft 应用 —— 应用由主程序启动并持有（启动即带
// --control，见 host.Ensure），打开动作走它自己的命令。
//
// 这就是「注册为系统打开方式」的后半段：操作系统只负责把文件递过来，
// 选哪个应用、怎么打开，是本文件的事。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"craft-agent/internal/craft"
	"craft-agent/internal/journal"
)

// OpenDocument 把一个文档路径分发到对应应用。
//
// 流程：按扩展名找应用 → 启动/连接（默认带 connect）→ 命中「已打开」
// 去重就跳过 → 执行它的 DocOpen 命令。每一步都记流水：
// 用户双击文件这件事在窗口里要看得见，失败了更要看得见。
func (s *AppService) OpenDocument(path string) error {
	start := time.Now()
	app, ok := craft.ForExt(filepath.Ext(path))
	if !ok {
		return s.journalOpenDoc(path, "", start, false,
			fmt.Sprintf("不认识的文档类型 %q：套件里没有应用声明接管它", filepath.Ext(path)))
	}
	if app.DocOpen == "" {
		return s.journalOpenDoc(path, app.ID, start, false,
			"该应用还没有实测过按路径打开的命令，无法分发")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return s.journalOpenDoc(path, app.ID, start, false, err.Error())
	}
	if _, err := os.Stat(abs); err != nil {
		return s.journalOpenDoc(abs, app.ID, start, false, "文件不存在："+err.Error())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client, err := s.host.Ensure(ctx, app.ID)
	if err != nil {
		return s.journalOpenDoc(abs, app.ID, start, false, err.Error())
	}

	params, _ := json.Marshal(map[string]any{"path": abs})
	if out, skip := maybeSkipOpen(ctx, app.ID, client, app.DocOpen, params); skip {
		return s.journalOpenDocOut(abs, app.ID, start, out)
	}
	out, err := client.Execute(ctx, app.DocOpen, params)
	detail := ""
	if err != nil {
		detail = err.Error()
	} else if len(out) > 0 && len(out) <= 300 {
		detail = string(out)
	}
	return s.journalOpenDoc(abs, app.ID, start, err == nil, detail)
}

// journalOpenDoc 记一条分发流水并返回 err（存在时）。
func (s *AppService) journalOpenDoc(path, appID string, start time.Time, ok bool, detail string) error {
	s.journal.Append(journal.Entry{
		At: time.Now(), Source: "gui", Session: "gui", Kind: "open-doc",
		AppID: appID, Command: "open", OK: ok,
		Detail:   detail,
		Duration: time.Since(start).Milliseconds(),
	})
	if ok {
		return nil
	}
	return fmt.Errorf("打开 %s 失败: %s", path, detail)
}

func (s *AppService) journalOpenDocOut(path, appID string, start time.Time, out json.RawMessage) error {
	detail := "文件已打开，已重新定位到该实例：" + string(out)
	s.journal.Append(journal.Entry{
		At: time.Now(), Source: "gui", Session: "gui", Kind: "open-doc",
		AppID: appID, Command: "open", OK: true,
		Detail:   detail,
		Duration: time.Since(start).Milliseconds(),
	})
	return nil
}

// LaunchApp 从托盘一键打开一个 Craft 应用：启动即带 --control，
// 应用出现在界面上、可被 CLI/MCP 直接用。
func (s *AppService) LaunchApp(id string) error {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := s.host.Ensure(ctx, id)
	detail := "已从托盘启动（控制通道默认开启）"
	if err != nil {
		detail = err.Error()
	}
	s.journal.Append(journal.Entry{
		At: time.Now(), Source: "gui", Session: "gui", Kind: "launch",
		AppID: id, OK: err == nil,
		Detail:   detail,
		Duration: time.Since(start).Milliseconds(),
	})
	return err
}
