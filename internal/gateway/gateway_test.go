package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// shortDir 是一个短路径的临时目录：macOS 的 unix socket 路径上限 104 字节，
// t.TempDir 的长随机路径会让 bind 失败 —— 那是测试环境的限制，不是实现的。
// 真实数据目录（~/Library/Application Support/CraftAgent/run/gateway.sock）远够短。
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "gw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// newServer 起一个用临时目录的转发口，exec 原样回显参数。
func newServer(t *testing.T) string {
	t.Helper()
	dir := shortDir(t)
	stop, err := Serve(dir, func(ctx context.Context, app, command string, params json.RawMessage) (json.RawMessage, error) {
		if app == "boom" {
			return nil, errors.New("应用炸了: " + command)
		}
		return json.Marshal(map[string]any{"app": app, "command": command, "echo": json.RawMessage(params)})
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return dir
}

func TestCallRoundTrip(t *testing.T) {
	dir := newServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := Call(ctx, dir, "deckcraft", "slide.new", json.RawMessage(`{"title":"你好"}`))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		App     string          `json:"app"`
		Command string          `json:"command"`
		Echo    json.RawMessage `json:"echo"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.App != "deckcraft" || got.Command != "slide.new" || string(got.Echo) != `{"title":"你好"}` {
		t.Fatalf("回显不符: %+v", got)
	}
}

func TestCallPropagatesExecError(t *testing.T) {
	dir := newServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := Call(ctx, dir, "boom", "slide.new", nil)
	if err == nil || err.Error() != "应用炸了: slide.new" {
		t.Fatalf("执行错误应原样传播，得到: %v", err)
	}
}

func TestCallUnavailable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// 空目录里没有 socket：必须返回 ErrUnavailable 而不是别的错，
	// 调用方靠它区分「降级直连」和「命令失败」。
	_, err := Call(ctx, t.TempDir(), "deckcraft", "slide.new", nil)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("应返回 ErrUnavailable，得到: %v", err)
	}
}

func TestCallUnavailableWhenSocketIsStale(t *testing.T) {
	// 死 socket（文件在、没人听）也要归为 ErrUnavailable。
	dir := shortDir(t)
	path := SocketPath(dir)
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte("junk"), 0o600)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := Call(ctx, dir, "deckcraft", "slide.new", nil)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("死 socket 应返回 ErrUnavailable，得到: %v", err)
	}
}

func TestServeReusesStaleSocket(t *testing.T) {
	// 上一次异常退出留下的死 socket 文件不应让重启失败。
	dir := shortDir(t)
	path := SocketPath(dir)
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte("junk"), 0o600)

	stop, err := Serve(dir, func(ctx context.Context, app, command string, params json.RawMessage) (json.RawMessage, error) {
		return json.Marshal(`ok`)
	})
	if err != nil {
		t.Fatalf("死 socket 残留时 Serve 不应失败: %v", err)
	}
	defer stop()
}
