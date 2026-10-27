package release

import "testing"

func mustParse(t *testing.T, s string) Version {
	t.Helper()
	v, ok := Parse(s)
	if !ok {
		t.Fatalf("Parse(%q) 失败", s)
	}
	return v
}

func TestParseTagsAndSpellings(t *testing.T) {
	cases := map[string]string{
		"v0.5.0":        "0.5.0",
		" 1.2.3\n":      "1.2.3",
		"V0.11.1":       "0.11.1",
		"0.5":           "0.5.0",
		"0.2.0-rc.1":    "0.2.0-rc.1",
		"0.2.0~rc.1-1":  "0.2.0-rc.1",
		"0.5.0+build.7": "0.5.0",
	}
	for in, want := range cases {
		if got := mustParse(t, in).String(); got != want {
			t.Errorf("Parse(%q) = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "v", "latest", "1", "1.x.0", "0.5.", ""} {
		if v, ok := Parse(bad); ok {
			t.Errorf("Parse(%q) 应该失败，得到 %v", bad, v)
		}
	}
}

func TestParseLooseFromCLIOutput(t *testing.T) {
	cases := map[string]string{
		"DeckCraft 0.11.1 (abc, 2026)": "0.11.1",
		"photocraft 0.7.0":             "0.7.0",
		"0.3.0.r47.g47f9306":           "0.3.0",
		"version: v1.2.3":              "1.2.3",
	}
	for in, want := range cases {
		v, ok := ParseLoose(in)
		if !ok || v.String() != want {
			t.Errorf("ParseLoose(%q) = %v %v, want %q", in, v, ok, want)
		}
	}
}

func TestOrderingAndUpdates(t *testing.T) {
	v := func(s string) Version { return mustParse(t, s) }

	if v("0.10.0").Compare(v("0.9.9")) <= 0 {
		t.Error("0.10.0 应大于 0.9.9")
	}
	if v("0.2.0").Compare(v("0.2.0-rc.5")) <= 0 {
		t.Error("正式版应大于同号预发布")
	}
	if v("0.2.0+abc").Compare(v("0.2.0")) != 0 {
		t.Error("构建元数据不应影响排序")
	}
	if !IsUpdate(v("0.5.0"), v("0.4.1")) {
		t.Error("0.5.0 相对 0.4.1 应提示更新")
	}
	if IsUpdate(v("0.5.0"), v("0.5.0")) {
		t.Error("同版本不应提示更新")
	}
	if IsUpdate(v("0.5.0"), v("0.6.0")) {
		t.Error("旧版本不应提示更新")
	}
	// 平台把预发布后缀丢了（0.3.0-rc.2 记成 0.3.0）时不应永远提示。
	if IsUpdate(v("0.3.0-rc.2"), v("0.3.0")) {
		t.Error("数字版 0.3.0 对 0.3.0-rc.2 不应提示更新")
	}
}
