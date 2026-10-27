// Package journal 是一份跨进程的「操作流水」。
//
// 为什么需要它：MCP 服务器是 Cursor / Claude 自己拉起的独立进程，GUI 是另一个进程。
// agent 通过 MCP 改了用户的文件，界面必须能显示「刚才谁改了什么」。
// 两个进程没有共享内存，只能通过磁盘上的追加日志对齐。
//
// 格式是 JSON Lines：一行一条，只追加。文件超过上限时整体重写，保留尾部，
// 避免长跑之后无限增长。
package journal

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Entry 是一次可观察的动作。
type Entry struct {
	At       time.Time `json:"at"`
	Source   string    `json:"source"`  // gui | mcp
	Session  string    `json:"session"` // 会话标签：gui，或 mcp:<pid>
	Client   string    `json:"client,omitempty"`
	AppID    string    `json:"appId,omitempty"`
	Kind     string    `json:"kind"` // session-start | session-end | launch | connect | disconnect | command | error
	Command  string    `json:"command,omitempty"`
	OK       bool      `json:"ok"`
	Detail   string    `json:"detail,omitempty"`
	Duration int64     `json:"durationMs,omitempty"`
}

const (
	maxBytes = 4 << 20 // 4 MiB
	keepTail = 2000    // 轮转时保留的条数上限
)

// Journal 是日志句柄，可被多个 goroutine 并发写。
type Journal struct {
	path string
	mu   sync.Mutex
}

// Open 打开（或创建）数据目录下的 sessions.jsonl。
func Open(dataDir string) (*Journal, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	return &Journal{path: filepath.Join(dataDir, "sessions.jsonl")}, nil
}

// Path 返回日志文件位置，界面可以用「在访达中显示」。
func (j *Journal) Path() string { return j.path }

// Append 追加一条。写失败只丢弃这一条，不影响主流程 —— 流水不是业务数据。
func (j *Journal) Append(e Entry) {
	if j == nil {
		return
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	line = append(line, '\n')

	j.mu.Lock()
	defer j.mu.Unlock()

	if info, err := os.Stat(j.path); err == nil && info.Size() > maxBytes {
		j.rotateLocked()
	}
	f, err := os.OpenFile(j.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(line)
}

// rotateLocked 只保留尾部若干条，写回同一个文件。
func (j *Journal) rotateLocked() {
	entries, err := j.readLocked(0)
	if err != nil || len(entries) == 0 {
		return
	}
	if len(entries) > keepTail {
		entries = entries[len(entries)-keepTail:]
	}
	var b strings.Builder
	for _, e := range entries {
		line, err := json.Marshal(e)
		if err != nil {
			continue
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	_ = os.WriteFile(j.path, []byte(b.String()), 0o644)
}

// Read 返回最后 limit 条，时间正序（老的在前）。limit <= 0 表示全给。
func (j *Journal) Read(limit int) ([]Entry, error) {
	if j == nil {
		return nil, nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	entries, err := j.readLocked(0)
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(entries) > limit {
		entries = entries[len(entries)-limit:]
	}
	return entries, nil
}

// ReadSince 返回时间戳晚于 since 的条目，用于增量刷新。
func (j *Journal) ReadSince(since time.Time) ([]Entry, error) {
	if j == nil {
		return nil, nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	entries, err := j.readLocked(0)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if e.At.After(since) {
			out = append(out, e)
		}
	}
	return out, nil
}

func (j *Journal) readLocked(limit int) ([]Entry, error) {
	f, err := os.Open(j.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var entries []Entry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			// 写了一半的行（进程被杀）跳过即可，不要因此让整份流水不可读。
			continue
		}
		entries = append(entries, e)
	}
	if err := scanner.Err(); err != nil {
		return entries, err
	}
	if limit > 0 && len(entries) > limit {
		entries = entries[len(entries)-limit:]
	}
	return entries, nil
}
