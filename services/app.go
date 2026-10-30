// Package services 是前端能看见的那一层：Wails 绑定方法。
//
// 范围刻意很窄 —— 这个窗口只做三件事：
//  1. 维护并展示每个 Craft 应用的现状（装没装、开没开、连在哪、文档是什么）；
//  2. 提供启动 / 连接 / 断开这几个必要动作；
//  3. 展示会话操作流（本机和 AI 各自做了什么）。
//
// 真正的编辑能力不在界面里，而是统一交给 MCP：agent 走 tools，人走应用自己的窗口。
package services

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"craft-agent/internal/control"
	"craft-agent/internal/craft"
	"craft-agent/internal/gateway"
	"craft-agent/internal/host"
	"craft-agent/internal/journal"
	"craft-agent/internal/menu"
	"craft-agent/internal/release"
	"craft-agent/internal/tray"
	"craft-agent/internal/update"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// Version 由 main 注入。
var Version = "0.2.1"

// DocumentView 是文档状态摘要，只取界面要显示的部分。
type DocumentView struct {
	Path      string   `json:"path"`
	Dirty     bool     `json:"dirty"`
	Sections  []string `json:"sections"`
	Objects   int      `json:"objects"`
	Selection string   `json:"selection"`
	Summary   string   `json:"summary"`
}

// SessionView 是一条仍在线的 AI 会话。
type SessionView struct {
	Session    string   `json:"session"`
	StartedAt  string   `json:"startedAt"`
	LastActive string   `json:"lastActive"`
	Actions    int      `json:"actions"`
	Apps       []string `json:"apps"`
}

// AppService 绑定到前端。
type AppService struct {
	host    *host.Host
	journal *journal.Journal
	// rel 查本机版本与远端最新发布；缓存落在数据目录里。
	rel *release.Checker
	// upd 下载并安装更新。只在用户点「更新」时用。
	upd *update.Client
	// menus 是预加载的菜单：内存一份、磁盘一份，切应用时不用等应用回话。
	menus *menu.Store

	installMu  sync.Mutex
	installing map[string]bool

	// watchMu 让锚点轮询同一时刻只跑一轮（见 anchor_watch.go）。
	watchMu sync.Mutex

	// gatewayStop 停掉命令转发口（见 startGateway）；未开启时为 nil。
	gatewayStop func()

	// Emit 把安装进度推给界面；由 main 接上 Wails 事件总线。
	Emit func(UpdateProgress)

	ctx    context.Context
	cancel context.CancelFunc
}

// NewAppService 构造绑定层。
func NewAppService(h *host.Host, j *journal.Journal) *AppService {
	ctx, cancel := context.WithCancel(context.Background())
	svc := &AppService{
		host:       h,
		journal:    j,
		rel:        release.NewChecker(h.DataDir(), Version),
		upd:        update.NewClient("Craft-Agent/" + Version),
		menus:      menu.NewStore(h.DataDir()),
		installing: map[string]bool{},
		ctx:        ctx,
		cancel:     cancel,
	}
	// 上次预加载的结果先装回来：界面第一帧就有菜单，不用等应用响应。
	_ = svc.menus.Load()
	return svc
}

// ServiceStartup 由 Wails 在应用启动时调用。
//
// 签名必须与 wails 的 ServiceStartup 接口完全一致（ctx + ServiceOptions + error）——
// 之前写成只有 ctx 的版本，接口不满足，框架从未调用过它：
// 锚点保存感知轮询因此一直空转。这是 2026-10-30 接 gateway 时才暴露的。
func (s *AppService) ServiceStartup(ctx context.Context, options application.ServiceOptions) error {
	s.ctx, s.cancel = context.WithCancel(ctx)
	s.startAnchorWatch()
	s.startGateway()
	return nil
}

// ServiceShutdown 在窗口关闭时断开所有通道（应用进程留给用户）。
func (s *AppService) ServiceShutdown() error {
	if s.gatewayStop != nil {
		s.gatewayStop()
	}
	s.cancel()
	s.host.Close()
	return nil
}

/* ---------------- 转发口 ---------------- */

// startGateway 开出命令转发口：CLI（craft-agent run）与将来的 MCP 都只做
// 薄客户端，命令转发到这里执行。应用由主程序启动并持有 —— 主程序的进程组
// 不随任何一次调用结束，AI 客户端沙箱收割不到它，窗口就一闪不过。
func (s *AppService) startGateway() {
	stop, err := gateway.Serve(s.host.DataDir(), s.gatewayExec)
	if err != nil {
		log.Printf("命令转发口未开启：%v（CLI 将退回直连模式）", err)
		return
	}
	s.gatewayStop = stop
}

// gatewayExec 执行一条转发来的命令，并把它记进操作流 ——
// AI 客户端用 CLI 干的每件事，用户在窗口里都看得到。
//
// open 类命令先查当前文档：路径已经是活动文档就不再执行 ——
// agent 反复 open 同一个文件时，应用侧会弹重复的打开对话框或新窗口，
// 而这个事实（文件已开着）控制通道本来就答得上来。
func (s *AppService) gatewayExec(ctx context.Context, appID, command string, params json.RawMessage) (json.RawMessage, error) {
	start := time.Now()
	client, err := s.host.Ensure(ctx, appID)
	if err != nil {
		s.journal.Append(journal.Entry{
			At: time.Now(), Source: "cli", Session: "cli", Kind: "command",
			AppID: appID, Command: command, OK: false,
			Detail:   err.Error(),
			Duration: time.Since(start).Milliseconds(),
		})
		return nil, err
	}

	if out, skip := maybeSkipOpen(ctx, appID, client, command, params); skip {
		detail := "文件已打开，跳过重复打开：" + string(out)
		s.journal.Append(journal.Entry{
			At: time.Now(), Source: "cli", Session: "cli", Kind: "command",
			AppID: appID, Command: command, OK: true,
			Detail:   detail,
			Duration: time.Since(start).Milliseconds(),
		})
		return out, nil
	}

	out, err := client.Execute(ctx, command, params)
	detail := ""
	if err != nil {
		detail = err.Error()
	} else if len(out) > 0 && len(out) <= 200 {
		detail = string(out)
	}
	s.journal.Append(journal.Entry{
		At: time.Now(), Source: "cli", Session: "cli", Kind: "command",
		AppID: appID, Command: command, OK: err == nil,
		Detail:   detail,
		Duration: time.Since(start).Milliseconds(),
	})
	return out, err
}

// openCommands 是「按路径打开文档」的命令，跨应用统一拦截：
// CADCraft 是 AutoCAD 风格的 open，DeckCraft 这代是 file.open。
var openCommands = map[string]bool{
	"open":      true,
	"file.open": true,
}

// openTarget 从 open 参数里取路径；取不到（缺字段、不是对象）返回空串。
func openTarget(params json.RawMessage) string {
	var p struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(params, &p) != nil {
		return ""
	}
	return strings.TrimSpace(p.Path)
}

// samePath 比较两个路径是否指同一个文件：macOS 文件系统不区分大小写，
// Clean 抹掉多余分隔符。仅用于「是不是同一个文件」的判断，不做存在性检查。
func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// maybeSkipOpen 判断这条 open 是否可以跳过：目标路径已经是活动文档时
// 返回合成的成功结果（skip=true），不再打扰应用。其余情况一律放行去执行 ——
// open 本身是幂等意图，身份读不出来时执行它是对的默认。
func maybeSkipOpen(ctx context.Context, app string, client *control.Client, command string, params json.RawMessage) (json.RawMessage, bool) {
	if !openCommands[command] {
		return nil, false
	}
	target := openTarget(params)
	if target == "" {
		return nil, false
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	info, err := readIdentity(wctx, app, client)
	if err != nil || info.Path == "" {
		return nil, false
	}
	if !samePath(info.Path, target) {
		return nil, false
	}
	out, _ := json.Marshal(map[string]any{
		"ok":          true,
		"alreadyOpen": true,
		"path":        info.Path,
		"title":       info.Title,
	})
	return out, true
}

/* ---------------- 状态 ---------------- */

// Suite 返回套件里每个应用的当前状态，并补齐版本与更新信息。
//
// 版本这部分刻意放在服务层而不是 host：host 只负责进程与通道，查远端是另一件事，
// 而且它需要缓存与网络超时，不该拖慢「启动应用」这条主路径。
func (s *AppService) Suite() []host.Status {
	statuses := s.host.Status()
	snap := s.rel.Snapshot()
	for i := range statuses {
		st := &statuses[i]
		inst := s.rel.Installed(st.App)
		st.InstalledVersion = inst.Version
		st.VersionSource = inst.Source
		st.VersionError = inst.Error

		latest, ok := snap.Apps[st.App.ID]
		if !ok {
			continue
		}
		st.LatestVersion = latest.Version
		st.LatestURL = latest.URL
		st.LatestNotes = latest.Notes
		st.LatestPrerelease = latest.Prerelease
		if inst.Version == "" {
			continue
		}
		iv, okInstalled := release.Parse(inst.Version)
		lv, okLatest := release.Parse(latest.Version)
		if okInstalled && okLatest {
			st.UpdateAvailable = release.IsUpdate(lv, iv)
		}
	}
	return statuses
}

// Inspect 读当前文档摘要。状态面板用它显示「这个应用里现在是什么」。
func (s *AppService) Inspect(id string) (DocumentView, error) {
	client := s.host.Client(id)
	if client == nil {
		return DocumentView{}, fmt.Errorf("未连接")
	}
	raw, err := client.Document(s.ctx)
	if err != nil {
		return DocumentView{}, err
	}
	return summarizeDocument(raw), nil
}

/* ---------------- 菜单（预加载） ---------------- */

// Menus 返回每个应用已预加载的菜单摘要。界面用它显示「菜单多少条、什么时候抓的」。
func (s *AppService) Menus() []menu.Summary {
	return s.menus.Summaries(func(a craft.App) string { return s.rel.Installed(a).Version })
}

// Menu 返回预加载好的统一菜单模型。
//
// 只读缓存，不碰应用：界面在选中应用的一瞬间就会调它，要是这里顺手 Ensure，
// 点一下左边列表就等于替用户打开了那个应用。
func (s *AppService) Menu(id string) (menu.Menu, error) {
	if m, ok := s.menus.Get(id); ok {
		return m, nil
	}
	return menu.Menu{App: id}, nil
}

// RefreshMenu 重新抓一次命令表。应用没开着就启动它 —— 这是用户明确点了刷新。
func (s *AppService) RefreshMenu(id string) (menu.Menu, error) {
	app, ok := craft.ByID(id)
	if !ok {
		return menu.Menu{}, fmt.Errorf("未知应用: %s", id)
	}
	ctx, cancel := context.WithTimeout(s.ctx, 40*time.Second)
	defer cancel()
	client, err := s.host.Ensure(ctx, id)
	if err != nil {
		return menu.Menu{}, err
	}
	return s.loadMenu(ctx, client, app)
}

// PreloadMenus 把「已经在运行」的应用的菜单抓紧缓存。
//
// 刻意不启动应用：预加载是后台行为，用户打开 Craft Agent 不该换来六个窗口。
// 没开的那些等他自己启动（启动后会自动补），或者点界面上的「刷新菜单」。
func (s *AppService) PreloadMenus() []menu.Summary {
	ctx, cancel := context.WithTimeout(s.ctx, 90*time.Second)
	defer cancel()
	for _, app := range craft.Suite {
		if !app.Installed() || !s.menuNeedsRefresh(app) {
			continue
		}
		client := s.host.Client(app.ID)
		if client == nil {
			// Connect 只接管已监听的控制通道；应用没开时这里会很快失败。
			if err := s.host.Connect(ctx, app.ID); err != nil {
				continue
			}
			client = s.host.Client(app.ID)
		}
		if client == nil {
			continue
		}
		_, _ = s.loadMenu(ctx, client, app)
	}
	return s.Menus()
}

// loadMenu 抓一次命令表，重建成统一菜单并落盘，同时留一条流水。
func (s *AppService) loadMenu(ctx context.Context, client *control.Client, app craft.App) (menu.Menu, error) {
	start := time.Now()
	m, err := s.menus.Refresh(ctx, client, app, s.installedVersion(app))
	entry := journal.Entry{
		At: time.Now(), Source: "gui", Session: "gui", Kind: "menu",
		AppID: app.ID, Command: "engine.commands", OK: err == nil,
		Duration: time.Since(start).Milliseconds(),
	}
	if err != nil {
		entry.Detail = err.Error()
	} else {
		entry.Detail = fmt.Sprintf("%d 条命令 · %d 个菜单 · %d 条按前缀归组", m.Commands, len(m.Sections), m.Grouped)
	}
	s.journal.Append(entry)
	return m, err
}

// refreshMenuQuiet 在连接/启动之后补一次预加载。失败不打断动作本身：
// 菜单是辅助信息，连上了但菜单没抓到，界面照样能用。
func (s *AppService) refreshMenuQuiet(id string) {
	app, ok := craft.ByID(id)
	if !ok || !s.menuNeedsRefresh(app) {
		return
	}
	client := s.host.Client(id)
	if client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	_, _ = s.loadMenu(ctx, client, app)
}

func (s *AppService) installedVersion(app craft.App) string {
	return s.rel.Installed(app).Version
}

// menuNeedsRefresh：没缓存要抓；缓存的应用版本和本机不一致也要抓。
func (s *AppService) menuNeedsRefresh(app craft.App) bool {
	if _, ok := s.menus.Get(app.ID); !ok {
		return true
	}
	for _, sum := range s.Menus() {
		if sum.App == app.ID {
			return sum.Stale
		}
	}
	return true
}

/* ---------------- 文件锚点 ---------------- */

// AnchorView 是界面上可见的一份锚点：agent 端维护的「文件 ↔ 文档身份」对应关系。
// Saved=false 表示还停在 title 指纹阶段（文档没保存过，丢了就是丢了）；
// Saved=true 表示已升级为文件身份，应用重启也能凭 path 找回。
type AnchorView struct {
	App        string `json:"app"`
	AppName    string `json:"appName"`
	Title      string `json:"title"`
	Path       string `json:"path"`
	AnchoredAt string `json:"anchoredAt"`
	Saved      bool   `json:"saved"`
	FileExists bool   `json:"fileExists"`
}

// anchorFileShape 是 anchors.json 的读取口径，与 internal/mcp 的写入口径一致。
// 刻意在本包重述一遍而不是导出 mcpserver 的类型：绑定生成器只认本包的形状。
type anchorFileShape struct {
	Title      string    `json:"title"`
	Path       string    `json:"path"`
	AnchoredAt time.Time `json:"anchoredAt"`
}

// Anchors 返回所有落盘的锚点，套件顺序在前、其余（已卸载应用的遗留）在后。
// 文件不存在、损坏都当「没有」——锚点是意图，不是事实。
func (s *AppService) Anchors() []AnchorView {
	raw, err := os.ReadFile(filepath.Join(s.host.DataDir(), "anchors.json"))
	if err != nil {
		return nil
	}
	var file map[string]anchorFileShape
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil
	}
	known := map[string]bool{}
	out := make([]AnchorView, 0, len(file))
	for _, app := range craft.Suite {
		if d, ok := file[app.ID]; ok {
			out = append(out, s.anchorView(app.ID, d))
			known[app.ID] = true
		}
	}
	ids := make([]string, 0, len(file))
	for id := range file {
		if !known[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		out = append(out, s.anchorView(id, file[id]))
	}
	return out
}

func (s *AppService) anchorView(id string, d anchorFileShape) AnchorView {
	v := AnchorView{
		App:        id,
		AppName:    id,
		Title:      d.Title,
		Path:       d.Path,
		Saved:      d.Path != "",
		AnchoredAt: d.AnchoredAt.Format("2006-01-02 15:04"),
		FileExists: d.Path != "",
	}
	if app, ok := craft.ByID(id); ok {
		v.AppName = app.Name
	}
	if d.Path != "" {
		if _, err := os.Stat(d.Path); err != nil {
			v.FileExists = false
		}
	}
	return v
}

// ReleaseAnchor 解除一个应用的锚点。这是管理动作：锚点错了（比如用户自己在
// 应用里换了文档、或文件已删除）时，与其等下一次写入被守卫找回/拒绝，不如
// 让用户直接清掉——下次 AI 写入会就地重新锚定当前文档。
//
// 读-改-写：MCP 会话那边每次访问也重读磁盘，所以这里的解除对活着的会话立即生效。
// 幂等：锚点本来就不在（文件没了、键不在）就直接成功。
func (s *AppService) ReleaseAnchor(id string) error {
	path := filepath.Join(s.host.DataDir(), "anchors.json")
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读锚点文件失败: %w", err)
	}
	var file map[string]json.RawMessage
	if err := json.Unmarshal(raw, &file); err != nil {
		return fmt.Errorf("锚点文件损坏: %w", err)
	}
	if _, ok := file[id]; !ok {
		return nil
	}
	delete(file, id)
	if len(file) == 0 {
		if err := os.Remove(path); err != nil {
			return err
		}
	} else {
		out, err := json.MarshalIndent(file, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, out, 0o600); err != nil {
			return err
		}
	}
	s.journal.Append(journal.Entry{
		At: time.Now(), Source: "gui", Session: "gui", Kind: "anchor",
		AppID: id, Command: "release", OK: true,
		Detail: "用户在界面解除了文件锚点",
	})
	return nil
}

/* ---------------- 动作 ---------------- */

// Connect 接管一个已经在运行的实例，不启动新进程。
func (s *AppService) Connect(id string) (host.Status, error) {
	err := s.host.Connect(s.ctx, id)
	if err == nil {
		go s.refreshMenuQuiet(id)
	}
	return s.statusOf(id), err
}

// Launch 启动应用并等控制通道就绪。
func (s *AppService) Launch(id string) (host.Status, error) {
	err := s.host.Launch(s.ctx, id)
	// 用户可能刚装完/刚升级过这个应用，启动后重新读一次版本。
	s.rel.ForgetInstalled(id)
	if err == nil {
		go s.refreshMenuQuiet(id)
	}
	return s.statusOf(id), err
}

// Ensure 连接，必要时启动。
func (s *AppService) Ensure(id string) (host.Status, error) {
	_, err := s.host.Ensure(s.ctx, id)
	if err == nil {
		go s.refreshMenuQuiet(id)
	}
	return s.statusOf(id), err
}

// Disconnect 断开连接并结束由本进程启动的实例。
func (s *AppService) Disconnect(id string) host.Status {
	s.host.Stop(id)
	return s.statusOf(id)
}

// Screenshot 截图到数据目录，返回路径。
func (s *AppService) Screenshot(id string) (string, error) {
	client := s.host.Client(id)
	if client == nil {
		return "", fmt.Errorf("未连接")
	}
	path := filepath.Join(s.host.DataDir(), "shots", fmt.Sprintf("%s-%d.png", id, time.Now().Unix()))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if _, err := client.Screenshot(s.ctx, path); err != nil {
		return "", err
	}
	return path, nil
}

// RevealScreenshot 在文件管理器里定位截图。
func (s *AppService) RevealScreenshot(path string) error {
	return reveal(path)
}

/* ---------------- 版本与更新 ---------------- */

// UpdateState 是一次检查的总体情况，界面用它显示「上次检查」与失败原因。
type UpdateState struct {
	CheckedAt string   `json:"checkedAt"`
	Source    string   `json:"source"`
	Checked   int      `json:"checked"`
	Updates   int      `json:"updates"`
	Errors    []string `json:"errors"`
}

// UpdateState 返回缓存里的检查结果，不发网络请求。
func (s *AppService) UpdateState() UpdateState {
	return s.summarize(s.rel.Snapshot())
}

// CheckUpdates 查询最新发布。force=false 时 6 小时内的缓存直接复用。
//
// 这是用户点「检查更新」走的路：同步返回结果，失败也把原因带回去，
// 不要在界面上只留一个转圈。
func (s *AppService) CheckUpdates(force bool) (UpdateState, error) {
	ctx, cancel := context.WithTimeout(s.ctx, 45*time.Second)
	defer cancel()

	start := time.Now()
	snap, err := s.rel.Check(ctx, craft.Suite, force)
	state := s.summarize(snap)
	if err != nil {
		s.journal.Append(journal.Entry{
			At: time.Now(), Source: "gui", Session: "gui", Kind: "update",
			Command: "check", OK: false, Detail: err.Error(),
			Duration: time.Since(start).Milliseconds(),
		})
		return state, err
	}
	s.journal.Append(journal.Entry{
		At: time.Now(), Source: "gui", Session: "gui", Kind: "update",
		Command: "check", OK: len(snap.Errors) == 0,
		Detail:   fmt.Sprintf("apps=%d updates=%d errors=%d", state.Checked, state.Updates, len(state.Errors)),
		Duration: time.Since(start).Milliseconds(),
	})
	return state, nil
}

func (s *AppService) summarize(snap release.Snapshot) UpdateState {
	out := UpdateState{Source: snap.Source, Errors: snap.Errors}
	if !snap.CheckedAt.IsZero() {
		out.CheckedAt = snap.CheckedAt.Format(time.RFC3339)
	}
	for _, st := range s.Suite() {
		if _, ok := snap.Apps[st.App.ID]; ok {
			out.Checked++
		}
		if st.UpdateAvailable {
			out.Updates++
		}
	}
	return out
}

/* ---------------- 安装更新 ---------------- */

// UpdateProgress 是安装进度，经事件推给界面。
type UpdateProgress struct {
	AppID   string `json:"appId"`
	Step    string `json:"step"` // download | verify | install | done | manual
	Percent int    `json:"percent"`
	Bytes   int64  `json:"bytes"`
	Total   int64  `json:"total"`
	Note    string `json:"note,omitempty"`
}

// InstallResult 是一次安装的结果。Fresh 表示原本没装、这是第一次装；
// Manual 为 true 表示只下载并校验了安装包，真正的安装要用户自己在系统里完成。
type InstallResult struct {
	AppID      string `json:"appId"`
	Version    string `json:"version"`
	Path       string `json:"path"`
	Backup     string `json:"backup,omitempty"`
	Fresh      bool   `json:"fresh,omitempty"`
	Manual     bool   `json:"manual,omitempty"`
	Relaunched bool   `json:"relaunched"`
	DurationMs int64  `json:"durationMs"`
}

// CanAutoInstall 表示本平台能不能代用户完成安装。
func (s *AppService) CanAutoInstall() bool { return runtime.GOOS == "darwin" }

// Install 装上一个应用的最新发布：没装的装上，装了的换掉。
//
// 两条路共用同一段代码是有意的 —— 首次安装和升级在磁盘上做的是同一件事
// （下载 → 校验 → 挂载 → 验签 → 放到目标位置），唯一的区别是目标位置
// 来自现有副本还是默认目录，以及要不要留备份。
//
// 顺序是有讲究的：先确认有安装包、再退出应用、最后才换文件。
// 任何一步失败都停在原地 —— 磁盘上的那份要么是旧的、要么是新的，
// 不会出现「换了一半」的应用包。
func (s *AppService) Install(id string) (InstallResult, error) {
	app, ok := craft.ByID(id)
	if !ok {
		return InstallResult{}, fmt.Errorf("未知应用: %s", id)
	}
	if !s.beginInstall(id) {
		return InstallResult{}, fmt.Errorf("%s 正在安装中", app.Name)
	}
	defer s.endInstall(id)

	start := time.Now()
	res := InstallResult{AppID: id, Fresh: !app.Installed()}
	// 升级前的版本要在这里记：之后应用会退出、包会被换掉，再 Detect 就是新的了。
	// latest 是「要去的版本」而不是「现在装的版本」，拿它当 from 只在
	// 重复安装最新版时碰巧对 —— 正常升级会把 v0.13.0 → v0.14.0 记成 v0.14.0 → v0.14.0。
	before := release.Detect(app).Version
	fail := func(err error) (InstallResult, error) {
		// 带上出网方式：下载慢、连不上时，第一件要排除的就是「有没有走代理」。
		note := err.Error()
		if route := s.upd.Route(); route != "" {
			note += "（出网：" + route + "）"
		}
		s.journal.Append(journal.Entry{
			At: time.Now(), Source: "gui", Session: "gui", Kind: "update",
			AppID: id, Command: "install", OK: false, Detail: note,
			Duration: time.Since(start).Milliseconds(),
		})
		s.emit(UpdateProgress{AppID: id, Step: "error", Percent: -1, Note: note})
		return res, err
	}

	latest := s.rel.Snapshot().Apps[id]
	if latest.Version == "" || latest.Tag == "" {
		return fail(fmt.Errorf("还没有 %s 的版本信息，先点一次「检查更新」", app.Name))
	}

	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Minute)
	defer cancel()

	repo := s.rel.RepoFor(app)
	rel, err := s.upd.Release(ctx, repo, latest.Tag)
	if err != nil {
		return fail(err)
	}
	asset, err := update.Pick(app, rel)
	if err != nil {
		return fail(err)
	}
	if asset, err = s.upd.EnsureSum(ctx, repo, rel, asset); err != nil {
		return fail(err)
	}

	// 应用正在运行时先请它退出：AppleScript 的 quit 会给它机会保存用户的工作。
	wasOnline := s.isOnline(id)
	if err := update.QuitApp(app); err != nil {
		return fail(err)
	}
	s.host.Stop(id)

	out, err := update.Install(ctx, s.upd, update.Options{
		App:     app,
		Asset:   asset,
		WorkDir: update.WorkDir(s.host.DataDir()),
		Version: rel.Version,
		OnStage: func(st update.Stage) {
			s.emit(UpdateProgress{
				AppID: id, Step: st.Step, Percent: st.Percent,
				Bytes: st.Bytes, Total: st.Total, Note: st.Note,
			})
		},
	})
	if err != nil {
		return fail(err)
	}

	// 刚换了磁盘上的包，版本与菜单缓存都必须作废，否则界面还会显示旧内容。
	s.rel.ForgetInstalled(id)
	s.menus.Forget(id)
	res.Version = out.Version
	res.Path = out.Path
	res.Backup = out.Backup
	res.Manual = out.Manual
	res.DurationMs = time.Since(start).Milliseconds()

	if out.Manual && out.Path != "" {
		// 本平台只下载不安装：把安装包直接交给系统，用户少找一次文件。
		_ = openItem(out.Path)
	}
	if wasOnline && !out.Manual {
		// 更新前它是开着的，装完就还给它 —— 没人希望点一次更新，应用反而被关掉。
		if err := s.host.Launch(s.ctx, id); err == nil {
			res.Relaunched = true
			go s.refreshMenuQuiet(id)
		}
	}

	var detail string
	switch {
	case res.Fresh:
		detail = "安装 v" + out.Version
	case before == "":
		// 装了但读不到旧版本（Info.plist 缺字段之类）：如实说不知道，别拿新版本冒充。
		detail = "升级到 v" + out.Version + "（原版本未能读取）"
	default:
		detail = fmt.Sprintf("v%s → v%s", before, out.Version)
	}
	s.journal.Append(journal.Entry{
		At: time.Now(), Source: "gui", Session: "gui", Kind: "update",
		AppID: id, Command: "install", OK: true,
		Detail:   detail,
		Duration: res.DurationMs,
	})
	return res, nil
}

func (s *AppService) beginInstall(id string) bool {
	s.installMu.Lock()
	defer s.installMu.Unlock()
	if s.installing[id] {
		return false
	}
	s.installing[id] = true
	return true
}

func (s *AppService) endInstall(id string) {
	s.installMu.Lock()
	delete(s.installing, id)
	s.installMu.Unlock()
}

func (s *AppService) isOnline(id string) bool {
	for _, st := range s.host.Status() {
		if st.App.ID == id {
			return st.Connected
		}
	}
	return false
}

func (s *AppService) emit(p UpdateProgress) {
	if s.Emit != nil {
		s.Emit(p)
	}
}

// OpenRelease 在浏览器里打开发布页。版本信息来自网络，只接受 http(s)。
func (s *AppService) OpenRelease(id string) error {
	url := ""
	for _, st := range s.Suite() {
		if st.App.ID == id {
			url = st.LatestURL
			break
		}
	}
	if url == "" {
		return fmt.Errorf("还没有这个应用的发布地址，先检查一次更新")
	}
	if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
		return fmt.Errorf("发布地址不是 http(s)，已拒绝打开")
	}
	return openURL(url)
}

func openURL(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

/* ---------------- 会话流 ---------------- */

// Journal 返回最近的操作流水，最新的在前。
func (s *AppService) Journal(limit int) []journal.Entry {
	if limit <= 0 {
		limit = 120
	}
	entries, err := s.journal.Read(limit)
	if err != nil {
		return nil
	}
	out := make([]journal.Entry, 0, len(entries))
	for i := len(entries) - 1; i >= 0; i-- {
		out = append(out, entries[i])
	}
	return out
}

// Sessions 汇总仍在线的 AI 会话：有 session-start 但还没有 session-end 的。
func (s *AppService) Sessions() []SessionView {
	entries, err := s.journal.Read(0)
	if err != nil {
		return nil
	}
	type acc struct {
		view SessionView
		apps map[string]bool
	}
	open := map[string]*acc{}
	var order []string

	for _, e := range entries {
		if e.Source != "mcp" || e.Session == "" {
			continue
		}
		switch e.Kind {
		case "session-start":
			open[e.Session] = &acc{
				view: SessionView{Session: e.Session, StartedAt: e.At.Format(time.RFC3339)},
				apps: map[string]bool{},
			}
			order = append(order, e.Session)
		case "session-end":
			delete(open, e.Session)
		default:
			a, ok := open[e.Session]
			if !ok {
				continue
			}
			a.view.Actions++
			a.view.LastActive = e.At.Format(time.RFC3339)
			if e.AppID != "" {
				a.apps[e.AppID] = true
			}
		}
	}

	out := make([]SessionView, 0, len(open))
	for _, id := range order {
		a, ok := open[id]
		if !ok {
			continue
		}
		for app := range a.apps {
			a.view.Apps = append(a.view.Apps, app)
		}
		sort.Strings(a.view.Apps)
		if a.view.LastActive == "" {
			a.view.LastActive = a.view.StartedAt
		}
		out = append(out, a.view)
	}
	return out
}

/* ---------------- 交给第三方 AI 的配置 ---------------- */

// MCPConfig 生成可直接粘贴进 mcp.json 的片段。
func (s *AppService) MCPConfig() string {
	exe, err := os.Executable()
	if err != nil {
		exe = "craft-agent"
	}
	cfg := map[string]any{
		"mcpServers": map[string]any{
			"craft-agent": map[string]any{
				"command": exe,
				"args":    []string{"mcp"},
			},
		},
	}
	raw, _ := json.MarshalIndent(cfg, "", "  ")
	return string(raw)
}

// DataDir 是数据目录，界面显示「流水和注册表存在哪」。
func (s *AppService) DataDir() string { return s.host.DataDir() }

// OpenDataDir 在系统文件管理器里打开数据目录。
func (s *AppService) OpenDataDir() error { return reveal(s.host.DataDir()) }

// Version 返回版本号。
func (s *AppService) Version() string { return Version }

// SetTrayLanguage 让托盘菜单跟上界面语言。那份菜单画在系统 chrome 里，
// 页面上的 i18n 够不着它，只能由前端把当前语言报过来。
func (s *AppService) SetTrayLanguage(lang string) { tray.SetLanguage(lang) }

/* ---------------- MCP 运行时 ---------------- */

// MCPSessionView 是一个 MCP 执行端会话的运行状态。MCP 进程由 AI 客户端拉起，
// 和主程序是两个进程——主程序升级后旧会话还挂着旧版本，界面要能看见这件事。
type MCPSessionView struct {
	Session   string   `json:"session"`
	PID       int      `json:"pid"`
	Version   string   `json:"version"`
	StartedAt string   `json:"startedAt"`
	Apps      []string `json:"apps"`
	Alive     bool     `json:"alive"`
	// Outdated：会话版本与当前主程序不一致。升级主程序后旧会话继续跑旧代码，
	// 直到用户重启 AI 客户端——这条提示就是那个重启按钮的替代品。
	Outdated bool `json:"outdated"`
}

// MCPRuntime 返回落盘的 MCP 会话与它们的活性/版本状态。
// 死掉超过 24 小时的条目在读时顺手清走——进程被 kill -9 时注销代码不会执行，
// 不清理的话 runtime.json 会积累僵尸条目。
func (s *AppService) MCPRuntime() []MCPSessionView {
	raw, err := os.ReadFile(filepath.Join(s.host.DataDir(), "runtime.json"))
	if err != nil {
		return nil
	}
	var file struct {
		Sessions map[string]struct {
			PID       int       `json:"pid"`
			Version   string    `json:"version"`
			StartedAt time.Time `json:"startedAt"`
			Apps      []string  `json:"apps"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil
	}
	if len(file.Sessions) == 0 {
		return nil
	}

	out := make([]MCPSessionView, 0, len(file.Sessions))
	dead := []string{}
	for id, e := range file.Sessions {
		alive := pidAlive(e.PID)
		if !alive && time.Since(e.StartedAt) > 24*time.Hour {
			dead = append(dead, id)
		}
		out = append(out, MCPSessionView{
			Session:   id,
			PID:       e.PID,
			Version:   e.Version,
			StartedAt: e.StartedAt.Format("2006-01-02 15:04"),
			Apps:      e.Apps,
			Alive:     alive,
			Outdated:  e.Version != "" && e.Version != Version,
		})
	}
	sort.Strings(dead)
	for _, id := range dead {
		// 注意形状：文件是 {"sessions": {...}}，要删的是 sessions 里那一层。
		var f struct {
			Sessions map[string]json.RawMessage `json:"sessions"`
		}
		raw2, err := os.ReadFile(filepath.Join(s.host.DataDir(), "runtime.json"))
		if err != nil {
			continue
		}
		if json.Unmarshal(raw2, &f) != nil || f.Sessions == nil {
			continue
		}
		delete(f.Sessions, id)
		if len(f.Sessions) == 0 {
			_ = os.Remove(filepath.Join(s.host.DataDir(), "runtime.json"))
			continue
		}
		if outBytes, err := json.MarshalIndent(f, "", "  "); err == nil {
			_ = os.WriteFile(filepath.Join(s.host.DataDir(), "runtime.json"), outBytes, 0o600)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Session < out[j].Session })
	return out
}

/* ---------------- 内部 ---------------- */

func (s *AppService) statusOf(id string) host.Status {
	// 走 Suite 而不是 host.Status，让启动/连接后返回的那一份也带版本信息。
	for _, st := range s.Suite() {
		if st.App.ID == id {
			return st
		}
	}
	app, _ := craft.ByID(id)
	return host.Status{App: app}
}

// openItem 用系统默认程序打开一个文件（macOS 上等价于双击）。
func openItem(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	return cmd.Start()
}

func reveal(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", "-R", path)
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			cmd = exec.Command("open", path)
		}
	case "windows":
		cmd = exec.Command("explorer", "/select,", path)
	default:
		cmd = exec.Command("xdg-open", filepath.Dir(path))
	}
	return cmd.Start()
}

// summarizeDocument 把不同应用的文档结构压成同一套摘要。
//
// 各应用 inspect 结果的形状不同（DeckCraft 给 slides/shapes，别的应用给自己的结构），
// 所以按已知键尽力提取，取不到就留空 —— 不假装统一。
func summarizeDocument(raw json.RawMessage) DocumentView {
	view := DocumentView{}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return view
	}
	if p, ok := doc["path"].(string); ok {
		view.Path = p
	}
	if dirty, ok := doc["dirty"].(bool); ok {
		view.Dirty = dirty
	}
	for _, key := range []string{"slides", "pages", "sheets", "sections", "scenes"} {
		items, ok := doc[key].([]any)
		if !ok {
			continue
		}
		for _, item := range items {
			switch v := item.(type) {
			case map[string]any:
				name := firstString(v, "title", "name", "label")
				if name != "" {
					view.Sections = append(view.Sections, name)
				}
			case string:
				view.Sections = append(view.Sections, v)
			}
		}
		break
	}
	for _, key := range []string{"shapes", "objects", "items", "elements"} {
		if items, ok := doc[key].([]any); ok && view.Objects == 0 {
			view.Objects = len(items)
			break
		}
	}
	if sel, ok := doc["selection"].(map[string]any); ok {
		if ids, ok := sel["ids"].([]any); ok && len(ids) > 0 {
			parts := make([]string, 0, len(ids))
			for _, id := range ids {
				parts = append(parts, fmt.Sprintf("%v", id))
			}
			view.Selection = strings.Join(parts, ", ")
		}
	}
	view.Summary = fmt.Sprintf("%d 页 · %d 个对象", len(view.Sections), view.Objects)
	return view
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}
