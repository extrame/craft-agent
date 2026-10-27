// Package release 负责两件事：读本机已安装的版本，以及查远端最新发布。
//
// 这两件事在 craft-launcher 里是核心（它要负责装和升级），在这里只取其中的
// 「看得见、能判断、能指路」三件事：已安装版本的来源、最新发布的版本、以及两者比较的结果。
// 真正的下载安装不在这里做 —— 那需要签名校验、包管理器识别与提权，交给用户和官方安装包更稳妥。
package release

import (
	"strconv"
	"strings"
)

// Version 是一个足够用的语义化版本：主次修订 + 预发布标识。
//
// 不引第三方库的理由：这里只需要解析和比较，规则很小；而 craft 应用的版本来源很杂
// （v0.5.0、0.5、0.2.0~rc.1-1、Debian 带包修订的 0.5.0-1、--version 输出里夹着的版本号），
// 需要的恰恰是「宽容解析」，不是严格的 semver。
type Version struct {
	Major int    `json:"major"`
	Minor int    `json:"minor"`
	Patch int    `json:"patch"`
	Pre   string `json:"pre,omitempty"`
}

func (v Version) String() string {
	s := strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor) + "." + strconv.Itoa(v.Patch)
	if v.Pre != "" {
		s += "-" + v.Pre
	}
	return s
}

// Parse 读 tag 与安装包报出的版本号。
//
// 接受：`v0.5.0`、`0.5.0`、`0.2.0-rc.1`、Debian 的 `0.2.0~rc.1-1`、以及两段式的 `0.5`。
func Parse(text string) (Version, bool) {
	t := strings.TrimSpace(text)
	t = strings.TrimPrefix(strings.TrimPrefix(t, "v"), "V")
	if t == "" || len(t) > 64 {
		return Version{}, false
	}

	// Debian/RPM 用 `~` 表示预发布，并用 `-` 追加包修订号。先归一化成 semver 的写法。
	if strings.Contains(t, "~") {
		if i := strings.IndexByte(t, '-'); i >= 0 {
			t = t[:i]
		}
		t = strings.ReplaceAll(t, "~", "-")
	}

	if v, ok := parseStrict(t); ok {
		return v, true
	}
	// `0.5` → `0.5.0`
	core, rest := t, ""
	if i := strings.IndexAny(t, "-+"); i >= 0 {
		core, rest = t[:i], t[i:]
	}
	parts := strings.Split(core, ".")
	if len(parts) == 2 && allDigits(parts) {
		return parseStrict(core + ".0" + rest)
	}
	return Version{}, false
}

// ParseLoose 从一段文本里捞出第一个 X.Y.Z。用于 `--version` 这种带前缀的输出
// （`DeckCraft 0.11.1 (abc, 2026)`），以及包管理器带后缀的快照版本。
func ParseLoose(text string) (Version, bool) {
	if v, ok := Parse(text); ok {
		return v, true
	}
	s := text
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			continue
		}
		if i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
			continue
		}
		major, n, ok := digitsAt(s, i)
		if !ok || n >= len(s) || s[n] != '.' {
			continue
		}
		minor, m, ok := digitsAt(s, n+1)
		if !ok || m >= len(s) || s[m] != '.' {
			continue
		}
		patch, _, ok := digitsAt(s, m+1)
		if !ok {
			continue
		}
		return Version{Major: major, Minor: minor, Patch: patch}, true
	}
	return Version{}, false
}

func digitsAt(s string, from int) (int, int, bool) {
	end := from
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == from {
		return 0, from, false
	}
	n, err := strconv.Atoi(s[from:end])
	if err != nil {
		return 0, from, false
	}
	return n, end, true
}

func parseStrict(t string) (Version, bool) {
	var v Version
	rest := t

	// major
	n, i, ok := digitsAt(rest, 0)
	if !ok {
		return v, false
	}
	v.Major, rest = n, rest[i:]
	if !strings.HasPrefix(rest, ".") {
		return v, false
	}
	rest = rest[1:]

	n, i, ok = digitsAt(rest, 0)
	if !ok {
		return v, false
	}
	v.Minor, rest = n, rest[i:]
	if !strings.HasPrefix(rest, ".") {
		return v, false
	}
	rest = rest[1:]

	n, i, ok = digitsAt(rest, 0)
	if !ok {
		return v, false
	}
	v.Patch, rest = n, rest[i:]

	// 构建元数据不参与排序，直接丢掉。
	if i := strings.IndexByte(rest, '+'); i >= 0 {
		rest = rest[:i]
	}
	if rest == "" {
		return v, true
	}
	if !strings.HasPrefix(rest, "-") || len(rest) == 1 {
		return v, false
	}
	v.Pre = rest[1:]
	return v, true
}

func allDigits(parts []string) bool {
	for _, p := range parts {
		if p == "" {
			return false
		}
		for i := 0; i < len(p); i++ {
			if p[i] < '0' || p[i] > '9' {
				return false
			}
		}
	}
	return true
}

// Compare 按优先级比较：返回 -1 / 0 / 1。构建元数据不参与。
func (v Version) Compare(o Version) int {
	if v.Major != o.Major {
		return sign(v.Major - o.Major)
	}
	if v.Minor != o.Minor {
		return sign(v.Minor - o.Minor)
	}
	if v.Patch != o.Patch {
		return sign(v.Patch - o.Patch)
	}
	// 有预发布标识的比同号的正式版旧。
	if v.Pre == o.Pre {
		return 0
	}
	if v.Pre == "" {
		return 1
	}
	if o.Pre == "" {
		return -1
	}
	return comparePre(v.Pre, o.Pre)
}

func comparePre(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		if as[i] == bs[i] {
			continue
		}
		an, aNum := atoiStrict(as[i])
		bn, bNum := atoiStrict(bs[i])
		switch {
		case aNum && bNum:
			return sign(an - bn)
		case aNum: // 数字标识比字母小
			return -1
		case bNum:
			return 1
		default:
			return compareString(as[i], bs[i])
		}
	}
	return sign(len(as) - len(bs))
}

func atoiStrict(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

func compareString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func sign(n int) int {
	switch {
	case n > 0:
		return 1
	case n < 0:
		return -1
	default:
		return 0
	}
}

// IsUpdate 判断 latest 是否比 installed 新。
//
// 特例：有些平台只记录数字版本（macOS 的 CFBundleShortVersionString、
// Windows MSI 的 ProductVersion），预发布后缀会丢。若已安装的是 0.3.0、
// 最新是 0.3.0-rc.2，这不是更新 —— 否则会永远提示有新版。
func IsUpdate(latest, installed Version) bool {
	if installed.Pre == "" && latest.Pre != "" &&
		latest.Major == installed.Major && latest.Minor == installed.Minor && latest.Patch == installed.Patch {
		return false
	}
	return latest.Compare(installed) > 0
}
