// Package tray 把窗口收进菜单栏（macOS）和通知区域（Windows）。
//
// 这个程序盯的是别的进程的状态：关掉窗口不代表事情结束，Craft 应用还在跑，
// 操作流水还在长。macOS 上 Cmd+W 本来就不等于退出，Windows 上关窗进托盘也是
// 同一个期待，所以这里统一成「关窗 = 收起」，托盘是把它找回来的入口。
//
// 托盘菜单画在系统 chrome 里，页面上的 i18n 够不着，文案只能在这边另存一份。
package tray

import (
	_ "embed"
	"fmt"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

//go:embed icons/tray-template.png
var iconTemplate []byte

//go:embed icons/tray-win.png
var iconColour []byte

// Labels 是托盘菜单的全部文案。只留三句，不跟页面词表合并：
// 合并会让「改个按钮文案」变成「重新编译托盘」。
type Labels struct {
	Show    string
	DataDir string
	Quit    string
	Online  string
}

var words = map[string]Labels{
	"zh": {
		Show:    "显示主窗口",
		DataDir: "打开数据目录",
		Quit:    "退出 Craft Agent",
		Online:  "%d 个应用在线",
	},
	"en": {
		Show:    "Show window",
		DataDir: "Open data folder",
		Quit:    "Quit Craft Agent",
		Online:  "%d apps online",
	},
}

var (
	mu       sync.Mutex
	app      *application.App
	win      *application.WebviewWindow
	item     *application.SystemTray
	opener   func() error
	lang     = "zh"
	online   = -1
	quitting atomic.Bool
)

// Install 建托盘，并把关窗改写成收起。
//
// 必须在 app.Run 之前调用：那时托盘还没有原生对象，SetIcon / SetMenu 只是把
// 参数存下来，由 Run 统一创建。
func Install(a *application.App, w *application.WebviewWindow, language string, openDataDir func() error) {
	mu.Lock()
	app, win, opener = a, w, openDataDir
	if _, ok := words[language]; ok {
		lang = language
	}
	mu.Unlock()

	// 关窗用 Hook 而不是 OnWindowEvent：Wails 自己在这条事件上注册了一个
	// listener 负责销毁窗口，listener 是并发跑的且不看别人有没有取消。
	// hook 先于 listener 执行，这里取消掉，销毁那一步就不会发生。
	w.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		if quitting.Load() {
			return
		}
		e.Cancel()
		mu.Lock()
		target := win
		mu.Unlock()
		if target != nil {
			target.Hide()
		}
	})

	mu.Lock()
	item = app.SystemTray.New()
	if runtime.GOOS == "darwin" {
		// 模板图标：macOS 按系统主题着色，深浅色菜单栏都不用出两套图。
		item.SetTemplateIcon(iconTemplate)
	} else {
		item.SetIcon(iconColour)
	}
	item.SetTooltip("Craft Agent")
	mu.Unlock()

	rebuild()
}

// SetLanguage 让托盘菜单跟上界面语言。前端启动和切换语言时各调一次。
func SetLanguage(language string) {
	if _, ok := words[language]; !ok {
		return
	}
	mu.Lock()
	changed := lang != language
	lang = language
	mu.Unlock()
	if changed {
		rebuild()
	}
}

// SetOnline 把「几个应用在线」报出去。窗口收起后，这是唯一还看得见的进程状态；
// 不变就不更新，免得每 2 秒往主线程推一次无谓的调用。
//
// 两个平台放的地方不一样，不是审美问题：macOS 的托盘没有 tooltip（Wails 那边
// 是空实现），只有图标旁边那点标题位置能放字；Windows 反过来，tooltip 有、
// label 是空实现。
func SetOnline(n int) {
	mu.Lock()
	if n == online {
		mu.Unlock()
		return
	}
	online = n
	target := item
	w := words[lang]
	mu.Unlock()
	if target == nil {
		return
	}

	if runtime.GOOS == "darwin" {
		label := ""
		if n > 0 {
			label = strconv.Itoa(n)
		}
		target.SetLabel(label)
		return
	}

	text := "Craft Agent"
	if n > 0 {
		text = "Craft Agent · " + fmt.Sprintf(w.Online, n)
	}
	target.SetTooltip(text)
}

func rebuild() {
	mu.Lock()
	a, target, w := app, item, words[lang]
	mu.Unlock()
	if a == nil || target == nil {
		return
	}

	menu := a.NewMenu()
	menu.Add(w.Show).OnClick(func(*application.Context) { Show() })
	menu.Add(w.DataDir).OnClick(func(*application.Context) { OpenDataDir() })
	menu.AddSeparator()
	menu.Add(w.Quit).OnClick(func(*application.Context) { Quit() })
	target.SetMenu(menu)
}

// Show 把主窗口叫回来。收起时窗口对象还在，Show 只是让它重新可见。
func Show() {
	mu.Lock()
	target := win
	mu.Unlock()
	if target != nil {
		target.Show().Focus()
	}
}

// OpenDataDir 打开数据目录：mcp.json、流水、日志都在那儿，出问题时用户要能自己看。
func OpenDataDir() {
	mu.Lock()
	fn := opener
	mu.Unlock()
	if fn != nil {
		_ = fn()
	}
}

// Quit 是唯一真正退出的路径。先立旗子，否则刚注册的关窗 hook 会把退出拦下来。
func Quit() {
	quitting.Store(true)
	mu.Lock()
	a := app
	mu.Unlock()
	if a != nil {
		a.Quit()
	}
}
