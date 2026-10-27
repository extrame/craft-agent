// Craft Agent：Craft 套件的 AI 协同工作台。
//
// 同一个二进制有两种活法：
//
//	craft-agent               打开桌面窗口（启动应用、看活动、手动操作）
//	craft-agent mcp           作为 MCP 服务器挂在 stdin/stdout 上，交给 Cursor / Claude
//
// 分流必须在最前面做完：MCP 协议禁止 stdout 出现任何非 JSON-RPC 内容，
// 所以 GUI 的初始化（它会打印、会写日志、会起 webview）一行都不能在 mcp 模式里执行。
package main

import (
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"craft-agent/internal/craft"
	"craft-agent/internal/host"
	"craft-agent/internal/journal"
	mcpserver "craft-agent/internal/mcp"
	"craft-agent/internal/release"
	"craft-agent/internal/update"
	"craft-agent/services"
)

// 前端构建产物随二进制分发。
//
//go:embed all:frontend/dist
var assets embed.FS

const version = "0.1.0"

// Wails 事件：活动流、状态快照、跨进程流水、安装进度。
const (
	eventActivity = "craft:activity"
	eventStatus   = "craft:status"
	eventJournal  = "craft:journal"
	eventUpdate   = "craft:update"
)

func init() {
	application.RegisterEvent[host.Event](eventActivity)
	application.RegisterEvent[[]host.Status](eventStatus)
	application.RegisterEvent[[]journal.Entry](eventJournal)
	application.RegisterEvent[services.UpdateProgress](eventUpdate)
	services.Version = version
}

// newHost 装配宿主。Source / Session 决定流水里的归属：
// GUI 写 "gui"，每个 MCP 进程写自己的 "mcp:<pid>"，界面靠它区分人操作和 agent 操作。
func newHost(dataDir, source, session string, onEvent func(host.Event)) (*host.Host, *journal.Journal, error) {
	dir := dataDir
	if dir == "" {
		d, err := host.DefaultDataDir()
		if err != nil {
			return nil, nil, err
		}
		dir = d
	}
	j, err := journal.Open(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("打开会话流水失败: %w", err)
	}
	h, err := host.New(host.Options{
		DataDir: dir,
		Journal: j,
		Source:  source,
		Session: session,
		OnEvent: onEvent,
	})
	if err != nil {
		return nil, nil, err
	}
	return h, j, nil
}

func main() {
	// 日志统一走 stderr：stdout 是 MCP 的专用通道。
	log.SetOutput(os.Stderr)
	log.SetFlags(log.Ltime)

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "mcp":
			os.Exit(runMCP(os.Args[2:]))
		case "apps":
			os.Exit(runApps(os.Args[2:]))
		case "run":
			os.Exit(runOnce(os.Args[2:]))
		case "versions":
			os.Exit(runVersions(os.Args[2:]))
		case "install":
			os.Exit(runInstall(os.Args[2:]))
		case "update":
			os.Exit(runUpdate(os.Args[2:]))
		case "version", "--version", "-V":
			fmt.Println("craft-agent", version)
			return
		case "help", "--help", "-h":
			usage()
			return
		}
	}
	runGUI()
}

func usage() {
	fmt.Fprintf(os.Stderr, `craft-agent %s — Craft 套件的 AI 协同工作台

用法:
  craft-agent [窗口]                             打开桌面窗口
  craft-agent mcp [--apps deckcraft,wordcraft] [--tools minimal|core|full]
                                                 以 MCP stdio 服务器运行（给 Cursor / Claude）
  craft-agent apps                               列出套件应用与本机状态
  craft-agent versions [--refresh]               列出本机版本与远端最新版本
  craft-agent install <app> [--dry-run]          安装最新发布（含首次安装）
  craft-agent update <app> [--dry-run]            升级到最新发布（--dry-run 只查安装包）
  craft-agent run <app> <command> [json]          一次性执行命令，结果打印到 stdout
  craft-agent version                            显示版本

示例:
  craft-agent run deckcraft slide.inspect
  craft-agent run deckcraft slide.new '{"layout":"titleOnly","title":"你好"}'

mcp.json:
  {"mcpServers":{"craft-agent":{"command":"%s","args":["mcp"]}}}
`, version, executable())
}

// runVersions 打印本机版本与远端最新版本，作为界面之外的一条自查路径。
func runVersions(args []string) int {
	fs := flag.NewFlagSet("versions", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	refresh := fs.Bool("refresh", false, "忽略 6 小时缓存，重新查询")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	dir, err := host.DefaultDataDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "找不到数据目录: %v\n", err)
		return 1
	}
	c := release.NewChecker(dir, version)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	snap, err := c.Check(ctx, craft.Suite, *refresh)
	if err != nil {
		fmt.Fprintf(os.Stderr, "查询最新版本失败: %v\n", err)
		return 1
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\t应用\t本机\t最新\t状态\t本地来源")
	for _, app := range craft.Suite {
		inst := c.Installed(app)
		latest := snap.Apps[app.ID]

		state := "—"
		switch {
		case !app.Installed():
			state = "未安装"
		case inst.Version == "":
			state = "版本未知"
		case latest.Version == "":
			state = "最新未知"
		default:
			iv, okI := release.Parse(inst.Version)
			lv, okL := release.Parse(latest.Version)
			switch {
			case okI && okL && release.IsUpdate(lv, iv):
				state = "可更新"
			case okI && okL:
				state = "已最新"
			}
		}
		local := inst.Version
		if local == "" {
			local = "—"
		}
		remote := latest.Version
		if remote == "" {
			remote = "—"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", app.ID, app.Name, local, remote, state, inst.Source)
	}
	_ = w.Flush()
	fmt.Printf("\n数据来源: %s · 检查于 %s\n", snap.Source, snap.CheckedAt.Format("2006-01-02 15:04:05"))
	if len(snap.Errors) > 0 {
		fmt.Printf("部分失败:\n")
		for _, e := range snap.Errors {
			fmt.Printf("  - %s\n", e)
		}
	}
	return 0
}

func executable() string {
	exe, err := os.Executable()
	if err != nil {
		return "craft-agent"
	}
	return exe
}

// runUpdate 在命令行里完成一次升级，顺便给界面那条路径留一条可复现的对照。
//
// 进度写 stderr、结果写 stdout：和 mcp 模式同样的理由 ——
// stdout 只留机器可读的东西。
// runUpdate 与 runInstall 走同一条路：磁盘上要发生的事完全一样 ——
// 下载、校验、挂载、验签、放到目标位置。差别只有两点：install 允许第一次装，
// 而 update 要求已经装了（否则「更新」这个词没有指代对象）。
func runUpdate(args []string) int { return runInstallCmd(args, true) }

func runInstall(args []string) int { return runInstallCmd(args, false) }

func runInstallCmd(args []string, requireInstalled bool) int {
	verb, name := "安装", "install"
	if requireInstalled {
		verb, name = "更新", "update"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dry := fs.Bool("dry-run", false, "只挑出安装包并显示校验和，不下载")
	dest := fs.String("dest", "", "装到指定目录（默认跟随现有副本的位置）")
	dataDir := fs.String("data-dir", "", "数据目录")
	// 位置参数放在前面是人的习惯（update deckcraft --dry-run），但标准库的 flag
	// 遇到第一个非选项就停止解析。先把两者分开，再各自处理。
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		// 带值的选项要把它的值一并带走，否则 "/tmp/x" 会被当成应用名。
		if !strings.Contains(a, "=") && (a == "--dest" || a == "-dest" || a == "--data-dir" || a == "-data-dir") && i+1 < len(args) {
			flags = append(flags, args[i+1])
			i++
		}
	}
	if err := fs.Parse(flags); err != nil {
		return 2
	}
	if len(positional) < 1 {
		fmt.Fprintf(os.Stderr, "用法: craft-agent %s <app> [--dry-run] [--dest 目录]\n", name)
		return 2
	}
	id := positional[0]
	app, ok := craft.ByID(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "未知应用: %s\n", id)
		return 2
	}
	if requireInstalled && !app.Installed() {
		fmt.Fprintf(os.Stderr, "%s 没有安装；要装请用: craft-agent install %s\n", app.Name, app.ID)
		return 1
	}

	dir := *dataDir
	if dir == "" {
		d, err := host.DefaultDataDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "找不到数据目录: %v\n", err)
			return 1
		}
		dir = d
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	checker := release.NewChecker(dir, version)
	snap, err := checker.Check(ctx, craft.Suite, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "查询最新版本失败: %v\n", err)
		return 1
	}
	latest := snap.Apps[app.ID]
	if latest.Tag == "" {
		fmt.Fprintf(os.Stderr, "拿不到 %s 的最新发布标签\n", app.Name)
		return 1
	}

	client := update.NewClient("Craft-Agent/" + version)
	repo := checker.RepoFor(app)
	rel, err := client.Release(ctx, repo, latest.Tag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	asset, err := update.Pick(app, rel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	if asset, err = client.EnsureSum(ctx, repo, rel, asset); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	// 首次安装时本机版本是空的：空串在表里显示成「—」，比编一个 0.0.0 诚实。
	current := checker.Installed(app).Version
	shown := current
	if shown == "" {
		shown = "—"
	}
	fmt.Fprintf(os.Stderr, "%s %s → %s\n  安装包 %s（%.1f MB）\n  校验和 %s…\n",
		app.Name, shown, rel.Version, asset.Name, float64(asset.Size)/(1<<20), asset.SHA256[:12])
	if *dry {
		fmt.Printf(`{"app":%q,"from":%q,"to":%q,"asset":%q,"size":%d,"sha256":%q}`+"\n",
			app.ID, current, rel.Version, asset.Name, asset.Size, asset.SHA256)
		return 0
	}

	// 应用在跑就先请它退出；这里不用 kill，用户未保存的工作得留住。
	if err := update.QuitApp(app); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	lastLine := ""
	out, err := update.Install(ctx, client, update.Options{
		App:     app,
		Asset:   asset,
		WorkDir: update.WorkDir(dir),
		Dest:    *dest,
		Version: rel.Version,
		OnStage: func(st update.Stage) {
			line := fmt.Sprintf("\r%s %d%%", st.Step, st.Percent)
			if st.Percent < 0 {
				line = "\r" + st.Step + "…"
			}
			if line != lastLine {
				fmt.Fprint(os.Stderr, line)
				lastLine = line
			}
		},
	})
	fmt.Fprintln(os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s失败: %v\n", verb, err)
		return 1
	}
	raw, _ := json.Marshal(out)
	fmt.Println(string(raw))
	if out.Backup != "" {
		fmt.Fprintf(os.Stderr, "旧版本留在 %s\n", out.Backup)
	}
	return 0
}

// runMCP 是静默模式：只读 stdin、只写 stdout，日志全部去 stderr。
func runMCP(args []string) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	appsArg := fs.String("apps", "", "限定本会话接管的 app，逗号分隔；留空表示全部")
	mode := fs.String("tools", string(mcpserver.ModeMinimal), "工具暴露粒度: minimal|core|full")
	dataDir := fs.String("data-dir", "", "数据目录（默认走系统目录）")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	session := fmt.Sprintf("mcp:%d", os.Getpid())
	h, j, err := newHost(*dataDir, "mcp", session, nil)
	if err != nil {
		log.Printf("[craft-agent] 初始化失败: %v", err)
		return 1
	}
	defer h.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf("[craft-agent] MCP 模式启动，工具粒度 %s，数据目录 %s", *mode, h.DataDir())
	if err := mcpserver.Serve(ctx, mcpserver.Options{
		Host:    h,
		Apps:    craft.Select(splitList(*appsArg)),
		Version: version,
		Mode:    mcpserver.ToolMode(*mode),
		Journal: j,
		Session: session,
	}); err != nil {
		log.Printf("[craft-agent] MCP 服务器退出: %v", err)
		return 1
	}
	return 0
}

// runApps 打印套件清单与本机状态，排查「为什么连不上」时先看这个。
func runApps(args []string) int {
	fs := flag.NewFlagSet("apps", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dataDir := fs.String("data-dir", "", "数据目录")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	h, _, err := newHost(*dataDir, "cli", "cli", nil)
	if err != nil {
		log.Printf("初始化失败: %v", err)
		return 1
	}
	defer h.Close()

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\t应用\t状态\t控制通道\t端点\t路径")
	for _, st := range h.Status() {
		state := "未安装"
		if st.Installed {
			state = "已安装"
			if st.Connected {
				state = "已连接"
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			st.App.ID, st.App.Name, state, st.Control, st.Endpoint.String(), st.Path)
	}
	_ = w.Flush()
	fmt.Printf("\n数据目录: %s\n", h.DataDir())
	return 0
}

// runOnce 是一次性执行，方便脚本与人工验证；不涉及 MCP，可以直接写 stdout。
func runOnce(args []string) int {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "用法: craft-agent run <app> <command> [json]")
		return 2
	}
	appID, command := args[0], args[1]
	paramsJSON := "{}"
	if len(args) > 2 {
		paramsJSON = args[2]
	}

	h, _, err := newHost("", "cli", "cli", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "初始化失败: %v\n", err)
		return 1
	}
	defer h.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client, err := h.Ensure(ctx, appID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	var params any
	if strings.TrimSpace(paramsJSON) != "" {
		if err := json.Unmarshal([]byte(paramsJSON), &params); err != nil {
			fmt.Fprintf(os.Stderr, "参数不是合法 JSON: %v\n", err)
			return 2
		}
	}
	out, err := client.Execute(ctx, command, params)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	if len(out) == 0 {
		fmt.Println(`{"ok":true}`)
	} else {
		fmt.Println(strings.TrimSpace(string(out)))
	}
	return 0
}

func runGUI() {
	h, j, err := newHost("", "gui", "gui", func(e host.Event) {
		if app := application.Get(); app != nil {
			app.Event.Emit(eventActivity, e)
		}
	})
	if err != nil {
		log.Fatalf("初始化失败: %v", err)
	}
	defer h.Close()

	svc := services.NewAppService(h, j)
	svc.Emit = func(p services.UpdateProgress) {
		if app := application.Get(); app != nil {
			app.Event.Emit(eventUpdate, p)
		}
	}

	// 启动后在后台查一次最新版本：不阻塞窗口，失败也不影响使用（界面会显示原因）。
	go func() {
		if _, err := svc.CheckUpdates(false); err != nil {
			log.Printf("检查更新失败: %v", err)
		}
	}()

	app := application.New(application.Options{
		Name:        "Craft Agent",
		Description: "Craft 套件的 AI 协同工作台",
		Services: []application.Service{
			application.NewService(svc),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
	})

	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:     "Craft Agent",
		Width:     1200,
		Height:    800,
		MinWidth:  960,
		MinHeight: 640,
		Mac: application.MacWindow{
			// 隐藏系统标题栏，内容铺满整窗；顶部留一条隐形标题栏给交通灯与拖拽。
			TitleBar:                application.MacTitleBarHidden,
			Backdrop:                application.MacBackdropNormal,
			InvisibleTitleBarHeight: 36,
		},
		// 与前端 --bg 一致，避免窗口出现前闪黑。
		BackgroundColour: application.NewRGB(247, 246, 243),
		URL:              "/",
	})

	// 状态与流水每 2 秒推一次：应用是别的进程，界面不能假设自己知道什么时候变了。
	// 这是「保持更新」的兜底 —— 事件负责即时反馈，轮询负责把漏掉的补齐。
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			a := application.Get()
			if a == nil {
				continue
			}
			a.Event.Emit(eventStatus, svc.Suite())
			a.Event.Emit(eventJournal, svc.Journal(120))
		}
	}()

	if err := app.Run(); err != nil {
		log.Fatalf("窗口退出: %v", err)
	}
}

func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
