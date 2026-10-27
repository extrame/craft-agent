package update

import (
	"strings"
	"testing"

	"craft-agent/internal/craft"
)

// assets 是 storytold/deckcraft v0.11.1 的真实资产名（节选），
// 用真实数据测是因为「挑错资产」这件事只有在真实命名里才看得出来。
func assets() []Asset {
	names := []string{
		"deckcraft-0.11.1-freebsd-x86_64.tar.gz",
		"deckcraft-0.11.1-linux-aarch64.AppImage",
		"deckcraft-0.11.1-linux-x86_64.deb",
		"deckcraft-0.11.1-macos-universal.dmg",
		"deckcraft-0.11.1-windows-arm64-portable.zip",
		"deckcraft-0.11.1-windows-x64-portable.zip",
		"deckcraft-0.11.1-windows-x64.msi",
		"deckcraft-cli-0.11.1-macos-universal.zip",
		"deckcraft-web-0.11.1.zip",
		"SHA256SUMS.txt",
	}
	out := make([]Asset, 0, len(names))
	for _, n := range names {
		out = append(out, Asset{Name: n, Size: 1024, URL: "https://github.com/storytold/deckcraft/releases/download/v0.11.1/" + n})
	}
	return out
}

func TestPickPerPlatform(t *testing.T) {
	app, _ := craft.ByID("deckcraft")
	cases := []struct {
		goos, goarch, want string
	}{
		{"darwin", "arm64", "deckcraft-0.11.1-macos-universal.dmg"},
		{"darwin", "amd64", "deckcraft-0.11.1-macos-universal.dmg"},
		{"windows", "amd64", "deckcraft-0.11.1-windows-x64.msi"},
		{"windows", "arm64", "deckcraft-0.11.1-windows-arm64-portable.zip"},
		{"linux", "amd64", "deckcraft-0.11.1-linux-x86_64.deb"},
	}
	for _, c := range cases {
		got, err := pickFor(app, Release{Tag: "v0.11.1", Version: "0.11.1", Assets: assets()}, c.goos, c.goarch)
		if err != nil {
			t.Fatalf("%s/%s: %v", c.goos, c.goarch, err)
		}
		if got.Name != c.want {
			t.Errorf("%s/%s 选中 %s，期望 %s", c.goos, c.goarch, got.Name, c.want)
		}
	}
}

// 名字对不上时（版本号写法变了）要能按后缀兜底，且仍然不挑命令行版。
func TestPickFallsBackToSuffix(t *testing.T) {
	app, _ := craft.ByID("deckcraft")
	rel := Release{Version: "0.12.0", Assets: []Asset{
		{Name: "deckcraft-cli-0.12.0-macos-universal.zip"},
		{Name: "deckcraft_0.12.0_aarch64.dmg"},
		{Name: "deckcraft-web-0.12.0.zip"},
	}}
	got, err := pickFor(app, rel, "darwin", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "deckcraft_0.12.0_aarch64.dmg" {
		t.Errorf("选中 %s", got.Name)
	}
}

func TestPickMissingForPlatform(t *testing.T) {
	app, _ := craft.ByID("deckcraft")
	_, err := pickFor(app, Release{Version: "0.11.1", Assets: []Asset{{Name: "deckcraft-0.11.1-linux-x86_64.deb"}}}, "darwin", "arm64")
	if err == nil || !strings.Contains(err.Error(), "没有适用于") {
		t.Fatalf("期望「没有适用于」错误，得到 %v", err)
	}
}

func TestDigestHex(t *testing.T) {
	ok := "sha256:c0b4e89c004b31187832afce312f279f15f5adb7db61808db74608d822726f1d"
	if got := digestHex(ok); got != strings.TrimPrefix(ok, "sha256:") {
		t.Errorf("digest 归一失败: %q", got)
	}
	for _, bad := range []string{"", "sha256:", "sha256:zz", "md5:abc", ok + "0"} {
		if got := digestHex(bad); got != "" {
			t.Errorf("%q 应被拒绝，得到 %q", bad, got)
		}
	}
}

// 下载地址必须属于该应用自己的发布：别的仓库、点段、查询串一律不放行。
func TestValidAssetURL(t *testing.T) {
	repo := "storytold/deckcraft"
	good := "https://github.com/storytold/deckcraft/releases/download/v0.11.1/deckcraft-0.11.1-macos-universal.dmg"
	if !validAssetURL(repo, good) {
		t.Fatal("合法地址被判为非法")
	}
	for _, bad := range []string{
		"https://github.com/storytold/other/releases/download/v1/a.dmg",
		"https://evil.com/storytold/deckcraft/releases/download/v1/a.dmg",
		"http://github.com/storytold/deckcraft/releases/download/v1/a.dmg",
		"https://github.com/storytold/deckcraft/releases/download/../../etc/passwd",
		"https://github.com/storytold/deckcraft/releases/download/v1/a.dmg?x=1",
		"https://github.com/storytold/deckcraft/releases/download//a.dmg",
	} {
		if validAssetURL(repo, bad) {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
}

func TestSafeName(t *testing.T) {
	for _, ok := range []string{"a.dmg", "deckcraft-0.11.1-macos-universal.dmg", "SHA256SUMS.txt"} {
		if !safeName(ok) {
			t.Errorf("%q 应被接受", ok)
		}
	}
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`, "a\x00b", "a:b"} {
		if safeName(bad) {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
}

func TestArchName(t *testing.T) {
	cases := map[[2]string]string{
		{"darwin", "amd64"}:  "x86_64",
		{"darwin", "arm64"}:  "arm64",
		{"windows", "amd64"}: "x64",
		{"windows", "386"}:   "x86",
		{"windows", "arm64"}: "arm64",
		{"linux", "amd64"}:   "x86_64",
		{"linux", "arm64"}:   "aarch64",
	}
	for in, want := range cases {
		if got := archName(in[0], in[1]); got != want {
			t.Errorf("%v → %q，期望 %q", in, got, want)
		}
	}
}
