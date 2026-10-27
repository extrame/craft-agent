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
	"io"
	"io/fs"
	"log"
	"net/http"
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
	"craft-agent/internal/menu"
	"craft-agent/internal/release"
	"craft-agent/internal/tray"
	"craft-agent/internal/update"
	"craft-agent/services"
)

// 前端构建产物随二进制分发。
//
// 目录里只要有一个文件就能编译：fresh clone 里只有 .keep，界面由 pageHandler
// 兜底说明。以前这里放的是一张占位的 index.html，代价是 `git checkout` 恢复它
// 之后忘了重新构建，二进制里就悄悄换成占位页 —— 构建成功、界面作废。
//
//go:embed all:frontend/dist
var assets embed.FS

const version = "0.1.0"

// pageHandler 决定窗口里显示什么。
//
// dist 里没有 index.html 时（fresh clone，只 commit 了 .keep），不能把请求直接
// 交给文件服务器：那只会得到一张 404，看不出该跑哪条命令。这里换成一张说明页，
// 内容就是让界面能起来的步骤。
func pageHandler(fsys fs.FS, built http.Handler) http.Handler {
	if _, err := fs.Stat(fsys, "frontend/dist/index.html"); err == nil {
		return built
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, notBuiltPage)
	})
}

// notBuiltPage 只在二进制没带前端时出现，所以它自己不能依赖任何前端资源。
const notBuiltPage = `<!doctype html>
<html lang="zh-CN"><meta charset="utf-8">
<title>Craft Agent</title>
<style>
  :root { color-scheme: light dark; }
  body { margin: 0; min-height: 100vh; display: grid; place-items: center;
         font: 14px/1.7 -apple-system, "PingFang SC", system-ui, sans-serif;
         background: #f7f6f3; color: #26241f; }
  main { max-width: 40rem; padding: 2.5rem; }
  h1 { font-size: 1.15rem; margin: 0 0 .75rem; }
  p { margin: 0 0 1rem; color: #55524b; }
  pre { background: #fff; border: 1px solid #e3e0d8; border-radius: 6px;
        padding: .9rem 1rem; overflow-x: auto; font-size: 13px; }
  code { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
  small { color: #85817a; }
</style>
<main>
  <h1>界面还没构建 / Front end not built</h1>
  <p>这个二进制里没有打包界面，只有占位页。命令行功能不受影响：
     <code>craft-agent apps</code>、<code>craft-agent mcp</code> 照常可用。</p>
  <pre><code>cd frontend &amp;&amp; npm install &amp;&amp; npm run build
cd .. &amp;&amp; go build -o bin/craft-agent .</code></pre>
  <p><small>This binary was built from a checkout without <code>frontend/dist</code>.
     Run the two commands above, then rebuild.</small></p>
</main></html>`

// Wails 事件：活动流、状态快照、跨进程流水、安装进度。
const (
	eventActivity = "craft:activity"
	eventStatus   = "craft:status"
	eventJournal  = "craft:journal"
	eventUpdate   = "craft:update"
	eventMenus    = "craft:menus"
)

func init() {
	application.RegisterEvent[host.Event](eventActivity)
	application.RegisterEvent[[]host.Status](eventStatus)
	application.RegisterEvent[[]journal.Entry](eventJournal)
	application.RegisterEvent[services.UpdateProgress](eventUpdate)
	application.RegisterEvent[[]menu.Summary](eventMenus)
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
		case "menu", "menus":
			os.Exit(runMenu(os.Args[2:]))
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
  craft-agent menu [app] [--refresh] [--json]    查看统一菜单模型（--refresh 重新抓，没开则启动）
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

// runMenu 查看/刷新统一菜单模型。界面之外的一条自查路径：
// agent 与界面用的是同一份模型，这里能直接看到它长什么样。
func runMenu(args []string) int {
	fs := flag.NewFlagSet("menu", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	refresh := fs.Bool("refresh", false, "重新抓取命令表（应用没开着就启动）")
	asJSON := fs.Bool("json", false, "输出完整菜单模型 JSON")
	path := fs.String("path", "", "只看某个菜单节点，例如 \"Format/Text\"")
	query := fs.String("query", "", "按关键字搜索命令")
	limit := fs.Int("limit", 40, "搜索时最多列出多少条")
	dataDir := fs.String("data-dir", "", "数据目录")

	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		if !strings.Contains(a, "=") && takesValue(a) && i+1 < len(args) {
			flags = append(flags, args[i+1])
			i++
		}
	}
	if err := fs.Parse(flags); err != nil {
		return 2
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
	h, _, err := newHost(dir, "cli", "cli", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "初始化失败: %v\n", err)
		return 1
	}
	defer h.Close()

	store := menu.NewStore(h.DataDir())
	_ = store.Load()
	checker := release.NewChecker(h.DataDir(), version)

	// 不给应用名：列全量摘要；--refresh 时顺便把「开着的」应用都刷一遍。
	if len(positional) == 0 {
		if *refresh {
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()
			for _, app := range craft.Suite {
				if !app.Installed() {
					continue
				}
				client, err := h.Ensure(ctx, app.ID)
				if err != nil {
					fmt.Fprintf(os.Stderr, "%s: %v\n", app.ID, err)
					continue
				}
				if _, err := store.Refresh(ctx, client, app, checker.Installed(app).Version); err != nil {
					fmt.Fprintf(os.Stderr, "%s: %v\n", app.ID, err)
				}
			}
		}
		return printMenuSummaries(store, checker)
	}

	id := strings.ToLower(strings.TrimSpace(positional[0]))
	app, ok := craft.ByID(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "未知应用: %s\n", id)
		return 2
	}
	if *refresh {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		client, err := h.Ensure(ctx, id)
		if err != nil {
			fmt.Fprintf(os.Stderr, "抓取 %s 失败: %v\n", app.Name, err)
			return 1
		}
		if _, err := store.Refresh(ctx, client, app, checker.Installed(app).Version); err != nil {
			fmt.Fprintf(os.Stderr, "抓取 %s 失败: %v\n", app.Name, err)
			return 1
		}
	}

	m, ok := store.Get(id)
	if !ok {
		fmt.Fprintf(os.Stderr, "%s 还没有预加载的菜单；加 --refresh 抓一次\n", app.Name)
		return 1
	}
	if *asJSON {
		raw, err := json.MarshalIndent(m, "", " ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "编码失败: %v\n", err)
			return 1
		}
		fmt.Println(string(raw))
		return 0
	}
	printMenu(m, *path, *query, *limit)
	return 0
}

func takesValue(flagArg string) bool {
	for _, name := range []string{"--path", "-path", "--query", "-query", "--limit", "-limit", "--data-dir", "-data-dir"} {
		if flagArg == name {
			return true
		}
	}
	return false
}

func printMenuSummaries(store *menu.Store, checker *release.Checker) int {
	sums := store.Summaries(func(a craft.App) string { return checker.Installed(a).Version })
	if len(sums) == 0 {
		fmt.Println("还没有预加载的菜单。跑一次: craft-agent menu <app> --refresh")
		return 0
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\t应用\t命令\t菜单\t归组\t版本\t抓取时间\t备注")
	for _, s := range sums {
		note := ""
		if s.Stale {
			note = "版本已变，建议 --refresh"
		}
		if s.Error != "" {
			note = s.Error
		}
		ver := s.Version
		if ver == "" {
			ver = "—"
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%s\t%s\t%s\n",
			s.App, s.Name, s.Commands, s.Sections, s.Grouped, ver,
			s.FetchedAt.Local().Format("01-02 15:04"), note)
	}
	_ = w.Flush()
	fmt.Printf("\n菜单文件: %s\n", store.Dir())
	return 0
}

// printMenu 是给人看的树。层级用缩进表示，条数写在节点后面 ——
// 「这个应用有几条命令」和「它长在哪几个菜单下」是两个问题，这里都有。
func printMenu(m menu.Menu, path string, query string, limit int) {
	if query != "" {
		hits := m.Search(query, limit)
		for _, it := range hits {
			fmt.Printf("%-28s %-24s %-10s %s\n", it.ID, clipLabel(it.Label), it.Shortcut, strings.Join(it.Path, " › "))
		}
		fmt.Printf("\n%d 条命中（共 %d 条命令）\n", len(hits), m.Commands)
		return
	}

	header := fmt.Sprintf("%s %s · %d 条命令 · %d 个菜单 · %d 条按前缀归组 · 抓取于 %s",
		m.AppName, versionLabel(m.Version), m.Commands, len(m.Sections), m.Grouped,
		m.FetchedAt.Local().Format("2006-01-02 15:04:05"))
	fmt.Println(header)

	if path != "" {
		node, ok := m.Find(strings.Split(path, "/")...)
		if !ok {
			fmt.Printf("\n找不到菜单节点 %q；顶层有: %s\n", path, sectionNames(m))
			return
		}
		printNode(node, 0)
		return
	}
	if len(m.Sections) == 0 {
		fmt.Println("\n（这份菜单是空的）")
		return
	}
	fmt.Println()
	for _, n := range m.Sections {
		printNode(n, 0)
	}
}

func printNode(n *menu.Node, depth int) {
	indent := strings.Repeat("  ", depth)
	name := n.Name
	if n.Grouped {
		name += "（按命令前缀）"
	}
	fmt.Printf("%s%s  [%d]\n", indent, name, n.Count)
	for _, it := range n.Items {
		fmt.Printf("%s  %-28s %-24s %s\n", indent, it.ID, clipLabel(it.Label), it.Shortcut)
	}
	for _, c := range n.Children {
		printNode(c, depth+1)
	}
}

func sectionNames(m menu.Menu) string {
	names := make([]string, 0, len(m.Sections))
	for _, n := range m.Sections {
		names = append(names, n.Name)
	}
	return strings.Join(names, " / ")
}

func versionLabel(v string) string {
	if v == "" {
		return "（版本未知）"
	}
	return "v" + v
}

// clipLabel 按字符截断，别把长标签挤掉后面的快捷键。
func clipLabel(s string) string {
	r := []rune(s)
	if len(r) <= 22 {
		return s
	}
	return string(r[:21]) + "…"
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

	// 菜单也预加载一次：只接管已经开着的应用，不替用户打开窗口。
	// 缓存已经在 NewAppService 里读回来了，所以界面第一帧就有内容；
	// 这一步只是把「应用在跑但菜单是旧的」补上。
	go func() {
		if n := len(svc.PreloadMenus()); n > 0 {
			log.Printf("菜单已预加载 %d 个应用", n)
		}
	}()

	app := application.New(application.Options{
		Name:        "Craft Agent",
		Description: "Craft 套件的 AI 协同工作台",
		Services: []application.Service{
			application.NewService(svc),
		},
		Assets: application.AssetOptions{
			Handler: pageHandler(assets, application.AssetFileServerFS(assets)),
		},
		Mac: application.MacOptions{
			// 关窗只是收起：Craft 应用和流水都还在，退出去下次还得重新发现一遍。
			ApplicationShouldTerminateAfterLastWindowClosed: false,
		},
	})

	win := app.Window.NewWithOptions(application.WebviewWindowOptions{
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

	// 菜单栏 / 通知区域：窗口收起来之后，这是唯一的入口。
	tray.Install(app, win, "zh", svc.OpenDataDir)

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
			suite := svc.Suite()
			a.Event.Emit(eventStatus, suite)
			a.Event.Emit(eventJournal, svc.Journal(120))
			a.Event.Emit(eventMenus, svc.Menus())

			// 窗口不可见时页面上的数字没人看，托盘 tooltip 是唯一还在报状态的地方。
			online := 0
			for _, s := range suite {
				if s.Connected {
					online++
				}
			}
			tray.SetOnline(online)
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
