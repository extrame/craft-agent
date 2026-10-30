// Package gateway 是主程序与命令行/MCP 之间的本地转发通道。
//
// 动机：craft-agent run 是一次性的 —— AI 客户端在沙箱里跑它，它用 exec.Command
// 启动的应用就挂在同一个进程组上，沙箱会话一结束整组被收割，应用窗口一闪而过。
// 应用应该由常驻的主程序启动并持有：主程序的进程组不随任何一次调用结束，
// 窗口就持续可见。CLI 与 MCP 只做薄客户端，把命令转发给主程序执行。
//
// 协议刻意简单：一行 JSON 请求、一行 JSON 响应，unix socket 上一次一来回。
// 传输选 unix socket 而不是 TCP 端口：不用挑端口、不用防别的用户连上来
// （socket 文件 0600，只有本用户能碰）。
package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// ErrUnavailable 表示主程序没有在监听：socket 不存在或连不上。
// 调用方据此决定降级直连 —— 这是唯一可以降级的错误，命令执行失败不行
// （命令可能有副作用，换一条路再执行一遍等于做两次）。
var ErrUnavailable = errors.New("主程序未在运行")

// Request 是客户端发来的一行。
type Request struct {
	App     string          `json:"app"`
	Command string          `json:"command"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response 是服务端回的一行。
type Response struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// Exec 由宿主（AppService）提供：真正去执行一条命令。
// 实现要自己处理「应用没开就去启动」（host.Ensure 的语义）与并发安全。
type Exec func(ctx context.Context, app, command string, params json.RawMessage) (json.RawMessage, error)

// SocketPath 是转发口的落点，与 registry/run 同目录。
func SocketPath(dataDir string) string {
	return filepath.Join(dataDir, "run", "gateway.sock")
}

// Serve 在 dataDir 下开始监听转发口。每个连接一个 goroutine，
// 同一连接内的请求按行串行执行。返回的 stop 停止监听并清掉 socket 文件。
func Serve(dataDir string, exec Exec) (stop func(), err error) {
	path := SocketPath(dataDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// 上一次异常退出留下的 socket 文件会让 Listen 报地址已占用，先清掉。
	// 有活着的监听者时不删 —— 删了会让那个服务端变成没人找得到的孤儿。
	if _, serr := os.Stat(path); serr == nil && !alive(path) {
		os.Remove(path)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("监听转发口失败: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveConn(conn, exec)
		}
	}()
	return func() {
		ln.Close()
		<-done
		os.Remove(path)
	}, nil
}

// alive 探测 socket 后面是否真有监听者。
func alive(path string) bool {
	conn, err := net.DialTimeout("unix", path, 500*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// serveConn 处理一个客户端连接：逐行读请求、执行、逐行回响应。
// 连接断了就结束；执行错不终止连接，客户端可以继续发下一条。
func serveConn(conn net.Conn, exec Exec) {
	defer conn.Close()
	scan := bufio.NewScanner(conn)
	scan.Buffer(make([]byte, 0, 64<<10), 8<<20) // params 可能是整张图片的 base64
	out := bufio.NewWriter(conn)
	for scan.Scan() {
		line := scan.Bytes()
		if len(line) == 0 {
			continue
		}
		var req Request
		resp := Response{}
		if err := json.Unmarshal(line, &req); err != nil {
			resp.Error = "请求无法解析: " + err.Error()
		} else {
			result, err := exec(connCtx(), req.App, req.Command, req.Params)
			if err != nil {
				resp.Error = err.Error()
			} else {
				resp.OK = true
				resp.Result = result
			}
		}
		body, err := json.Marshal(resp)
		if err != nil {
			body, _ = json.Marshal(Response{Error: "响应无法编码: " + err.Error()})
		}
		out.Write(body)
		out.WriteByte('\n')
		if out.Flush() != nil {
			return
		}
	}
}

// connCtx 目前与调用方生命周期一致；留一个口子，将来加每请求超时在这改。
func connCtx() context.Context { return context.Background() }

// Call 是客户端：把一条命令转发给主程序执行。
// socket 不存在或连不上时返回 ErrUnavailable —— 除此之外的错误都来自执行本身。
func Call(ctx context.Context, dataDir, app, command string, params json.RawMessage) (json.RawMessage, error) {
	path := SocketPath(dataDir)
	if _, err := os.Stat(path); err != nil {
		return nil, ErrUnavailable
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer conn.Close()

	req, err := json.Marshal(Request{App: app, Command: command, Params: params})
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return nil, fmt.Errorf("发送请求失败: %w", err)
	}

	// 读回一行响应。deadline 由 ctx 控制：到点还没回就当失败，
	// 主程序那边执行慢（启动应用要几秒）时 ctx 应该给足。
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	scan := bufio.NewScanner(conn)
	scan.Buffer(make([]byte, 0, 64<<10), 8<<20)
	if !scan.Scan() {
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		return nil, fmt.Errorf("主程序断开连接，没有返回结果")
	}
	var resp Response
	if err := json.Unmarshal(scan.Bytes(), &resp); err != nil {
		return nil, fmt.Errorf("响应无法解析: %w", err)
	}
	if !resp.OK {
		return nil, errors.New(resp.Error)
	}
	return resp.Result, nil
}
