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

// Transport 是控制通道的形态。
//
// 实测（DeckCraft 0.11.1 / PdfCraft）：两个应用的 ControlFlag 都是 --control，
// 但参数含义不同 —— DeckCraft 要一个 TCP 端口号，PdfCraft 要一个 socket 文件路径。
// 所以通道形态必须按应用声明，不能用统一约定。
type Transport string

const (
	TCP  Transport = "tcp"
	Unix Transport = "unix"
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

	Transport   Transport `json:"transport,omitempty"`   // 控制通道形态
	DefaultPort int       `json:"defaultPort,omitempty"` // Transport 为 tcp 时的默认端口；0 表示由宿主分配
	ControlFlag string    `json:"controlFlag"`           // 启动控制通道的参数名
}

// DeckCraftID 是套件里唯一实测过「幻灯片 + 文本框 + 页」模型的成员。
// 以 deckcraft_ 开头的 MCP 工具只对它成立，这层限定由 catalog 定义，
// 免得散落在各个 handler 里写同一个字符串。
const DeckCraftID = "deckcraft"

// BundleID 是 macOS bundle id，也是探测安装位置时最可靠的依据。
func (a App) BundleID() string { return "ai.storyteller." + a.ID }

// ControlArgs 生成启动控制通道所需的参数。
// endpoint 的含义随 Transport 变化：tcp 是端口号，unix 是 socket 文件路径。
func (a App) ControlArgs(endpoint string) []string {
	return []string{a.ControlFlag, endpoint}
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
// Transport / DefaultPort 只填已经实测过的应用；其余留空表示「尚未确认」，
// 宿主会在启动后尝试按 --control 探测，探测失败就如实报告不可连，而不是假装支持。
//
// 已经确认的一族：它们的 --control 收的是**端口号**，不是 socket 路径
// （`Usage: figcraft [--control PORT] …`）。给 unix 路径不是「换了种通道」，
// 是给了一个它不认识的参数，进程照常起来、只是不监听任何地方，于是宿主只能
// 报「控制通道未就绪」。FigCraft 实测：597 条命令，engine.commands /
// engine.execute 与 DeckCraft 同协议。其余几个同厂同引擎，按此推定，端口错开
// 是为了让每个应用重启后回到自己的位置，而不是从 7990 往上抢。
var Suite = []App{
	{ID: "photocraft", Name: "PhotoCraft", Tagline: "图像编辑", Repo: "storytold/photocraft", Accent: "#2f7bf5", Fallback: "#1a5bc4", TaglineEN: "Image editing", ControlFlag: "--control"},
	{ID: "vectorcraft", Name: "VectorCraft", Tagline: "矢量插画", Repo: "storytold/vectorcraft", Accent: "#e8573f", Fallback: "#b83a24", TaglineEN: "Vector illustration", ControlFlag: "--control"},
	{ID: "filmcraft", Name: "FilmCraft", Tagline: "视频剪辑与调色", Repo: "storytold/filmcraft", Accent: "#8b5cf6", Fallback: "#6a3fd6", TaglineEN: "Video editing and grading", ControlFlag: "--control"},
	{ID: "lightcraft", Name: "LightCraft", Tagline: "照片库与 RAW 冲洗", Repo: "storytold/lightcraft", Accent: "#f2a516", Fallback: "#8a5800", TaglineEN: "Photo library and RAW", ControlFlag: "--control"},
	{ID: "pdfcraft", Name: "PdfCraft", Tagline: "PDF 阅读、整理与保护", Repo: "storytold/pdfcraft", Accent: "#12a58a", Fallback: "#0a7563", Transport: Unix, TaglineEN: "PDF reading, organizing, protecting", ControlFlag: "--control"},
	{ID: "effectcraft", Name: "EffectCraft", Tagline: "动态图形与特效", Repo: "storytold/effectcraft", Accent: "#e0368f", Fallback: "#b0206c", TaglineEN: "Motion graphics and effects", ControlFlag: "--control"},
	{ID: "designcraft", Name: "DesignCraft", Tagline: "版式设计与出版", Repo: "storytold/designcraft", Accent: "#7bb51c", Fallback: "#457008", TaglineEN: "Layout and publishing", ControlFlag: "--control"},
	// FigCraft：实测过。--control PORT，JSON lines，597 条命令，但**没有 file.info**
	// —— 命令集是设计工具自己的（node.create / frame.create / shape.*），
	// 依赖文档级模型的 craft_doc 在这里不成立。
	{ID: "figcraft", Name: "FigCraft", Tagline: "界面设计与原型", Repo: "storytold/figcraft", Accent: "#02da96", Fallback: "#047a56", Transport: TCP, DefaultPort: 7991, TaglineEN: "UI design and prototyping", ControlFlag: "--control"},
	{ID: "wordcraft", Name: "WordCraft", Tagline: "写作与文档排版", Repo: "storytold/wordcraft", Accent: "#3f5fe0", Fallback: "#2a44b8", Transport: TCP, DefaultPort: 7992, TaglineEN: "Writing and document layout", ControlFlag: "--control"},
	{ID: "soundcraft", Name: "SoundCraft", Tagline: "录音、剪辑与混音", Repo: "storytold/soundcraft", Accent: "#1fa5c9", Fallback: "#0e7290", Transport: TCP, DefaultPort: 7993, TaglineEN: "Recording, editing, mixing", ControlFlag: "--control"},
	// DeckCraft：完整实测过的应用。--control PORT，JSON lines，
	// 方法见 docs/control-protocol.md（engine.execute / engine.commands / document.inspect…）。
	{ID: "deckcraft", Name: "DeckCraft", Tagline: "演示文稿与放映", Repo: "storytold/deckcraft", Accent: "#f07a1e", Fallback: "#a84f0a", Transport: TCP, DefaultPort: 7990, TaglineEN: "Presentations and slideshows", ControlFlag: "--control"},
	{ID: "cadcraft", Name: "CADCraft", Tagline: "计算机辅助设计", Repo: "storytold/cadcraft", Accent: "#3ea6e8", Fallback: "#1a6fa8", Transport: TCP, DefaultPort: 7994, TaglineEN: "Computer-aided design", ControlFlag: "--control"},
	{ID: "gridcraft", Name: "GridCraft", Tagline: "表格与数据", Repo: "storytold/gridcraft", Accent: "#1f9d55", Fallback: "#12703a", Transport: TCP, DefaultPort: 7995, TaglineEN: "Spreadsheets and data", ControlFlag: "--control"},
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
func (a App) KnownControl() bool { return a.Transport != "" }

// DescribeControl 是给界面和日志看的一句话描述。
func (a App) DescribeControl() string {
	switch a.Transport {
	case TCP:
		if a.DefaultPort > 0 {
			return fmt.Sprintf("TCP 127.0.0.1:%d", a.DefaultPort)
		}
		return "TCP（端口由宿主分配）"
	case Unix:
		return "Unix socket"
	default:
		return "未确认"
	}
}
