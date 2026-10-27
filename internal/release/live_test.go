package release

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"craft-agent/internal/craft"
)

// 这组测试要打真机与真网络，默认跳过；验证时用 CRAFT_RELEASE_LIVE=1 go test 打开。

func TestDetectInstalledLive(t *testing.T) {
	if os.Getenv("CRAFT_RELEASE_LIVE") == "" {
		t.Skip("设置 CRAFT_RELEASE_LIVE=1 才跑真机检测")
	}
	app, ok := craft.ByID("deckcraft")
	if !ok || !app.Installed() {
		t.Skip("本机没装 DeckCraft")
	}
	got := Detect(app)
	t.Logf("DeckCraft 安装版本: %+v", got)
	if got.Version == "" {
		t.Fatalf("没有读到版本: %s", got.Error)
	}
}

func TestFeedLive(t *testing.T) {
	if os.Getenv("CRAFT_RELEASE_LIVE") == "" {
		t.Skip("设置 CRAFT_RELEASE_LIVE=1 才跑真网络")
	}
	dir := t.TempDir()
	c := NewChecker(dir, "test")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	snap, err := c.Check(ctx, craft.Suite, true)
	if err != nil {
		t.Fatalf("检查失败: %v", err)
	}
	t.Logf("source=%s apps=%d errors=%v", snap.Source, len(snap.Apps), snap.Errors)
	for _, id := range []string{"deckcraft", "pdfcraft", "wordcraft"} {
		if l, ok := snap.Apps[id]; ok {
			t.Logf("%s: v%s (%s) %s", id, l.Version, l.Tag, l.URL)
		} else {
			t.Errorf("%s 没有结果", id)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "releases.json")); err != nil {
		t.Errorf("缓存没有落盘: %v", err)
	}
}
