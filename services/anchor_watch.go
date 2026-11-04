package services

// 保存感知：控制通道没有通知机制（三代协议都是一问一答，没有 subscribe/notify），
// 用户在应用里自己保存文件这件事不会从端口上推过来。唯一的信号是身份命令的
// 返回 —— dirty true→false、path 从无到有（各应用的身份命令不同，见 catalog.IdentityCmd）。
//
// MCP 守卫只在「下一次写入前」读身份，所以两次写入之间的保存，agent 和界面都
// 不知情。这个 watcher 用拉的方式补上推送的缺口：对每个有锚点的应用周期性读一次
// 身份，发现「首次保存」（锚点还没有 path、当前有了）就就地升级锚点并记一条
// 流水 —— 规则与守卫的 relSaved 升级完全一致，只是把发现时间从「下次写入」
// 提前到「保存后几秒内」。
//
// 刻意只做升级，不做别的动作：
//   - 不启动应用：没连接就跳过（下次守卫自然会处理）；
//   - 不做找回：锚点有 path 而当前对不上时，是切走了还是另存了区分不了，
//     凭路径 file.open 会抢用户眼前的窗口 —— 这是写入时守卫的职责，轮询不抢；
//   - 应用没有身份命令时安静跳过，不刷流水。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"craft-agent/internal/control"
	"craft-agent/internal/craft"
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
		info, cmd, err := readIdentity(ctx, a.App, client)
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
				AppID: a.App, Command: cmd, OK: true,
				Detail: fmt.Sprintf("用户已在应用内保存：锚点升级为文件身份 %s", info.Path),
			})
		}
	}
}

// fileInfo 是身份命令的返回口径，与 internal/mcp 的 docIdentity 一致。
// doc.list 来源（FigCraft 那代）也压成这个形状：name→title。名字沿用旧称，
// 因为这三个来源在这一层的形状确实一样 —— 但命令本身不是 file.info。
type fileInfo struct {
	Title  string `json:"title"`
	Path   string `json:"path"`
	Slides int    `json:"slides"`
	Dirty  bool   `json:"dirty"`
}

// idSources 记住「试出来的」身份来源，只对 catalog 没声明的应用生效；
// 只在轮询 goroutine 里读写（watchMu 串行化），不需要锁。
const (
	srcFileInfo       = "file.info"
	srcDocList        = "doc.list"
	srcDrawingInspect = "drawing.inspect"
)

var idSources = map[string]string{}

// identityCommand 决定该应用用哪条命令读身份：catalog 声明的优先，
// 其次是过去试出来的。返回空串表示「不知道，需要试」。
// 抽成纯函数是为了能钉住：CADCraft 永远不会走到试错分支。
func identityCommand(appID string) string {
	if a, ok := craft.ByID(appID); ok && a.IdentityCmd != "" {
		return a.IdentityCmd
	}
	return idSources[appID]
}

// readIdentity 读当前文档身份，并回报实际用的命令（流水里要记真实命令）。
//
// 来源以 catalog 的声明为准：三个应用的叫法各不相同，且不是套件约定 ——
// CADCraft 既没有 file.info 也没有 doc.list，身份只在 drawing.inspect 里。
// 拿固定顺序去试，等于每轮对着它发两条不存在的命令。
//
// 只有 catalog 没声明的应用才试错（兼容将来新接入、尚未写进 catalog 的应用）：
// 报 unknown command 就换下一个，把成功的那个记住。
func readIdentity(ctx context.Context, app string, client *control.Client) (fileInfo, string, error) {
	src := identityCommand(app)

	switch src {
	case srcDocList:
		info, err := readDocListIdentity(ctx, client)
		return info, srcDocList, err
	case srcDrawingInspect:
		info, err := readDrawingInspect(ctx, client)
		return info, srcDrawingInspect, err
	case srcFileInfo:
		info, err := readFileInfo(ctx, client)
		return info, srcFileInfo, err
	}

	// 没声明过来源：按已知的三代叫法试。每条只试一次，成功即记住。
	info, err := readFileInfo(ctx, client)
	if err == nil {
		idSources[app] = srcFileInfo
		return info, srcFileInfo, nil
	}
	if !strings.Contains(err.Error(), "unknown command") {
		return fileInfo{}, srcFileInfo, err
	}
	info2, err2 := readDocListIdentity(ctx, client)
	if err2 == nil {
		idSources[app] = srcDocList
		return info2, srcDocList, nil
	}
	if !strings.Contains(err2.Error(), "unknown command") {
		return fileInfo{}, srcDocList, err2
	}
	info3, err3 := readDrawingInspect(ctx, client)
	if err3 == nil {
		idSources[app] = srcDrawingInspect
		return info3, srcDrawingInspect, nil
	}
	return fileInfo{}, "", err
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

// readDocListIdentity 从 doc.list 取活动文档的身份。
func readDocListIdentity(ctx context.Context, client *control.Client) (fileInfo, error) {
	raw, err := client.Execute(ctx, "doc.list", nil)
	if err != nil {
		return fileInfo{}, err
	}
	var entries []struct {
		Name   string `json:"name"`
		Path   string `json:"path"`
		Dirty  bool   `json:"dirty"`
		Active bool   `json:"active"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return fileInfo{}, fmt.Errorf("doc.list 返回无法解析: %w", err)
	}
	if len(entries) == 0 {
		return fileInfo{}, fmt.Errorf("doc.list 返回空列表：没有打开的文档")
	}
	active := entries[0]
	for _, e := range entries {
		if e.Active {
			active = e
			break
		}
	}
	return fileInfo{Title: active.Name, Path: active.Path, Dirty: active.Dirty}, nil
}

// readDrawingInspect 是 drawing.inspect 来源（CADCraft 0.17）：
// 返回字段与 file.info 同形，path 为 null 时解成空串。
func readDrawingInspect(ctx context.Context, client *control.Client) (fileInfo, error) {
	raw, err := client.Execute(ctx, "drawing.inspect", nil)
	if err != nil {
		return fileInfo{}, err
	}
	var info fileInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return fileInfo{}, fmt.Errorf("drawing.inspect 返回无法解析: %w", err)
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
