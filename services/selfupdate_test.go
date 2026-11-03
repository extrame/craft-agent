package services

import (
	"testing"
	"time"

	"craft-agent/internal/craft"
	"craft-agent/internal/release"
)

func TestSelfUpdateOf(t *testing.T) {
	mk := func(latest string) release.Snapshot {
		return release.Snapshot{
			CheckedAt: time.Now(),
			Apps: map[string]release.Latest{
				craft.Self.ID: {Version: latest, Tag: "v" + latest, URL: "https://github.com/extrame/craft-agent/releases/tag/v" + latest},
			},
		}
	}

	t.Run("远端更新", func(t *testing.T) {
		got := selfUpdateOf(mk("99.0.0"))
		if !got.UpdateAvailable || got.LatestVersion != "99.0.0" || !got.Checked {
			t.Fatalf("远端比本机新应判可更新: %+v", got)
		}
	})
	t.Run("远端更旧或相同", func(t *testing.T) {
		for _, v := range []string{"0.0.1", Version} {
			if got := selfUpdateOf(mk(v)); got.UpdateAvailable {
				t.Fatalf("远端 %s 不应判可更新: %+v", v, got)
			}
		}
	})
	t.Run("没查到远端", func(t *testing.T) {
		got := selfUpdateOf(release.Snapshot{Apps: map[string]release.Latest{}})
		if got.Checked || got.UpdateAvailable || got.Version != Version {
			t.Fatalf("无远端信息不应判可更新: %+v", got)
		}
	})
}
