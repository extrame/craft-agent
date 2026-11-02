package craft

import "testing"

// TestForExt 钉住扩展名到应用的映射。
//
// 这些值来自各应用 bundle 自己声明的类型，不是我们定的 —— 所以特别容易被
// 「看着像」的猜测带偏（DeckCraft 的扩展名是 .deckcraft，不是 .deck）。
// 真值校验：mdls -name kMDItemContentType x.deckcraft
func TestForExt(t *testing.T) {
	cases := []struct {
		ext     string
		wantID  string
		wantHit bool
	}{
		{".deckcraft", "deckcraft", true},
		{".figcraft", "figcraft", true},
		{".dxf", "cadcraft", true},
		{".dwg", "cadcraft", true},
		{".DXF", "cadcraft", true}, // 大小写不敏感
		{".deck", "", false},       // 曾经的错值，不能被认领
		{".png", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		app, ok := ForExt(c.ext)
		if ok != c.wantHit || (ok && app.ID != c.wantID) {
			t.Errorf("ForExt(%q) = (%q, %v), want (%q, %v)", c.ext, app.ID, ok, c.wantID, c.wantHit)
		}
	}
}

// TestDocOpenPaired 有扩展名就必须有打开命令，否则分发会落不了地。
func TestDocOpenPaired(t *testing.T) {
	for _, a := range Suite {
		if len(a.DocExts) > 0 && a.DocOpen == "" {
			t.Errorf("%s 声明了扩展名 %v 但没有 DocOpen", a.ID, a.DocExts)
		}
	}
}
