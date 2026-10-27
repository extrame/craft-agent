// Package host 负责 craft 应用的发现、启动、控制通道连接，并把「哪个应用开在哪个
// 端点上」写进磁盘上的注册表。
//
// 为什么要注册表：GUI 与 MCP 是两个独立进程。GUI 负责拉起应用、分配端口；
// 被 Cursor/Claude 拉起的 MCP 进程要能接管同一个实例，只能通过这份文件对齐。
// 因此 MCP 模式不依赖 GUI 正在运行 —— 读不到注册表时退回默认端点探测。
package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"craft-agent/internal/control"
	"craft-agent/internal/craft"
	"craft-agent/internal/journal"
)

// Event 是给界面看的活动记录。MCP 模式下没有订阅者，只留在内存里。
type Event struct {
	At      time.Time `json:"at"`
	AppID   string    `json:"appId"`
	Kind    string    `json:"kind"` // launch | connect | disconnect | command | error
	Message string    `json:"message"`
}

// Entry 是注册表里的一项：应用当前开在哪个端点、进程号是多少。
//
// Token 单独放一层：Endpoint 会随状态快照一路推到界面和 MCP 工具结果里，
// 令牌不该出现在那儿；但 MCP 进程要能接管 GUI 拉起的实例，又必须拿到它。
// 所以令牌只落在这份 0600 的注册表文件里。
type Entry struct {
	Endpoint  control.Endpoint `json:"endpoint"`
	Token     string           `json:"token,omitempty"`
	PID       int              `json:"pid"`
	Version   string           `json:"version,omitempty"`
	UpdatedAt time.Time        `json:"updatedAt"`
}

// Registry 是磁盘上的跨进程约定。
type Registry struct {
	Apps map[string]Entry `json:"apps"`
}

// Status 是界面与 MCP 工具共用的应用状态。
//
// 版本与更新这几项由服务层填充（host 只管进程，不查网络）；MCP 侧看到的这几个字段为空，
// 因为插件的职责是控制，不是升级。
type Status struct {
	App       craft.App        `json:"app"`
	Installed bool             `json:"installed"`
	Path      string           `json:"path,omitempty"`
	Endpoint  control.Endpoint `json:"endpoint"`
	Connected bool             `json:"connected"`
	PID       int              `json:"pid,omitempty"`
	Version   string           `json:"version,omitempty"`
	Control   string           `json:"control"`
	Error     string           `json:"error,omitempty"`

	// 本机这份副本的版本，从 bundle / exe / --version 读到。
	InstalledVersion string `json:"installedVersion,omitempty"`
	VersionSource    string `json:"versionSource,omitempty"`
	VersionError     string `json:"versionError,omitempty"`

	// 远端最新发布。
	LatestVersion    string `json:"latestVersion,omitempty"`
	LatestURL        string `json:"latestUrl,omitempty"`
	LatestNotes      string `json:"latestNotes,omitempty"`
	LatestPrerelease bool   `json:"latestPrerelease,omitempty"`
	UpdateAvailable  bool   `json:"updateAvailable"`
	UpdateError      string `json:"updateError,omitempty"`
}

// Options 是宿主的装配参数。
type Options struct {
	DataDir       string
	OnEvent       func(Event)
	LaunchTimeout time.Duration // 启动后等待控制通道就绪的上限，默认 15s
	CallTimeout   time.Duration // 单次控制请求超时，默认 20s

	// Journal 记录操作流水；Source/Session 标明这些动作来自哪个进程，
	// 界面靠它区分「我点的」和「agent 做的」。
	Journal *journal.Journal
	Source  string // gui | mcp
	Session string
}

// Host 管理所有应用的运行时状态。字段级的锁分两层：host.mu 只保护 map，
// instance.mu 串行化同一个应用的连接/启动动作。
type Host struct {
	opts Options

	mu        sync.Mutex
	instances map[string]*instance
	events    []Event
}

type instance struct {
	app craft.App

	mu      sync.Mutex
	client  *control.Client
	cmd     *exec.Cmd
	ep      control.Endpoint
	pid     int
	version string
	lastErr string
}

// New 构造宿主。DataDir 为空时用系统默认目录。
func New(opts Options) (*Host, error) {
	if opts.DataDir == "" {
		dir, err := DefaultDataDir()
		if err != nil {
			return nil, err
		}
		opts.DataDir = dir
	}
	if opts.LaunchTimeout <= 0 {
		opts.LaunchTimeout = 15 * time.Second
	}
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = 20 * time.Second
	}
	if err := os.MkdirAll(filepath.Join(opts.DataDir, "run"), 0o755); err != nil {
		return nil, fmt.Errorf("创建运行目录失败: %w", err)
	}
	return &Host{opts: opts, instances: map[string]*instance{}}, nil
}

// DataDir 返回宿主的数据目录。
func (h *Host) DataDir() string { return h.opts.DataDir }

// DefaultDataDir 按平台给出数据目录。
func DefaultDataDir() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", "CraftAgent"), nil
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "CraftAgent"), nil
		}
	}
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "craft-agent"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "craft-agent"), nil
}

func (h *Host) emit(e Event) {
	e.At = time.Now()
	h.mu.Lock()
	h.events = append(h.events, e)
	if len(h.events) > 300 {
		h.events = h.events[len(h.events)-300:]
	}
	h.mu.Unlock()
	if h.opts.Journal != nil {
		h.opts.Journal.Append(journal.Entry{
			At:      e.At,
			Source:  h.opts.Source,
			Session: h.opts.Session,
			AppID:   e.AppID,
			Kind:    e.Kind,
			OK:      e.Kind != "error",
			Detail:  e.Message,
		})
	}
	if h.opts.OnEvent != nil {
		h.opts.OnEvent(e)
	}
}

// Events 返回最近的活动，最新的在最后。
func (h *Host) Events() []Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Event, len(h.events))
	copy(out, h.events)
	return out
}

func (h *Host) instance(app craft.App) *instance {
	h.mu.Lock()
	defer h.mu.Unlock()
	inst, ok := h.instances[app.ID]
	if !ok {
		inst = &instance{app: app}
		h.instances[app.ID] = inst
	}
	return inst
}

// Status 汇总所有（或指定）应用的状态。不发起网络探测，只看已建立的连接与注册表。
func (h *Host) Status() []Status {
	reg, _ := h.readRegistry()
	out := make([]Status, 0, len(craft.Suite))
	for _, app := range craft.Suite {
		inst := h.instance(app)
		inst.mu.Lock()
		st := Status{
			App:       app,
			Installed: app.Installed(),
			Path:      app.InstalledPath(),
			Endpoint:  inst.ep,
			PID:       inst.pid,
			Version:   inst.version,
			Control:   app.DescribeControl(),
			Error:     inst.lastErr,
		}
		if inst.client != nil && inst.client.Alive() {
			st.Connected = true
		}
		if st.Endpoint.Network == "" {
			if e, ok := reg.Apps[app.ID]; ok {
				st.Endpoint = e.Endpoint
			}
		}
		inst.mu.Unlock()
		out = append(out, st)
	}
	return out
}

// Connect 连接一个已经在运行的实例（不启动新进程）。
func (h *Host) Connect(ctx context.Context, appID string) error {
	app, ok := craft.ByID(appID)
	if !ok {
		return fmt.Errorf("未知应用: %s", appID)
	}
	inst := h.instance(app)
	inst.mu.Lock()
	defer inst.mu.Unlock()

	if inst.client != nil && inst.client.Alive() {
		return nil
	}
	eps := h.candidateEndpoints(app)
	var lastErr error
	for _, ep := range eps {
		client, err := control.Dial(ctx, ep, h.opts.CallTimeout)
		if err != nil {
			lastErr = err
			continue
		}
		inst.client = client
		inst.ep = ep
		inst.lastErr = ""
		h.writeRegistry(app.ID, Entry{Endpoint: ep, PID: inst.pid, UpdatedAt: time.Now()})
		h.emit(Event{AppID: app.ID, Kind: "connect", Message: ep.String()})
		return nil
	}
	if lastErr == nil {
		lastErr = errors.New("没有可用的控制端点")
	}
	inst.lastErr = lastErr.Error()
	return fmt.Errorf("连接 %s 失败: %w", app.Name, lastErr)
}

// Ensure 保证应用可用：已连接直接返回；否则连接已注册/默认端点；再不行就启动进程。
func (h *Host) Ensure(ctx context.Context, appID string) (*control.Client, error) {
	app, ok := craft.ByID(appID)
	if !ok {
		return nil, fmt.Errorf("未知应用: %s", appID)
	}
	inst := h.instance(app)
	inst.mu.Lock()
	defer inst.mu.Unlock()

	if inst.client != nil && inst.client.Alive() {
		return inst.client, nil
	}
	if client, _ := h.attachLocked(ctx, inst); client != nil {
		return client, nil
	}
	return h.launchLocked(ctx, inst)
}

// attachLocked 依次尝试注册表端点、默认端点。命中即登记。
func (h *Host) attachLocked(ctx context.Context, inst *instance) (*control.Client, control.Endpoint) {
	for _, ep := range h.candidateEndpoints(inst.app) {
		client, err := control.Dial(ctx, ep, 1500*time.Millisecond)
		if err != nil {
			continue
		}
		inst.client = client
		inst.ep = ep
		inst.lastErr = ""
		h.writeRegistry(inst.app.ID, Entry{Endpoint: ep, PID: inst.pid, UpdatedAt: time.Now()})
		h.emit(Event{AppID: inst.app.ID, Kind: "connect", Message: ep.String()})
		return client, ep
	}
	return nil, control.Endpoint{}
}

// Launch 强制启动一个新进程（已有连接则先断开）。
func (h *Host) Launch(ctx context.Context, appID string) error {
	app, ok := craft.ByID(appID)
	if !ok {
		return fmt.Errorf("未知应用: %s", appID)
	}
	inst := h.instance(app)
	inst.mu.Lock()
	defer inst.mu.Unlock()
	if inst.client != nil {
		_ = inst.client.Close()
		inst.client = nil
	}
	_, err := h.launchLocked(ctx, inst)
	return err
}

func (h *Host) launchLocked(ctx context.Context, inst *instance) (*control.Client, error) {
	app := inst.app
	exe := app.InstalledPath()
	if exe == "" {
		inst.lastErr = "未安装"
		return nil, fmt.Errorf("未找到 %s：检查 %s", app.Name, strings.Join(app.Executables(), " 或 "))
	}

	base, arg, err := h.prepareTarget(app)
	if err != nil {
		inst.lastErr = err.Error()
		return nil, err
	}
	args := app.ControlArgs(arg)
	cmd := exec.Command(exe, args...)
	cmd.Dir = filepath.Dir(exe)
	// 应用自己的日志落到数据目录，**不要**接到本进程的 stdout/stderr：
	// 命令行模式下那会把调用方的管道一直占着 —— 我们退出了，读端还在等 EOF。
	if logFile, err := h.openLog(app); err == nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		defer logFile.Close()
	}
	if err := cmd.Start(); err != nil {
		inst.lastErr = err.Error()
		return nil, fmt.Errorf("启动 %s 失败: %w", app.Name, err)
	}
	inst.cmd = cmd
	inst.pid = cmd.Process.Pid
	inst.ep = base
	h.emit(Event{AppID: app.ID, Kind: "launch", Message: fmt.Sprintf("PID %d · %s", cmd.Process.Pid, app.DescribeControl())})

	// 进程退出时清掉连接，避免界面显示「已连接」但其实是死通道。
	go func(cmd *exec.Cmd) {
		_ = cmd.Wait()
		inst.mu.Lock()
		defer inst.mu.Unlock()
		if inst.cmd == cmd {
			inst.cmd = nil
			inst.pid = 0
			if inst.client != nil {
				_ = inst.client.Close()
				inst.client = nil
			}
			h.emit(Event{AppID: app.ID, Kind: "disconnect", Message: "exited"})
		}
	}(cmd)

	rendezvous := ""
	if app.Control == craft.ControlFile {
		rendezvous = arg
	}
	var lastErr error
	deadline := time.Now().Add(h.opts.LaunchTimeout)
	for {
		if ctx.Err() != nil {
			inst.lastErr = ctx.Err().Error()
			return nil, ctx.Err()
		}
		if ep, err := h.liveEndpoint(app, base, rendezvous); err == nil {
			client, derr := control.Dial(ctx, ep, 1200*time.Millisecond)
			if derr == nil {
				inst.client = client
				inst.ep = ep
				inst.lastErr = ""
				h.writeRegistry(app.ID, Entry{
					Endpoint:  ep,
					PID:       inst.pid,
					Version:   inst.version,
					UpdatedAt: time.Now(),
				})
				h.emit(Event{AppID: app.ID, Kind: "connect", Message: ep.String()})
				return client, nil
			}
			lastErr = derr
			// 鉴权失败不立刻判死：应用刚起来时令牌文件可能还是上一次的，
			// 每轮都会重读，等它写新的。等不到再由下面的超时分支报出来。
		}
		if time.Now().After(deadline) {
			msg := fmt.Sprintf("%s 已启动但控制通道未就绪（%s；%s）",
				app.Name, app.DescribeControl(), describeLaunchFailure(app, base, lastErr))
			inst.lastErr = msg
			h.emit(Event{AppID: app.ID, Kind: "error", Message: msg})
			return nil, errors.New(msg)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// openLog 给应用进程的 stdout/stderr 找个落点。
func (h *Host) openLog(app craft.App) (*os.File, error) {
	dir := filepath.Join(h.opts.DataDir, "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(filepath.Join(dir, app.ID+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

func describeLaunchFailure(app craft.App, base control.Endpoint, err error) string {
	if err == nil {
		return "没有任何端点可试"
	}
	var authErr *control.AuthError
	if errors.As(err, &authErr) {
		if _, port, perr := splitHostPort(base.Address); perr == nil {
			if n, _ := strconv.Atoi(port); n > 0 {
				return fmt.Sprintf("控制通道要求令牌，但 %s 里的令牌不被接受（%v）", app.TokenPath(n), err)
			}
		}
		return fmt.Sprintf("控制通道要求令牌，但令牌不被接受（%v）", err)
	}
	return err.Error()
}

// rendezvousPath 是 ControlFile 模式下交给应用的会合文件路径。
//
// 名字里刻意没有 .sock：里面是 JSON（pid/port/token），不是 socket。
func (h *Host) rendezvousPath(appID string) string {
	return filepath.Join(h.opts.DataDir, "run", appID+".control")
}

// Rendezvous 是 ControlFile 模式下应用写出来的会合信息。
type Rendezvous struct {
	PID   int    `json:"pid"`
	Port  int    `json:"port"`
	Token string `json:"token"`
}

func readRendezvous(path string) (Rendezvous, error) {
	var rv Rendezvous
	raw, err := os.ReadFile(path)
	if err != nil {
		return rv, err
	}
	if err := json.Unmarshal(raw, &rv); err != nil {
		return rv, fmt.Errorf("会合文件不是合法 JSON: %w", err)
	}
	return rv, nil
}

// readToken 读端口模式下应用写的令牌文件。没有就是没有 —— 老一代应用不写。
func readToken(app craft.App, port string) string {
	n, err := strconv.Atoi(port)
	if err != nil || n <= 0 {
		return ""
	}
	for _, path := range app.TokenPaths(n) {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if token := strings.TrimSpace(string(raw)); token != "" {
			return token
		}
	}
	return ""
}

// prepareTarget 为一次新启动决定端点与 --control 的参数。
func (h *Host) prepareTarget(app craft.App) (control.Endpoint, string, error) {
	if app.Control == craft.ControlFile {
		path := h.rendezvousPath(app.ID)
		_ = os.Remove(path)
		return control.Endpoint{Network: control.NetworkTCP}, path, nil
	}
	start := app.Port
	if start <= 0 {
		start = 7990
	}
	for port := start; port < start+40; port++ {
		if portFree(port) {
			return control.Endpoint{Network: control.NetworkTCP, Address: fmt.Sprintf("127.0.0.1:%d", port)}, strconv.Itoa(port), nil
		}
	}
	return control.Endpoint{}, "", errors.New("找不到空闲端口")
}

// liveEndpoint 把「刚启动的应用」解析成真正可拨的端点。
//
// 端口模式：端口是我们给的，令牌文件要等应用写出来，所以每轮都重读。
// 文件模式：端口和令牌都在会合文件里，读不到说明还没起来。
func (h *Host) liveEndpoint(app craft.App, base control.Endpoint, rendezvous string) (control.Endpoint, error) {
	if app.Control == craft.ControlFile {
		rv, err := readRendezvous(rendezvous)
		if err != nil {
			return control.Endpoint{}, err
		}
		if rv.Port <= 0 {
			return control.Endpoint{}, errors.New("会合文件里没有端口")
		}
		return control.Endpoint{
			Network: control.NetworkTCP,
			Address: fmt.Sprintf("127.0.0.1:%d", rv.Port),
			Token:   rv.Token,
		}, nil
	}
	ep := base
	if _, port, err := splitHostPort(base.Address); err == nil {
		ep.Token = readToken(app, port)
	}
	return ep, nil
}

// candidateEndpoints 给出可以尝试连接的端点，按可信度排序：
// 注册表里记录的 → 应用默认端口（端口模式）或会合文件（文件模式）。
func (h *Host) candidateEndpoints(app craft.App) []control.Endpoint {
	var out []control.Endpoint
	seen := map[string]bool{}
	add := func(ep control.Endpoint) {
		if ep.Address == "" || seen[ep.Network+ep.Address] {
			return
		}
		seen[ep.Network+ep.Address] = true
		out = append(out, ep)
	}
	if reg, err := h.readRegistry(); err == nil {
		if e, ok := reg.Apps[app.ID]; ok {
			add(e.Endpoint)
		}
	}
	switch app.Control {
	case craft.ControlPort:
		if app.Port > 0 {
			ep := control.Endpoint{
				Network: control.NetworkTCP,
				Address: fmt.Sprintf("127.0.0.1:%d", app.Port),
			}
			ep.Token = readToken(app, fmt.Sprint(app.Port))
			add(ep)
		}
	case craft.ControlFile:
		if rv, err := readRendezvous(h.rendezvousPath(app.ID)); err == nil && rv.Port > 0 {
			add(control.Endpoint{
				Network: control.NetworkTCP,
				Address: fmt.Sprintf("127.0.0.1:%d", rv.Port),
				Token:   rv.Token,
			})
		}
	}
	return out
}

// Stop 断开连接；如果进程是我们拉起的，一并结束。
func (h *Host) Stop(appID string) {
	app, ok := craft.ByID(appID)
	if !ok {
		return
	}
	inst := h.instance(app)
	inst.mu.Lock()
	defer inst.mu.Unlock()
	if inst.client != nil {
		_ = inst.client.Close()
		inst.client = nil
	}
	if inst.cmd != nil && inst.cmd.Process != nil {
		_ = inst.cmd.Process.Kill()
		inst.cmd = nil
		inst.pid = 0
	}
	h.emit(Event{AppID: app.ID, Kind: "disconnect", Message: ""})
}

// Client 返回已建立的连接，没有则返回 nil。
func (h *Host) Client(appID string) *control.Client {
	app, ok := craft.ByID(appID)
	if !ok {
		return nil
	}
	inst := h.instance(app)
	inst.mu.Lock()
	defer inst.mu.Unlock()
	if inst.client != nil && inst.client.Alive() {
		return inst.client
	}
	return nil
}

// Close 断开本进程持有的全部控制通道。
//
// 刻意不结束应用进程：Craft Agent 退出（或 MCP 会话结束）之后，用户屏幕上的
// DeckCraft 应该还开着 —— 那是他的工作，不是我们的子窗口。
// 要结束进程走 Stop。
func (h *Host) Close() {
	h.mu.Lock()
	ids := make([]string, 0, len(h.instances))
	for id := range h.instances {
		ids = append(ids, id)
	}
	h.mu.Unlock()
	sort.Strings(ids)
	for _, id := range ids {
		app, ok := craft.ByID(id)
		if !ok {
			continue
		}
		inst := h.instance(app)
		inst.mu.Lock()
		if inst.client != nil {
			_ = inst.client.Close()
			inst.client = nil
		}
		inst.mu.Unlock()
	}
}

// ---- 注册表 ----

func (h *Host) registryPath() string {
	return filepath.Join(h.opts.DataDir, "registry.json")
}

func (h *Host) readRegistry() (Registry, error) {
	reg := Registry{Apps: map[string]Entry{}}
	raw, err := os.ReadFile(h.registryPath())
	if err != nil {
		if os.IsNotExist(err) {
			return reg, nil
		}
		return reg, err
	}
	if err := json.Unmarshal(raw, &reg); err != nil {
		return Registry{Apps: map[string]Entry{}}, err
	}
	if reg.Apps == nil {
		reg.Apps = map[string]Entry{}
	}
	// 端点自己不带令牌（它会被推到界面），令牌存在 Entry 上；用的时候合回去。
	for id, e := range reg.Apps {
		if e.Token != "" {
			e.Endpoint.Token = e.Token
			reg.Apps[id] = e
		}
	}
	return reg, nil
}

// writeRegistry 用「读-改-写」更新一项。GUI 与 MCP 进程都可能写，所以加文件锁太重，
// 这里接受最后写入者胜出：丢掉的最坏情况只是一条过期端点，下一次连接会自愈。
func (h *Host) writeRegistry(appID string, entry Entry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	reg, _ := h.readRegistry()
	// 端点上的令牌要单独存一份：Endpoint 的 JSON 里没有 token 字段。
	if entry.Token == "" {
		entry.Token = entry.Endpoint.Token
	}
	// 接管别人的实例时不知道 PID，别把已知的进程号抹成 0。
	if entry.PID == 0 {
		if old, ok := reg.Apps[appID]; ok {
			entry.PID = old.PID
		}
	}
	reg.Apps[appID] = entry
	raw, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return
	}
	tmp := h.registryPath() + ".tmp"
	// 0600：这份文件现在带着控制令牌。
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, h.registryPath())
}
