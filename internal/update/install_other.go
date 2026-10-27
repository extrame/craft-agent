//go:build !darwin

package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"craft-agent/internal/craft"
	"craft-agent/internal/release"
)

// Options 是一次安装的全部输入。
type Options struct {
	App     craft.App
	Asset   Asset
	WorkDir string
	// Dest 覆盖安装目录，只给测试与「装到别处」用。
	Dest string
	// Version 是这次要装的版本。
	Version string
	// OnStage 接收进度；可以为 nil。
	OnStage func(Stage)
}

// Install 在 macOS 之外的平台上只做到「下载并校验」，然后把安装包交给用户。
//
// 不做静默安装是有意的：Windows 的 msi 需要提权、便携版 zip 要问装到哪、
// Linux 的 deb/rpm/AppImage 各有各的装法 —— 每条路径都要单独验证，
// 没验过的「自动安装」比让用户点两下更危险。这里保证的是「下来的东西是对的」。
func Install(ctx context.Context, c *Client, o Options) (Result, error) {
	if o.App.ID == "" || o.Asset.Name == "" {
		return Result{}, errors.New("缺少要安装的应用或安装包")
	}
	if err := os.MkdirAll(o.WorkDir, 0o755); err != nil {
		return Result{}, err
	}
	file := filepath.Join(o.WorkDir, o.Asset.Name)
	stage(o.OnStage, Stage{Step: "download", Percent: 0, Total: o.Asset.Size})
	if err := c.Download(ctx, o.Asset, file, func(done, total int64) {
		pct := -1
		if total > 0 {
			pct = int(done * 100 / total)
		}
		stage(o.OnStage, Stage{Step: "download", Percent: pct, Bytes: done, Total: total})
	}); err != nil {
		return Result{}, err
	}
	stage(o.OnStage, Stage{Step: "manual", Percent: 100, Note: "已下载并校验"})
	return Result{Version: o.Version, Path: file, Manual: true}, nil
}

// QuitApp 让应用退出。这里没有跨平台的优雅退出手段，只能报告它在运行。
func QuitApp(app craft.App) error {
	if release.Detect(app).Path == "" {
		return nil
	}
	if !running(app) {
		return nil
	}
	return fmt.Errorf("%s 还在运行，先退出再更新", app.Name)
}

func running(app craft.App) bool {
	if runtime.GOOS != "windows" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		pattern := app.Name
		out, err := exec.CommandContext(ctx, "pgrep", "-f", pattern).Output()
		if err != nil {
			return false
		}
		for _, field := range strings.Fields(string(out)) {
			if field != fmt.Sprint(os.Getpid()) {
				return true
			}
		}
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	script := fmt.Sprintf(`(Get-Process -Name '%s' -ErrorAction SilentlyContinue).Count`, strings.TrimSuffix(app.ID, ".exe"))
	out, err := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script).Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != "0"
}
