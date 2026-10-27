package release

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"craft-agent/internal/craft"
)

// Installed 是本机这一份副本的版本信息。
type Installed struct {
	// Version 读不到时为空 —— 界面显示「版本未知」，而不是猜一个。
	Version string `json:"version,omitempty"`
	// Source 说明版本是从哪读到的：bundle / msi / cli。界面据此决定文案。
	Source string `json:"source,omitempty"`
	// Path 是这份副本的位置（.app、exe 或二进制）。
	Path string `json:"path,omitempty"`
	// Error 只在「装了但读不到版本」时出现，用来解释原因。
	Error string `json:"error,omitempty"`
}

// Detect 读一个应用在本机的已安装版本。没装就返回零值。
//
// 三平台的做法不一样，和 craft-launcher 的判断一致：
//   - macOS：读 .app 的 Info.plist，优先 `<App>Version`（带预发布后缀），
//     再 CFBundleShortVersionString（只有数字），最后 CFBundleVersion；
//   - Windows：读 exe 的 ProductVersion（不运行程序）；
//   - Linux：跑一次 `--version` 并解析输出。
func Detect(app craft.App) Installed {
	exe := app.InstalledPath()
	if exe == "" {
		return Installed{}
	}
	out := Installed{Path: exe}
	switch runtime.GOOS {
	case "darwin":
		detectBundle(app, exe, &out)
	case "windows":
		detectWindows(exe, &out)
	default:
		detectCLI(exe, &out)
	}
	return out
}

// BundleOf 从 Contents/MacOS/<exe> 往上找到 .app；找不到返回空串。
func BundleOf(exe string) string { return bundleOf(exe) }

// bundleOf 从 Contents/MacOS/<exe> 往上找到 .app。
func bundleOf(exe string) string {
	p := exe
	for {
		parent := filepath.Dir(p)
		if parent == p || parent == "/" {
			return ""
		}
		if strings.HasSuffix(parent, ".app") {
			return parent
		}
		p = parent
	}
}

func detectBundle(app craft.App, exe string, out *Installed) {
	bundle := bundleOf(exe)
	if bundle == "" {
		out.Error = "找不到 .app 包"
		return
	}
	readBundlePlist(app, bundle, out)
}

// ReadBundle 读指定 .app 里声明的版本。
//
// 升级时用：刚解出来的新副本要先验明身份（bundle id、版本号）才允许换掉旧副本，
// 那时候它还没被安装，Detect 那套「按候选路径找可执行文件」的逻辑用不上。
func ReadBundle(app craft.App, bundle string) Installed {
	out := Installed{Path: bundle, Source: "bundle"}
	readBundlePlist(app, bundle, &out)
	return out
}

func readBundlePlist(app craft.App, bundle string, out *Installed) {
	out.Source = "bundle"
	plist := filepath.Join(bundle, "Contents", "Info.plist")
	values, err := readPlist(plist)
	if err != nil {
		out.Error = "读取 Info.plist 失败: " + err.Error()
		return
	}
	// bundle id 对不上说明这不是这个应用的副本，宁可说不知道，也不报一个别人的版本。
	if id := values["CFBundleIdentifier"]; id != "" && id != app.BundleID() {
		out.Error = "bundle id 不匹配: " + id
		return
	}
	for _, key := range []string{app.Name + "Version", "CFBundleShortVersionString", "CFBundleVersion"} {
		if raw := strings.TrimSpace(values[key]); raw != "" {
			if v, ok := Parse(raw); ok {
				out.Version = v.String()
				return
			}
		}
	}
	out.Error = "Info.plist 里没有可解析的版本号"
}

func detectWindows(exe string, out *Installed) {
	out.Source = "msi"
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	// 只读文件版本资源，不启动程序。
	script := `(Get-Item -LiteralPath '` + strings.ReplaceAll(exe, `'`, `''`) + `').VersionInfo.ProductVersion`
	cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	raw, err := cmd.Output()
	if err != nil {
		out.Error = "读取文件版本失败: " + err.Error()
		return
	}
	if v, ok := ParseLoose(string(raw)); ok {
		out.Version = v.String()
		return
	}
	out.Error = "文件版本无法解析: " + strings.TrimSpace(string(raw))
}

func detectCLI(exe string, out *Installed) {
	out.Source = "cli"
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "--version")
	raw, err := cmd.CombinedOutput()
	if err != nil && len(raw) == 0 {
		out.Error = "执行 --version 失败: " + err.Error()
		return
	}
	if v, ok := ParseLoose(string(raw)); ok {
		out.Version = v.String()
		return
	}
	out.Error = "--version 输出里没有版本号"
}

// readPlist 读 Info.plist 顶层 dict 里的字符串值。
//
// 大多数 .app 的 Info.plist 是 XML；遇到二进制 plist 就用系统的 plutil 转一次，
// 免得为了一个字段引入 plist 库。
func readPlist(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if bytes.HasPrefix(data, []byte("bplist")) {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		converted, err := exec.CommandContext(ctx, "plutil", "-convert", "xml1", "-o", "-", path).Output()
		if err != nil {
			return nil, errors.New("二进制 plist 无法转换")
		}
		data = converted
	}
	return parsePlistXML(data)
}

func parsePlistXML(data []byte) (map[string]string, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	out := map[string]string{}
	depth := 0
	pending := ""
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth != 3 {
				continue
			}
			switch {
			case t.Name.Local == "key":
				var s string
				if err := dec.DecodeElement(&s, &t); err == nil {
					pending = s
				}
				depth--
			case pending != "":
				if t.Name.Local == "string" {
					var s string
					if err := dec.DecodeElement(&s, &t); err == nil {
						out[pending] = s
					}
				} else {
					// 非字符串值（dict/array/integer/date…）跳过，但不要打乱深度。
					_ = dec.Skip()
				}
				pending = ""
				depth--
			}
		case xml.EndElement:
			depth--
		}
	}
	return out, nil
}
