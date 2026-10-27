// Package craft 描述 Craft 应用套件：每个应用的安装位置、启动参数与控制通道形态。
//
// 这一层只有数据，不含行为 —— 进程怎么起、通道怎么连，分别由 internal/host 和
// internal/control 负责。新增一个 craft 应用只需要在这里加一条。
package craft

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Control 决定 --control 后面跟的是什么。
//
// 实测（2026-10，四个已装应用逐个拉起、逐个读命令表）：
//
//	DeckCraft 0.11.1   --control 7990          应用自己监听端口
//	FigCraft  0.11.2   --control 7991          同上
//	CADCraft  0.13.0   --control 7994          同上，但多一个令牌文件 ~/.cache/cadcraft-control-7994.token
//	PdfCraft  0.14.0   --control <文件路径>     应用把 {pid,port,token} 写进那个文件，端口自己挑
//
// 所以「控制通道形态」是每个应用的属性，不是全局约定；同一个 --control
// 在三代实现里收的东西并不一样。
type Control string

const (
	// ControlPort：--control PORT，端口由宿主挑（或应用默认端口），应用监听 127.0.0.1:PORT。
	ControlPort Control = "port"
	// ControlFile：--control PATH，应用把 {pid,port,token} 写进 PATH，客户端据此连它挑的端口。
	ControlFile Control = "file"
)

// App 是套件里的一个应用。
type App struct {
	ID        string `json:"id"`        // 机器名：仓库名、二进制名、bundle id 尾段
	Name      string `json:"name"`      // 显示名
	Tagline   string `json:"tagline"`   // 一句话说明（中文）
	TaglineEN string `json:"taglineEn"` // 同上，英文；界面按语言选一个
	Repo      string `json:"repo"`      // GitHub owner/repo
	Accent    string `json:"accent"`    // 品牌色，界面卡片用
	Fallback  string `json:"fallback"`  // 品牌色在浅色背景上的文字色

	Control     Control `json:"control,omitempty"`     // 控制通道形态；空表示尚未实测
	Port        int     `json:"port,omitempty"`        // ControlPort 时的默认端口；0 表示由宿主分配
	ControlFlag string  `json:"controlFlag"`           // 启动控制通道的参数名
}

// DeckCraftID 是套件里唯一实测过「幻灯片 + 文本框 + 页」模型的成员。
// 以 deckcraft_ 开头的 MCP 工具只对它成立，这层限定由 catalog 定义，
// 免得散落在各个 handler 里写同一个字符串。
const DeckCraftID = "deckcraft"

// BundleID 是 macOS bundle id，也是探测安装位置时最可靠的依据。
func (a App) BundleID() string { return "ai.storyteller." + a.ID }

// ControlArgs 生成启动控制通道所需的参数。
// endpoint 的含义随 Control 变化：port 是端口号，file 是会合文件路径。
func (a App) ControlArgs(endpoint string) []string {
	return []string{a.ControlFlag, endpoint}
}

// TokenPaths 返回端口模式下应用可能写控制令牌的位置。
//
// 这是应用自己的约定（CADCraft 0.13：<cache>/cadcraft-control-7994.token），不是我们定的。
// 注意它写在 ~/.cache —— 字面量，不是 macOS 的 ~/Library/Caches。两个都试：
// 多探一条路径，比在代码里赌「它一定用哪个」安全。
//
// 没有令牌的应用这些路径都不存在，读不到就当没有 —— 不额外维护一张「谁要令牌」的表，
// 因为是否需要令牌会随版本变，而文件出现与否是当下的真相。
func (a App) TokenPaths(port int) []string {
	name := fmt.Sprintf("%s-control-%d.token", a.ID, port)
	var out []string
	if h := home(); h != "" {
		out = append(out, filepath.Join(h, ".cache", name))
	}
	if cache, err := os.UserCacheDir(); err == nil {
		out = append(out, filepath.Join(cache, name))
	}
	return out
}

// TokenPath 是首选位置，给日志和错误提示用。
func (a App) TokenPath(port int) string {
	if paths := a.TokenPaths(port); len(paths) > 0 {
		return paths[0]
	}
	return ""
}

// Executables 按平台给出可执行文件的候选路径，已存在的排在前面。
func (a App) Executables() []string {
	var candidates []string
	switch runtime.GOOS {
	case "darwin":
		for _, dir := range []string{"/Applications", filepath.Join(home(), "Applications")} {
			candidates = append(candidates,
				filepath.Join(dir, a.Name+".app", "Contents", "MacOS", a.Name))
		}
	case "windows":
		local := os.Getenv("LOCALAPPDATA")
		programFiles := os.Getenv("ProgramFiles")
		for _, base := range []string{local, programFiles} {
			if base == "" {
				continue
			}
			candidates = append(candidates,
				filepath.Join(base, "Programs", a.Name, a.ID+".exe"),
				filepath.Join(base, a.Name, a.ID+".exe"),
			)
		}
	default:
		candidates = append(candidates,
			filepath.Join("/usr/bin", a.ID),
			filepath.Join("/opt", a.Name, a.ID),
			filepath.Join("/opt", a.ID, a.ID),
			filepath.Join(home(), ".local", "bin", a.ID),
		)
	}
	return candidates
}

// InstalledPath 返回第一个真实存在的可执行文件，没有则返回空串。
func (a App) InstalledPath() string {
	for _, p := range a.Executables() {
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}

// Installed 表示本机是否装了该应用。
func (a App) Installed() bool { return a.InstalledPath() != "" }

func home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}

// Suite 是套件全量清单，顺序沿用 getartcraft.com 的展示顺序。
//
// Control / Port 只填已经实测过的应用；空值表示「尚未确认」，宿主不会假装能连，
// 只尝试注册表里记录的端点。已经确认的四个见 Control 的注释。
//
// 端口错开是为了让每个应用重启后回到自己的位置，而不是从 7990 往上抢 ——
// 宿主仍然会在被占用时顺延。
var Suite = []App{
	{ID: "photocraft", Name: "PhotoCraft", Tagline: "图像编辑", Repo: "storytold/photocraft", Accent: "#2f7bf5", Fallback: "#1a5bc4", TaglineEN: "Image editing", ControlFlag: "--control"},
	{ID: "vectorcraft", Name: "VectorCraft", Tagline: "矢量插画", Repo: "storytold/vectorcraft", Accent: "#e8573f", Fallback: "#b83a24", TaglineEN: "Vector illustration", ControlFlag: "--control"},
	{ID: "filmcraft", Name: "FilmCraft", Tagline: "视频剪辑与调色", Repo: "storytold/filmcraft", Accent: "#8b5cf6", Fallback: "#6a3fd6", TaglineEN: "Video editing and grading", ControlFlag: "--control"},
	{ID: "lightcraft", Name: "LightCraft", Tagline: "照片库与 RAW 冲洗", Repo: "storytold/lightcraft", Accent: "#f2a516", Fallback: "#8a5800", TaglineEN: "Photo library and RAW", ControlFlag: "--control"},
	// PdfCraft 0.14：--control 收的是**会合文件路径**，不是端口也不是 socket。
	// 文件内容是 {"pid":…,"port":…,"token":…}，端口由应用自己挑；握手是 JSON-RPC 2.0，
	// 先 auth {token}，方法名是 ui.commands / ui.command（没有 engine.*）。
	{ID: "pdfcraft", Name: "PdfCraft", Tagline: "PDF 阅读、整理与保护", Repo: "storytold/pdfcraft", Accent: "#12a58a", Fallback: "#0a7563", Control: ControlFile, TaglineEN: "PDF reading, organizing, protecting", ControlFlag: "--control"},
	{ID: "effectcraft", Name: "EffectCraft", Tagline: "动态图形与特效", Repo: "storytold/effectcraft", Accent: "#e0368f", Fallback: "#b0206c", TaglineEN: "Motion graphics and effects", ControlFlag: "--control"},
	{ID: "designcraft", Name: "DesignCraft", Tagline: "版式设计与出版", Repo: "storytold/designcraft", Accent: "#7bb51c", Fallback: "#457008", TaglineEN: "Layout and publishing", ControlFlag: "--control"},
	// FigCraft：实测 0.11.2。--control PORT，597 条命令，但**没有 file.info**
	// —— 命令集是设计工具自己的（node.create / frame.create / shape.*），
	// 依赖文档级模型的 craft_doc 在这里不成立。
	{ID: "figcraft", Name: "FigCraft", Tagline: "界面设计与原型", Repo: "storytold/figcraft", Accent: "#02da96", Fallback: "#047a56", Control: ControlPort, Port: 7991, TaglineEN: "UI design and prototyping", ControlFlag: "--control"},
	{ID: "wordcraft", Name: "WordCraft", Tagline: "写作与文档排版", Repo: "storytold/wordcraft", Accent: "#3f5fe0", Fallback: "#2a44b8", Control: ControlPort, Port: 7992, TaglineEN: "Writing and document layout", ControlFlag: "--control"},
	{ID: "soundcraft", Name: "SoundCraft", Tagline: "录音、剪辑与混音", Repo: "storytold/soundcraft", Accent: "#1fa5c9", Fallback: "#0e7290", Control: ControlPort, Port: 7993, TaglineEN: "Recording, editing, mixing", ControlFlag: "--control"},
	// DeckCraft：完整实测过的应用。--control PORT，JSON lines，
	// 方法见 docs/control-protocol.md（engine.execute / engine.commands / document.inspect…）。
	{ID: "deckcraft", Name: "DeckCraft", Tagline: "演示文稿与放映", Repo: "storytold/deckcraft", Accent: "#f07a1e", Fallback: "#a84f0a", Control: ControlPort, Port: 7990, TaglineEN: "Presentations and slideshows", ControlFlag: "--control"},
	// CADCraft 0.13：--control PORT，但每条请求要带令牌；令牌由应用写在
	// ~/.cache/cadcraft-control-<port>.token。方法名仍是 engine.*。
	{ID: "cadcraft", Name: "CADCraft", Tagline: "计算机辅助设计", Repo: "storytold/cadcraft", Accent: "#3ea6e8", Fallback: "#1a6fa8", Control: ControlPort, Port: 7994, TaglineEN: "Computer-aided design", ControlFlag: "--control"},
	{ID: "gridcraft", Name: "GridCraft", Tagline: "表格与数据", Repo: "storytold/gridcraft", Accent: "#1f9d55", Fallback: "#12703a", Control: ControlPort, Port: 7995, TaglineEN: "Spreadsheets and data", ControlFlag: "--control"},
}

// ByID 按 id 取应用。
func ByID(id string) (App, bool) {
	for _, a := range Suite {
		if a.ID == id {
			return a, true
		}
	}
	return App{}, false
}

// Select 按 id 列表筛选应用；空列表表示全选。
func Select(ids []string) []App {
	if len(ids) == 0 {
		return Suite
	}
	seen := make(map[string]bool, len(ids))
	var out []App
	for _, raw := range ids {
		id := strings.ToLower(strings.TrimSpace(raw))
		if id == "" || seen[id] {
			continue
		}
		if a, ok := ByID(id); ok {
			seen[id] = true
			out = append(out, a)
		}
	}
	return out
}

// KnownControl 表示该应用的控制通道形态是否已经过实测确认。
func (a App) KnownControl() bool { return a.Control != "" }

// DescribeControl 是给界面和日志看的一句话描述。文案不带「是否要令牌」：
// 那是运行期才知道的事，写在数据里迟早会过期。
func (a App) DescribeControl() string {
	switch a.Control {
	case ControlPort:
		if a.Port > 0 {
			return fmt.Sprintf("TCP 127.0.0.1:%d", a.Port)
		}
		return "TCP（端口由宿主分配）"
	case ControlFile:
		return "会合文件（应用分配端口）"
	default:
		return "未确认"
	}
}
