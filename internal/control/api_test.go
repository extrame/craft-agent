package control

import (
	"encoding/json"
	"strings"
	"testing"
)

// 两个应用的编码差异就在这几个字段上，而它们的差别足以让整张表解不出来。
const (
	deckRow   = `{"id":"text.bold","label":"Bold","menu":["Format","Text"],"params":"{}","shortcut":"Cmd+B","enabled":true}`
	figRow    = `{"id":"edit.undo","label":"Undo","menu":"Edit","params":"{}","shortcut":"Cmd+Z","undoable":true}`
	figBare   = `{"id":"node.create","label":"Create layer","menu":null,"params":"{}","shortcut":null,"undoable":true}`
	deckTable = `[` + deckRow + `,{"id":"slide.new","label":"New slide","menu":["Slide"],"params":"{}","shortcut":"","enabled":false}]`
	figTable  = `[` + figRow + `,` + figBare + `,{"id":"file.save","label":"Save","menu":"File","params":"{}","shortcut":"Cmd+S","undoable":false}]`
)

func TestCommandReadsDeckCraftShape(t *testing.T) {
	var c Command
	if err := json.Unmarshal([]byte(deckRow), &c); err != nil {
		t.Fatalf("解不开 DeckCraft 形态：%v", err)
	}
	if len(c.Menu) != 2 || c.Menu[0] != "Format" || c.Menu[1] != "Text" {
		t.Errorf("menu = %v，期望 [Format Text]", c.Menu)
	}
	if c.Enabled == nil || !*c.Enabled {
		t.Errorf("enabled 应当为 true，得到 %v", c.Enabled)
	}
	if c.Shortcut != "Cmd+B" {
		t.Errorf("shortcut = %q", c.Shortcut)
	}
}

func TestCommandReadsFigCraftShape(t *testing.T) {
	var c Command
	if err := json.Unmarshal([]byte(figRow), &c); err != nil {
		t.Fatalf("解不开 FigCraft 形态：%v", err)
	}
	if len(c.Menu) != 1 || c.Menu[0] != "Edit" {
		t.Errorf("字符串 menu 要归一成单元素切片，得到 %v", c.Menu)
	}
	if c.Enabled != nil {
		t.Errorf("没有 enabled 字段时应为 nil（未知），而不是 false；得到 %v", *c.Enabled)
	}
	if !c.Undoable {
		t.Error("undoable 没有读进来")
	}
}

func TestCommandToleratesNulls(t *testing.T) {
	var c Command
	if err := json.Unmarshal([]byte(figBare), &c); err != nil {
		t.Fatalf("null 应当能解：%v", err)
	}
	if c.Menu != nil {
		t.Errorf("menu=null 应当得到 nil，得到 %v", c.Menu)
	}
	if c.Shortcut != "" {
		t.Errorf("shortcut=null 应当得到空串，得到 %q", c.Shortcut)
	}
}

// 这是用户实际撞到的那件事：不是某一字段读错，是整张表都解不出来。
func TestWholeTablesParse(t *testing.T) {
	for name, table := range map[string]string{"deckcraft": deckTable, "figcraft": figTable} {
		var list []Command
		if err := json.Unmarshal([]byte(table), &list); err != nil {
			t.Fatalf("%s 的整张命令表解析失败：%v", name, err)
		}
		if len(list) != 2 && len(list) != 3 {
			t.Errorf("%s 解出 %d 条，数量不对", name, len(list))
		}
	}
}

// 两种形态混在同一张表里也不该出事——真实应用不会这么做，但兼容层不该假设。
func TestMixedTableParses(t *testing.T) {
	var list []Command
	if err := json.Unmarshal([]byte("["+deckRow+","+figRow+"]"), &list); err != nil {
		t.Fatalf("混合形态解析失败：%v", err)
	}
	if len(list) != 2 {
		t.Fatalf("解出 %d 条", len(list))
	}
}

// 完全认不出的形态要报错，并且不能把整张表塞进错误信息。
func TestMenuRejectsGarbage(t *testing.T) {
	var c Command
	err := json.Unmarshal([]byte(`{"id":"x","menu":12}`), &c)
	if err == nil {
		t.Fatal("数字 menu 应当报错")
	}
	if !strings.Contains(err.Error(), "menu") {
		t.Errorf("错误信息要提到 menu：%v", err)
	}
	if len(err.Error()) > 120 {
		t.Errorf("错误信息太长，会把日志淹掉：%d 字符", len(err.Error()))
	}
}
