package services

// 保存感知：控制通道没有通知机制（三代协议都是一问一答，没有 subscribe/notify），
// 用户在应用里自己保存文件这件事不会从端口上推过来。唯一的信号是 file.info 的
// 返回 —— dirty true→false、path 从无到有。
//
// MCP 守卫只在「下一次写入前」读身份，所以两次写入之间的保存，agent 和界面都
// 不知情。这个 watcher 用拉的方式补上推送的缺口：对每个有锚点的应用周期性读一次
// file.info，发现「首次保存」（锚点还没有 path、当前有了）就就地升级锚点并记一条
// 流水 —— 规则与守卫的 relSaved 升级完全一致，只是把发现时间从「下次写入」
// 提前到「保存后几秒内」。
//
// 刻意只做升级，不做别的动作：
//   - 不启动应用：没连接就跳过（下次守卫自然会处理）；
//   - 不做找回：锚点有 path 而当前对不上时，是切走了还是另存了区分不了，
//     凭路径 file.open 会抢用户眼前的窗口 —— 这是写入时守卫的职责，轮询不抢；
//   - 应用没有 file.info（FigCraft 那代）时安静跳过，不刷流水。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"craft-agent/internal/control"
	"craft-agent/internal/journal"
)

// anchorWatchInterval 是轮询节奏。file.info 是单条轻命令，3 秒一次的成本
// 可以忽略；太快没有意义 —— 保存是人手动的动作，秒级延迟足够。
const anchorWatchInterval = 3 * time.Second

// watchMu 保证同一时刻只有一轮轮询在跑：某个应用回话慢时，ticker 会触发
// 下一轮，直接放弃这一拍，不值得排队。
func (s *AppService) startAnchorWatch() {
	go func() {
		ticker := time.NewTicker(anchorWatchInterval)
		defer ticker.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-ticker.C:
				s.watchAnchorsOnce()
			}
		}
	}()
}

func (s *AppService) watchAnchorsOnce() {
	if !s.watchMu.TryLock() {
		return
	}
	defer s.watchMu.Unlock()

	for _, a := range s.Anchors() {
		select {
		case <-s.ctx.Done():
			return
		default:
		}
		if !a.FileExists && a.Path != "" {
			// 文件已删除的锚点不值得每 3 秒问一次应用。
			continue
		}
		client := s.host.Client(a.App)
		if client == nil {
			// 界面没连过这个应用时不主动接管：轮询是后台行为，
			// 悄悄开通道越过了「用户没点它」的边界。MCP 守卫兜底。
			continue
		}
		ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
		info, err := readFileInfo(ctx, client)
		cancel()
		if err != nil {
			continue // 没开文档、没有 file.info、应用正忙：都当「这次没看到」
		}
		if saveTransition(a.Path, info.Path) == "" {
			continue
		}
		if s.upgradeAnchor(a.App, info) {
			s.journal.Append(journal.Entry{
				At: time.Now(), Source: "gui", Session: "gui", Kind: "anchor",
				AppID: a.App, Command: "file.info", OK: true,
				Detail: fmt.Sprintf("用户已在应用内保存：锚点升级为文件身份 %s", info.Path),
			})
		}
	}
}

// fileInfo 是 file.info 的返回口径，与 internal/mcp 的 docIdentity 一致。
type fileInfo struct {
	Title  string `json:"title"`
	Path   string `json:"path"`
	Slides int    `json:"slides"`
	Dirty  bool   `json:"dirty"`
}

func readFileInfo(ctx context.Context, client *control.Client) (fileInfo, error) {
	raw, err := client.Execute(ctx, "file.info", nil)
	if err != nil {
		return fileInfo{}, err
	}
	var info fileInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return fileInfo{}, fmt.Errorf("file.info 返回无法解析: %w", err)
	}
	return info, nil
}

// saveTransition 判断这次观察是不是「用户保存了」。
//
// 只认一种转变：锚点还没有文件身份、当前有了。锚点没身份时谈不上「切到另一份
// 有身份的文档」—— 写入永远落当前文档，出现的 path 只能是这份文档被保存了。
// 其余情况（path 相同、锚点有 path 而当前不同）都不动：前者无事可做，
// 后者是守卫的找回职责，轮询不抢。
func saveTransition(anchorPath, curPath string) string {
	if anchorPath == "" && curPath != "" {
		return curPath
	}
	return ""
}

// upgradeAnchor 把某应用的锚点升级为文件身份（读-改-写，保留其他应用的条目
// 与本条目的 anchoredAt —— 锚定时间不变，变的是身份）。
// 返回 false 表示锚点已被另一边（MCP 会话或界面）动过、这次不再覆盖。
func (s *AppService) upgradeAnchor(app string, info fileInfo) bool {
	path := filepath.Join(s.host.DataDir(), "anchors.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var file map[string]json.RawMessage
	if err := json.Unmarshal(raw, &file); err != nil {
		return false
	}
	entryRaw, ok := file[app]
	if !ok {
		return false
	}
	var entry struct {
		Title      string    `json:"title"`
		Path       string    `json:"path"`
		Slides     int       `json:"slides"`
		Dirty      bool      `json:"dirty"`
		AnchoredAt time.Time `json:"anchoredAt"`
	}
	if err := json.Unmarshal(entryRaw, &entry); err != nil {
		return false
	}
	if entry.Path != "" {
		// 已有文件身份：不是这次升级要处理的状态（另一边先动过了）。
		return false
	}
	entry.Title = info.Title
	entry.Path = info.Path
	entry.Slides = info.Slides
	entry.Dirty = false
	out, err := json.Marshal(entry)
	if err != nil {
		return false
	}
	file[app] = out
	full, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return false
	}
	return os.WriteFile(path, full, 0o600) == nil
}
