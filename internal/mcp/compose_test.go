package mcpserver

import (
	"strings"
	"testing"

	"craft-agent/internal/craft"
)

// 以 deckcraft_ 开头的工具必须只对 DeckCraft 成立 —— 名字是承诺，
// 不能在一个 PDF 阅读器上「试试看」。
func TestDeckOnly(t *testing.T) {
	deck, ok := craft.ByID(craft.DeckCraftID)
	if !ok {
		t.Fatalf("套件清单里没有 %s", craft.DeckCraftID)
	}
	if err := deckOnly(deck, "deckcraft_text"); err != nil {
		t.Fatalf("DeckCraft 自己应该通过：%v", err)
	}

	pdf, ok := craft.ByID("pdfcraft")
	if !ok {
		t.Fatal("套件清单里没有 pdfcraft")
	}
	err := deckOnly(pdf, "deckcraft_text")
	if err == nil {
		t.Fatal("PdfCraft 不该通过 deckOnly")
	}
	// 拒绝不能只说「不行」：要给出能接着走的那条路。
	for _, want := range []string{"craft_run", "DeckCraft", pdf.Name} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("拒绝信息里缺 %q：%s", want, err)
		}
	}
}
