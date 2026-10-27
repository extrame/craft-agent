//go:build darwin

package update

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"craft-agent/internal/craft"
	"craft-agent/internal/release"
)

// 这个测试会真的下载一个安装包、挂载磁盘镜像、验签名、把应用装到临时目录。
// 它碰不到 /Applications（Dest 指向临时目录），但会花掉一次百兆流量，
// 所以默认跳过：CRAFT_UPDATE_LIVE=1 go test ./internal/update -run TestLive -v
func TestLiveInstallIntoScratchDir(t *testing.T) {
	if os.Getenv("CRAFT_UPDATE_LIVE") != "1" {
		t.Skip("设置 CRAFT_UPDATE_LIVE=1 才跑真实下载与安装")
	}
	app, ok := craft.ByID("deckcraft")
	if !ok {
		t.Fatal("清单里没有 deckcraft")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	c := NewClient("Craft-Agent-test")
	rel, err := c.Release(ctx, app.Repo, "")
	if err != nil {
		t.Fatalf("取发布失败: %v", err)
	}
	asset, err := Pick(app, rel)
	if err != nil {
		t.Fatalf("挑安装包失败: %v", err)
	}
	if asset, err = c.EnsureSum(ctx, app.Repo, rel, asset); err != nil {
		t.Fatalf("补校验和失败: %v", err)
	}

	work := t.TempDir()
	dest := t.TempDir()
	var stages []string
	out, err := Install(ctx, c, Options{
		App: app, Asset: asset, WorkDir: work, Dest: dest, Version: rel.Version,
		OnStage: func(s Stage) { stages = append(stages, s.Step) },
	})
	if err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	t.Logf("装好 %s 到 %s（进度阶段 %v）", out.Version, out.Path, dedupe(stages))

	if out.Version != rel.Version {
		t.Errorf("装上的版本 %q 与发布 %q 不一致", out.Version, rel.Version)
	}
	if out.Backup != "" {
		t.Errorf("干净的目录不该产生备份，得到 %s", out.Backup)
	}
	if _, err := os.Stat(filepath.Join(out.Path, "Contents", "MacOS", app.Name)); err != nil {
		t.Fatalf("装好的包里没有可执行文件: %v", err)
	}
	if got := release.ReadBundle(app, out.Path); got.Error != "" || got.Version == "" {
		t.Fatalf("回读失败: %+v", got)
	}
	if err := exec.Command("/usr/bin/codesign", "--verify", "--deep", "--strict", out.Path).Run(); err != nil {
		t.Fatalf("装好的包签名坏了: %v", err)
	}

	// 下载与暂存都不该留在工作目录里（镜像已卸载、临时目录已清）。
	entries, _ := os.ReadDir(work)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "mnt-") {
			t.Errorf("挂载点没清掉: %s", e.Name())
		}
	}
	if mounted, _ := exec.Command("/usr/bin/hdiutil", "info").Output(); strings.Contains(string(mounted), asset.Name) {
		t.Errorf("镜像仍然挂着: %s", asset.Name)
	}

	// 再装一次，这次应该留下旧副本备份 —— 覆盖安装是更新的常态。
	out2, err := Install(ctx, c, Options{
		App: app, Asset: asset, WorkDir: work, Dest: dest, Version: rel.Version,
	})
	if err != nil {
		t.Fatalf("覆盖安装失败: %v", err)
	}
	if out2.Backup == "" {
		t.Error("覆盖安装应当留下旧副本备份")
	} else if _, err := os.Stat(out2.Backup); err != nil {
		t.Errorf("备份不存在: %v", err)
	}
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
