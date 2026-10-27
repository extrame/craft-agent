package mcpserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// runtime.go 是 MCP 执行端落在盘上的自述：我是谁、什么版本、还活着吗。
//
// MCP 进程和 GUI 不共享内存：AI 客户端升级主程序后，还挂着的旧版 MCP 进程
// 会继续跑到客户端重启为止（替换文件动不了运行中的 inode）。这份文件让窗口
// 能看见这件事——哪个会话连着、版本对不对；版本偏了就提示用户重启 AI 客户端，
// 而不是让两边各自安静地跑各自的行为。
//
// 退出时进程会自己清条目；被 kill -9 的留给 GUI 用 PID 探活兜底，
// 死掉的条目在 GUI 读取时顺手清走。

type runtimeEntry struct {
	PID       int       `json:"pid"`
	Version   string    `json:"version"`
	StartedAt time.Time `json:"startedAt"`
	Apps      []string  `json:"apps,omitempty"`
}

// runtimeFile 的形状：session id（mcp:<pid>）→ 条目。
type runtimeFile struct {
	Sessions map[string]runtimeEntry `json:"sessions"`
}

func runtimePath(dataDir string) string {
	return filepath.Join(dataDir, "runtime.json")
}

func loadRuntime(dataDir string) runtimeFile {
	f := runtimeFile{Sessions: map[string]runtimeEntry{}}
	raw, err := os.ReadFile(runtimePath(dataDir))
	if err != nil {
		return f
	}
	_ = json.Unmarshal(raw, &f)
	if f.Sessions == nil {
		f.Sessions = map[string]runtimeEntry{}
	}
	return f
}

// saveRuntime 整份覆盖写。0600：文件里有 pid 与版本，属于本机运行状态。
func saveRuntime(dataDir string, f runtimeFile) {
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return
	}
	path := runtimePath(dataDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	_ = os.WriteFile(path, raw, 0o600)
}

// writeRuntime 登记一个会话。读-改-写：别的 AI 客户端可能同时连着，
// 不能整份覆盖别人的条目。
func writeRuntime(dataDir, session string, e runtimeEntry) {
	f := loadRuntime(dataDir)
	f.Sessions[session] = e
	saveRuntime(dataDir, f)
}

// clearRuntime 注销一个会话。最后一个条目清掉后文件整个删掉，不留空壳。
func clearRuntime(dataDir, session string) {
	f := loadRuntime(dataDir)
	if _, ok := f.Sessions[session]; !ok {
		return
	}
	delete(f.Sessions, session)
	if len(f.Sessions) == 0 {
		_ = os.Remove(runtimePath(dataDir))
		return
	}
	saveRuntime(dataDir, f)
}
