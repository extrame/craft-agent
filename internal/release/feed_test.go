package release

import (
	"testing"

	"craft-agent/internal/craft"
)

// 报文形状照 api.storyteller.ai/v1/craft_apps/release_info 的真实响应写，
// 额外加了改名与公告字段（当前为空，但发布侧随时可能填）。
const sampleFeed = `{
  "success": true,
  "release_info": {
    "apps": {
      "deckcraft": {
        "name": "DeckCraft",
        "repo": "storytold/deckcraft",
        "latest_release": {
          "tag": "v0.11.1",
          "version": "0.11.1",
          "name": "DeckCraft v0.11.1",
          "published_at": "2026-10-25T13:36:43Z",
          "html_url": "https://github.com/storytold/deckcraft/releases/tag/v0.11.1",
          "prerelease": false,
          "notes_markdown": "## What's Changed"
        }
      },
      "PdfCraft": {
        "name": "PdfCraft",
        "repo": "storytold/pdfcraft",
        "latest_release": { "tag": "v0.14.0", "published_at": "2026-10-20T00:00:00Z" }
      },
      "emptycraft": { "name": "Empty", "repo": "storytold/empty", "latest_release": null }
    },
    "last_fetched": "2026-10-27T00:00:00Z"
  },
  "app_renames": [],
  "repo_renames": [
    { "old_name": "Storytold/pdfcraft", "new_name": "storytold/pdfcraft-app", "renamed_at": null },
    { "old_name": "storytold/evil", "new_name": "someoneelse/evil", "renamed_at": null }
  ],
  "important_notices": { "announcements": [], "alerts": [] }
}`

func TestParseFeed(t *testing.T) {
	c := NewChecker(t.TempDir(), "test")
	apps, err := c.parseFeed([]byte(sampleFeed))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got := apps["deckcraft"].Version; got != "0.11.1" {
		t.Errorf("deckcraft 版本 = %q", got)
	}
	if got := apps["deckcraft"].Tag; got != "v0.11.1" {
		t.Errorf("deckcraft tag = %q", got)
	}
	// key 大小写不敏感：feed 里写的是 PdfCraft，清单里是 id。
	if got := apps["pdfcraft"].Version; got != "0.14.0" {
		t.Errorf("pdfcraft 版本 = %q", got)
	}
	if _, ok := apps["emptycraft"]; ok {
		t.Error("没有发布的条目不该出现在结果里")
	}

	// 仓库改名：本账号内跟，跨账号不跟。
	if got := c.RepoFor(craft.App{ID: "pdfcraft", Repo: "storytold/pdfcraft"}); got != "storytold/pdfcraft-app" {
		t.Errorf("改名没跟上: %q", got)
	}
	if got := c.RepoFor(craft.App{ID: "evil", Repo: "storytold/evil"}); got != "storytold/evil" {
		t.Errorf("跨账号的改名不该跟: %q", got)
	}
	if got := c.RepoFor(craft.App{ID: "deckcraft", Repo: "storytold/deckcraft"}); got != "storytold/deckcraft" {
		t.Errorf("没改名的仓库被改动了: %q", got)
	}
}

func TestParseFeedRejectsGarbage(t *testing.T) {
	c := NewChecker(t.TempDir(), "test")
	if _, err := c.parseFeed([]byte("<html>404</html>")); err == nil {
		t.Fatal("非 JSON 应当报错")
	}
	// 字段类型全错也不能崩：解析失败就当 feed 什么都没说。
	if _, err := c.parseFeed([]byte(`{"release_info":{"apps":{"x":5}}}`)); err == nil {
		t.Fatal("apps 里塞数字应当报错")
	}
}
