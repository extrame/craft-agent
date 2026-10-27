package services

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"craft-agent/internal/host"
	"craft-agent/internal/journal"
)

func newRuntimeService(t *testing.T) (*AppService, string) {
	t.Helper()
	dir := t.TempDir()
	h, err := host.New(host.Options{DataDir: dir})
	if err != nil {
		t.Fatalf("host.New: %v", err)
	}
	t.Cleanup(func() { h.Close() })
	j, err := journal.Open(dir)
	if err != nil {
		t.Fatalf("journal.Open: %v", err)
	}
	return NewAppService(h, j), dir
}

func TestMCPRuntimeViewsAndCleanup(t *testing.T) {
	s, dir := newRuntimeService(t)

	// 三个会话：活着的旧版本、活着的同版本、死了两天的僵尸。
	old := time.Now().Add(-48 * time.Hour)
	content := `{"sessions":{
	  "mcp:100":{"pid":` + itoa(os.Getpid()) + `,"version":"0.0.1","startedAt":"` + time.Now().Format(time.RFC3339) + `","apps":["DeckCraft"]},
	  "mcp:200":{"pid":` + itoa(os.Getpid()) + `,"version":"` + Version + `","startedAt":"` + time.Now().Format(time.RFC3339) + `"},
	  "mcp:300":{"pid":999999999,"version":"0.0.1","startedAt":"` + old.Format(time.RFC3339) + `"}
	}}`
	path := filepath.Join(dir, "runtime.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	got := s.MCPRuntime()
	if len(got) != 3 {
		t.Fatalf("应读到 3 条会话，实际 %d", len(got))
	}
	byID := map[string]MCPSessionView{}
	for _, v := range got {
		byID[v.Session] = v
	}
	if v := byID["mcp:100"]; !v.Outdated || !v.Alive || v.Version != "0.0.1" {
		t.Fatalf("旧版本活会话判定错: %+v", v)
	}
	if v := byID["mcp:200"]; v.Outdated {
		t.Fatalf("同版本会话不应标 outdated: %+v", v)
	}
	if v := byID["mcp:300"]; v.Alive {
		t.Fatal("pid 999999999 不应被判活")
	}

	// 读取一次后，死掉超 24h 的僵尸被顺手清走，活着的两条保留。
	raw, _ := os.ReadFile(path)
	var file struct {
		Sessions map[string]json.RawMessage `json:"sessions"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if _, ok := file.Sessions["mcp:300"]; ok {
		t.Fatal("僵尸条目应已被清理")
	}
	if len(file.Sessions) != 2 {
		t.Fatalf("活着的会话应保留，实际剩 %d", len(file.Sessions))
	}
}

func TestMCPRuntimeMissingFile(t *testing.T) {
	s, _ := newRuntimeService(t)
	if got := s.MCPRuntime(); got != nil {
		t.Fatalf("没有 runtime.json 应返回 nil，实际 %v", got)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
