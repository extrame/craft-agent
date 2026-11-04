package services

import "testing"

// 身份命令是每个应用的属性，不是可以按固定顺序试出来的东西。
// CADCraft 既没有 file.info 也没有 doc.list —— 拿这两条去探，等于每轮
// 白发两条它根本不认识命令。这个测试把归属钉死，别再退回「挨个试」。
func TestIdentityCommandIsDeclared(t *testing.T) {
	cases := map[string]string{
		"deckcraft": "file.info",
		"figcraft":  "doc.list",
		"cadcraft":  "drawing.inspect",
	}
	for app, want := range cases {
		if got := identityCommand(app); got != want {
			t.Errorf("%s 的身份命令应为 %q，实际 %q", app, want, got)
		}
	}
}

// 未在 catalog 声明过的应用才允许走试错（返回空串表示「不知道」）。
// 先清掉可能残留的「试出来的」记录，保证测的是声明而不是缓存。
func TestIdentityCommandUnknownAppFallsBack(t *testing.T) {
	const app = "futurecraft"
	delete(idSources, app)
	if got := identityCommand(app); got != "" {
		t.Fatalf("未声明的应用应返回空串（走试错），实际 %q", got)
	}
	idSources[app] = srcDocList
	defer delete(idSources, app)
	if got := identityCommand(app); got != srcDocList {
		t.Fatalf("试出来的来源应被沿用，实际 %q", got)
	}
}
