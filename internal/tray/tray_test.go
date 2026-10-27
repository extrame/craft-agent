package tray

import (
	"bytes"
	"image/png"
	"testing"
)

// MCP 模式下根本不装托盘，但前端的 SetTrayLanguage 仍会照常调进来。
// 这些入口在没人 Install 的时候必须是空操作，不能崩，也不能改语言以外的状态。
func TestCallsBeforeInstallAreInert(t *testing.T) {
	SetLanguage("en")
	if lang != "en" {
		t.Fatalf("语言没有被记下来: %q", lang)
	}
	SetLanguage("klingon")
	if lang != "en" {
		t.Fatalf("不认识的语言不该生效: %q", lang)
	}

	SetOnline(2)
	if online != 2 {
		t.Fatalf("在线数没有被记下来: %d", online)
	}

	Show()
	OpenDataDir()
}

// 图标是构建期生成的 PNG，坏掉的话只会在菜单栏里显示空白，
// 运行时没有任何报错 —— 所以在测试里解一遍。
func TestIconsDecode(t *testing.T) {
	for name, data := range map[string][]byte{"template": iconTemplate, "colour": iconColour} {
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s 图标解不开: %v", name, err)
		}
		if b := img.Bounds(); b.Dx() == 0 || b.Dy() == 0 {
			t.Fatalf("%s 图标是空的", name)
		}
	}
}
