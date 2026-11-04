//go:build live

package services

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"craft-agent/internal/control"
)

// 真机验证：CADCraft 的身份只从 drawing.inspect 读，不碰 file.info / doc.list。
func TestLiveCadcraftIdentity(t *testing.T) {
	home, _ := os.UserHomeDir()
	raw, err := os.ReadFile(filepath.Join(home, "Library/Application Support/CraftAgent/registry.json"))
	if err != nil {
		t.Skip("没有注册表，跳过")
	}
	var reg struct {
		Apps map[string]struct {
			Endpoint struct {
				Network string `json:"network"`
				Address string `json:"address"`
			} `json:"endpoint"`
			Token string `json:"token"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(raw, &reg); err != nil {
		t.Fatal(err)
	}
	cc, ok := reg.Apps["cadcraft"]
	if !ok {
		t.Skip("CADCraft 未注册")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := control.Dial(ctx, control.Endpoint{Network: cc.Endpoint.Network, Address: cc.Endpoint.Address, Token: cc.Token}, 3*time.Second)
	if err != nil {
		t.Skipf("连不上 CADCraft: %v", err)
	}
	defer client.Close()

	if got := identityCommand("cadcraft"); got != srcDrawingInspect {
		t.Fatalf("identityCommand = %q, 应为 %q", got, srcDrawingInspect)
	}
	info, cmd, err := readIdentity(ctx, "cadcraft", client)
	if err != nil {
		t.Fatalf("readIdentity: %v", err)
	}
	t.Logf("实际命令=%q title=%q path=%q dirty=%v", cmd, info.Title, info.Path, info.Dirty)
	if cmd != srcDrawingInspect {
		t.Fatalf("实际用了 %q", cmd)
	}
	if info.Title == "" {
		t.Fatal("title 为空，说明读的不是当前图纸")
	}
}
