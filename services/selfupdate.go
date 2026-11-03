package services

// Craft Agent 自己的更新：检查、安装、重启。
//
// 套件里 13 个应用的更新早就有（Install 对「没装的」也一样工作），缺的是
// 宿主自己 —— 原版 launcher 连自己都能「Update and restart」，这才是
// 「全部更新功能」的最后一块。本文件把 craft.Self 接进同一条管线：
// 查发布与挑资产的代码一字不改，只有三件事是「自己」特有的：
//
//  1. 不退出自己：update.QuitApp 是给别的应用的；换包期间自己必须活着，
//     macOS 允许 rename 一个正在运行的 .app（旧 inode 由进程握着）。
//  2. 换包完成后拉起新副本，再退出自己 —— 顺序不能反。
//  3. 本机版本不靠 Detect：自己就是编译产物，main 注入的 Version 才是
//     权威（跑 go run 时磁盘上根本没有 Craft Agent.app）。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"craft-agent/internal/craft"
	"craft-agent/internal/journal"
	"craft-agent/internal/release"
	"craft-agent/internal/tray"
	"craft-agent/internal/update"
)

// SelfUpdate 是宿主自己的更新状态，界面上单独一块（它不在套件表格里）。
type SelfUpdate struct {
	Version         string `json:"version"`                 // 本机在跑的版本（编译期注入）
	LatestVersion   string `json:"latestVersion,omitempty"` // 远端最新发布
	UpdateAvailable bool   `json:"updateAvailable"`         // 远端比本机新
	URL             string `json:"url,omitempty"`           // 发布页（手动下载兜底）
	Checked         bool   `json:"checked"`                 // 是否查到过远端（false = 还没查或查挂了）
}

// selfUpdateOf 从检查快照里取宿主自己的最新发布并判定有没有更新。
// 版本比较和套件应用同一套口径（release.IsUpdate），「本地比远端新」
// （开发机常见）不算可更新。
func selfUpdateOf(snap release.Snapshot) SelfUpdate {
	out := SelfUpdate{Version: Version}
	latest, ok := snap.Apps[craft.Self.ID]
	if !ok {
		return out
	}
	out.Checked = true
	out.LatestVersion = latest.Version
	out.URL = latest.URL
	if lv, okL := release.Parse(latest.Version); okL {
		if mv, okM := release.Parse(Version); okM {
			out.UpdateAvailable = release.IsUpdate(lv, mv)
		}
	}
	return out
}

// InstallSelf 把宿主自己换到最新发布，装好后拉起新副本并退出本进程。
//
// 流程与套件应用的 Install 相同（下载 → SHA-256 → 挂载 → 验签 → 原子换位），
// 差异只有开头没有 QuitApp、结尾多了 relaunch。换包期间本进程还活着：
// rename 正在运行的 .app 在 macOS 上是允许的，旧副本的 inode 由进程握着，
// 替换完成前用户看到的界面不受影响。
func (s *AppService) InstallSelf() (InstallResult, error) {
	if !s.beginInstall(craft.Self.ID) {
		return InstallResult{}, fmt.Errorf("Craft Agent 正在安装中")
	}
	defer s.endInstall(craft.Self.ID)

	start := time.Now()
	res := InstallResult{AppID: craft.Self.ID}
	fail := func(err error) (InstallResult, error) {
		note := err.Error()
		if route := s.upd.Route(); route != "" {
			note += "（出网：" + route + "）"
		}
		s.journal.Append(journal.Entry{
			At: time.Now(), Source: "gui", Session: "gui", Kind: "update",
			AppID: craft.Self.ID, Command: "install", OK: false, Detail: note,
			Duration: time.Since(start).Milliseconds(),
		})
		s.emit(UpdateProgress{AppID: craft.Self.ID, Step: "error", Percent: -1, Note: note})
		return res, err
	}

	latest := s.rel.Snapshot().Apps[craft.Self.ID]
	if latest.Version == "" || latest.Tag == "" {
		return fail(fmt.Errorf("还没有 Craft Agent 的版本信息，先检查一次更新"))
	}

	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Minute)
	defer cancel()

	rel, err := s.upd.Release(ctx, s.rel.RepoFor(craft.Self), latest.Tag)
	if err != nil {
		return fail(err)
	}
	asset, err := update.Pick(craft.Self, rel)
	if err != nil {
		return fail(err)
	}
	if asset, err = s.upd.EnsureSum(ctx, craft.Self.Repo, rel, asset); err != nil {
		return fail(err)
	}

	out, err := update.Install(ctx, s.upd, update.Options{
		App:     craft.Self,
		Asset:   asset,
		WorkDir: update.WorkDir(s.host.DataDir()),
		Version: rel.Version,
		OnStage: func(st update.Stage) {
			s.emit(UpdateProgress{
				AppID: craft.Self.ID, Step: st.Step, Percent: st.Percent,
				Bytes: st.Bytes, Total: st.Total, Note: st.Note,
			})
		},
	})
	if err != nil {
		return fail(err)
	}

	res.Version = out.Version
	res.Path = out.Path
	res.Backup = out.Backup
	res.Manual = out.Manual
	res.DurationMs = time.Since(start).Milliseconds()

	s.journal.Append(journal.Entry{
		At: time.Now(), Source: "gui", Session: "gui", Kind: "update",
		AppID: craft.Self.ID, Command: "install", OK: true,
		Detail:   fmt.Sprintf("v%s → v%s（%s）", Version, out.Version, out.Path),
		Duration: res.DurationMs,
	})

	// 只有 darwin 能自动装；其他平台 Manual=true，安装包已备好，让用户自己装。
	if !out.Manual {
		// 拉起新副本再退场：顺序反了就没有人负责退场了。give 新进程一秒
		// 起步时间，避免本进程退出顺带把启动环境收走。
		if s.hooks != nil && s.hooks.relaunch != nil {
			s.hooks.relaunch(out.Path)
		}
	}
	return res, nil
}

// notifyUpdates 用系统通知告诉用户有更新。macOS 走 osascript —— Wails
// beta.29 还没有通知 API，而这一条提醒不值得为此引一个依赖。
// 通知点击不会打开 Craft Agent：托盘菜单里有「有更新」条目兜底。
func (s *AppService) notifyUpdates(n int) {
	switch runtime.GOOS {
	case "darwin":
		body := fmt.Sprintf("%d 个应用有新版本，点开 Craft Agent 安装", n)
		script := `display notification "` + body + `" with title "Craft Agent" sound name "default"`
		cmd := exec.Command("osascript", "-e", script)
		cmd.Start()
		go func() { _ = cmd.Wait() }()
	case "windows":
		// Windows 的 toast 要走 PowerShell 的 Runtime 接口，先托盘标记兜底，
		// 通知等真正需要时再做。
	}
}

// notifiedFile 记录已经通知过的「应用@版本」，落盘的原因：进程重启后
// 不该把同一件事再喊一遍 —— 那和没去重一样烦。
func (s *AppService) notifiedPath() string {
	return filepath.Join(s.host.DataDir(), "updates-notified.json")
}

func (s *AppService) loadNotified() map[string]bool {
	out := map[string]bool{}
	raw, err := os.ReadFile(s.notifiedPath())
	if err != nil {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	return out
}

func (s *AppService) saveNotified(seen map[string]bool) {
	raw, err := json.Marshal(seen)
	if err != nil {
		return
	}
	_ = os.WriteFile(s.notifiedPath(), raw, 0o600)
}

// AnnounceUpdates 检查一遍快照，把没提醒过的新版本提醒掉，返回本次提醒数。
// 由定时检查与手动检查共用：手动点「检查更新」也该享受同一条提醒链路。
func (s *AppService) AnnounceUpdates() int {
	snap := s.rel.Snapshot()
	seen := s.loadNotified()
	fresh := 0
	for id, l := range snap.Apps {
		if id == craft.Self.ID {
			continue // 自己的更新由界面单独展示，不混进应用通知
		}
		if l.Version == "" {
			continue
		}
		key := id + "@" + l.Version
		if seen[key] {
			continue
		}
		// 只有「真的可更新」才值得喊：没装的/已最新的都不算。
		st := s.statusOf(id)
		if !st.UpdateAvailable {
			seen[key] = true // 记下来，免得每次检查都重新判定
			continue
		}
		seen[key] = true
		fresh++
	}
	s.saveNotified(seen)
	total := 0
	for _, st := range s.Suite() {
		if st.UpdateAvailable {
			total++
		}
	}
	tray.SetUpdates(total)
	if fresh > 0 {
		s.notifyUpdates(total)
		s.journal.Append(journal.Entry{
			At: time.Now(), Source: "gui", Session: "gui", Kind: "update",
			Command: "notify", OK: true,
			Detail: fmt.Sprintf("reminded=%d updates=%d", fresh, total),
		})
	}
	return fresh
}

// CheckLoop 周期性检查更新并提醒。force=false 走 6 小时缓存，所以
// 真实的出网频率就是 6 小时一次。ctx 结束（退出应用）即停。
func (s *AppService) CheckLoop() {
	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.CheckUpdates(false); err != nil {
				continue // 失败已记流水，下个周期再试
			}
			s.AnnounceUpdates()
		}
	}
}

/* ---------------- 全部更新 ---------------- */

// InstallOutcome 是「全部更新」里单个应用的结果。
type InstallOutcome struct {
	AppID   string `json:"appId"`
	OK      bool   `json:"ok"`
	Version string `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
}

// InstallAllResult 是一次「全部更新」的汇总。
type InstallAllResult struct {
	Total   int              `json:"total"` // 实际动手更新的应用数
	OK      int              `json:"ok"`
	Failed  int              `json:"failed"`
	Results []InstallOutcome `json:"results"`
}

// InstallAll 把所有「可更新」的应用逐个换到最新发布。
//
// 串行是刻意的：每个应用更新都要退出应用、挂载镜像、换包，并行只会让
// 两个 hdiutil 和两次 AppleScript quit 互相踩；慢一点但每一步都清楚。
// 未安装的应用不在范围内 —— 这个按钮的语义是「把已有的都更到最新」，
// 装新应用走各自卡片上的「安装」。
func (s *AppService) InstallAll() (InstallAllResult, error) {
	start := time.Now()
	out := InstallAllResult{Results: []InstallOutcome{}}

	for _, st := range s.Suite() {
		if !st.UpdateAvailable {
			continue
		}
		res, err := s.Install(st.App.ID)
		if err != nil {
			out.Failed++
			out.Results = append(out.Results, InstallOutcome{AppID: st.App.ID, Error: err.Error()})
			continue
		}
		out.OK++
		out.Results = append(out.Results, InstallOutcome{AppID: st.App.ID, OK: true, Version: res.Version})
	}
	out.Total = len(out.Results)

	s.journal.Append(journal.Entry{
		At: time.Now(), Source: "gui", Session: "gui", Kind: "update",
		Command: "install-all", OK: out.Failed == 0,
		Detail:   fmt.Sprintf("total=%d ok=%d failed=%d", out.Total, out.OK, out.Failed),
		Duration: time.Since(start).Milliseconds(),
	})
	return out, nil
}
