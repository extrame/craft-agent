//go:build darwin

package update

import (
	"path/filepath"
	"testing"

	"craft-agent/internal/craft"
)

func TestDestination(t *testing.T) {
	app, ok := craft.ByID("vectorcraft")
	if !ok {
		t.Fatal("套件清单里没有 vectorcraft")
	}

	// 显式指定目录：文件名按显示名生成，目录原样使用。
	dir, bundle := destination(Options{App: app, Dest: "/tmp/craft-test"}, "")
	if dir != "/tmp/craft-test" {
		t.Errorf("指定目录被改写：%s", dir)
	}
	if filepath.Base(bundle) != "VectorCraft.app" {
		t.Errorf("bundle 名不对：%s", bundle)
	}

	// 已装的副本放在家目录或别处时，升级必须装回原处，不能搬去 /Applications。
	existing := "/Users/someone/Applications/VectorCraft.app"
	dir, bundle = destination(Options{App: app}, existing)
	if dir != "/Users/someone/Applications" || bundle != existing {
		t.Errorf("没有跟随现有副本：%s / %s", dir, bundle)
	}

	// 首次安装：/Applications，写不进去才退回家目录的 Applications。
	// 这一步会探测目录可写性（家目录下可能因此多出一个空 Applications ——
	// 它本来就是首次安装要用的位置）。
	dir, bundle = destination(Options{App: app}, "")
	if filepath.Base(bundle) != "VectorCraft.app" {
		t.Errorf("bundle 名不对：%s", bundle)
	}
	home := filepath.Join(homeDir(), "Applications")
	if dir != "/Applications" && dir != home {
		t.Errorf("首次安装的目标目录不在预期之内：%s", dir)
	}
	if filepath.Dir(bundle) != dir {
		t.Errorf("bundle 不在目标目录里：%s vs %s", bundle, dir)
	}
}

// writable 的判据是「真的能落一个条目」，不是权限位。
func TestWritable(t *testing.T) {
	if !writable(t.TempDir()) {
		t.Error("临时目录应该可写")
	}
	if writable("/System/craft-agent-should-not-exist") {
		t.Error("系统目录不该被判定为可写")
	}
}
