package ids

import (
	"regexp"
	"testing"
)

// id 必须是 8 位 a-zA-Z0-9：界面上靠它区分并发会话，格式钉死别漂。
var pattern = regexp.MustCompile(`^[a-zA-Z0-9]{8}$`)

func TestNew(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := New()
		if !pattern.MatchString(id) {
			t.Fatalf("id %q 不符合 8 位 a-zA-Z0-9", id)
		}
		if seen[id] {
			t.Fatalf("1000 次生成里出现重复 id %q", id)
		}
		seen[id] = true
	}
}
