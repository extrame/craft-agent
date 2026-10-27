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
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"craft-agent/internal/craft"
	"craft-agent/internal/host"
	"craft-agent/internal/journal"
	"craft-agent/internal/release"
	"craft-agent/internal/update"
)

// Version 由 main 注入。
var Version = "0.1.0"

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

	installMu  sync.Mutex
	installing map[string]bool

	// Emit 把安装进度推给界面；由 main 接上 Wails 事件总线。
	Emit func(UpdateProgress)

	ctx    context.Context
	cancel context.CancelFunc
}

// NewAppService 构造绑定层。
func NewAppService(h *host.Host, j *journal.Journal) *AppService {
	ctx, cancel := context.WithCancel(context.Background())
	return &AppService{
		host:       h,
		journal:    j,
		rel:        release.NewChecker(h.DataDir(), Version),
		upd:        update.NewClient("Craft-Agent/" + Version),
		installing: map[string]bool{},
		ctx:        ctx,
		cancel:     cancel,
	}
} // ServiceStartup 由 Wails 在应用启动时调用。
func (s *AppService) ServiceStartup(ctx context.Context) {
	s.ctx, s.cancel = context.WithCancel(ctx)
}

// ServiceShutdown 在窗口关闭时断开所有通道（应用进程留给用户）。
func (s *AppService) ServiceShutdown() {
	s.cancel()
	s.host.Close()
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

/* ---------------- 动作 ---------------- */

// Connect 接管一个已经在运行的实例，不启动新进程。
func (s *AppService) Connect(id string) (host.Status, error) {
	err := s.host.Connect(s.ctx, id)
	return s.statusOf(id), err
}

// Launch 启动应用并等控制通道就绪。
func (s *AppService) Launch(id string) (host.Status, error) {
	err := s.host.Launch(s.ctx, id)
	// 用户可能刚装完/刚升级过这个应用，启动后重新读一次版本。
	s.rel.ForgetInstalled(id)
	return s.statusOf(id), err
}

// Ensure 连接，必要时启动。
func (s *AppService) Ensure(id string) (host.Status, error) {
	_, err := s.host.Ensure(s.ctx, id)
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

// InstallResult 是一次安装的结果。Manual 为 true 表示只下载并校验了安装包，
// 真正的安装要用户自己在系统里完成。
type InstallResult struct {
	AppID      string `json:"appId"`
	Version    string `json:"version"`
	Path       string `json:"path"`
	Backup     string `json:"backup,omitempty"`
	Manual     bool   `json:"manual,omitempty"`
	Relaunched bool   `json:"relaunched"`
	DurationMs int64  `json:"durationMs"`
}

// CanAutoInstall 表示本平台能不能代用户完成安装。
func (s *AppService) CanAutoInstall() bool { return runtime.GOOS == "darwin" }

// InstallUpdate 下载并安装某个应用的最新发布。
//
// 顺序是有讲究的：先确认有安装包、再退出应用、最后才换文件。
// 任何一步失败都停在原地 —— 磁盘上的那份要么是旧的、要么是新的，
// 不会出现「换了一半」的应用包。
func (s *AppService) InstallUpdate(id string) (InstallResult, error) {
	app, ok := craft.ByID(id)
	if !ok {
		return InstallResult{}, fmt.Errorf("未知应用: %s", id)
	}
	if !s.beginInstall(id) {
		return InstallResult{}, fmt.Errorf("%s 正在更新中", app.Name)
	}
	defer s.endInstall(id)

	start := time.Now()
	res := InstallResult{AppID: id}
	fail := func(err error) (InstallResult, error) {
		s.journal.Append(journal.Entry{
			At: time.Now(), Source: "gui", Session: "gui", Kind: "update",
			AppID: id, Command: "install", OK: false, Detail: err.Error(),
			Duration: time.Since(start).Milliseconds(),
		})
		s.emit(UpdateProgress{AppID: id, Step: "error", Percent: -1, Note: err.Error()})
		return res, err
	}

	latest := s.rel.Snapshot().Apps[id]
	if latest.Version == "" || latest.Tag == "" {
		return fail(fmt.Errorf("还没有 %s 的最新版本信息，先点一次「检查更新」", app.Name))
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

	// 刚换了磁盘上的包，版本缓存必须作废，否则界面还会显示旧版本。
	s.rel.ForgetInstalled(id)
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
		}
	}

	s.journal.Append(journal.Entry{
		At: time.Now(), Source: "gui", Session: "gui", Kind: "update",
		AppID: id, Command: "install", OK: true,
		Detail:   fmt.Sprintf("v%s → v%s", latest.Version, out.Version),
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
