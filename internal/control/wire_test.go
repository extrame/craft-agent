package control

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// 这一组测试用假服务端固定住三代协议的实际形状。真机测试（live_test.go）会连
// 真的应用，但它的前提是本机装着那些应用、并且版本没变；协议回归不该依赖这个。
//
//   genA  DeckCraft/FigCraft：{ok:true}，方法名 engine.*，无鉴权
//   genB  CADCraft：{ok:true}，方法名 engine.*，每条请求要顶层 token
//   genC  PdfCraft：JSON-RPC，先 auth，方法名 ui.*，结果包一层 {commands: …}

type fakeServer struct {
	t    *testing.T
	ln   net.Listener
	mu   sync.Mutex
	seen []string // 收到的方法名，按顺序
}

func newFakeServer(t *testing.T, handler func(method string, req map[string]any) string) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	s := &fakeServer{t: t, ln: ln}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn, handler)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *fakeServer) endpoint(token string) Endpoint {
	return Endpoint{Network: NetworkTCP, Address: s.ln.Addr().String(), Token: token}
}

func (s *fakeServer) methods() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.seen...)
}

func (s *fakeServer) serve(conn net.Conn, handler func(string, map[string]any) string) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var req map[string]any
		if err := json.Unmarshal(line, &req); err != nil {
			return
		}
		method, _ := req["method"].(string)
		s.mu.Lock()
		s.seen = append(s.seen, method)
		s.mu.Unlock()
		if reply := handler(method, req); reply != "" {
			if _, err := conn.Write([]byte(reply + "\n")); err != nil {
				return
			}
		}
	}
}

func replyOK(id any, result any) string {
	raw, _ := json.Marshal(map[string]any{"id": id, "ok": true, "result": result})
	return string(raw)
}

func replyErr(id any, msg string) string {
	raw, _ := json.Marshal(map[string]any{"id": id, "ok": false, "error": msg})
	return string(raw)
}

func replyRPC(id any, result any) string {
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	return string(raw)
}

func replyRPCErr(id any, code int, msg string) string {
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}})
	return string(raw)
}

var sampleCommands = []map[string]any{
	{"id": "file.open", "label": "Open", "menu": []string{"File"}, "params": "", "shortcut": "Cmd+O"},
	{"id": "engine.ping", "label": "Ping", "menu": nil, "params": "", "shortcut": ""},
}

// genA：不鉴权，方法名 engine.*。
func TestClientGenerationA(t *testing.T) {
	srv := newFakeServer(t, func(method string, req map[string]any) string {
		if method == "engine.commands" {
			return replyOK(req["id"], sampleCommands)
		}
		return replyErr(req["id"], fmt.Sprintf("unknown method %q", method))
	})
	c, err := Dial(context.Background(), srv.endpoint(""), 2*time.Second)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer c.Close()

	cmds, err := c.Commands(context.Background())
	if err != nil {
		t.Fatalf("取命令表失败: %v", err)
	}
	if len(cmds) != 2 || cmds[0].ID != "file.open" || cmds[1].Menu != nil {
		t.Fatalf("命令表解析不对: %+v", cmds)
	}
}

// genB：令牌放在请求顶层；服务端不认识 auth 方法。
func TestClientGenerationBToken(t *testing.T) {
	const token = "e6e7c0ffee"
	srv := newFakeServer(t, func(method string, req map[string]any) string {
		if got, _ := req["token"].(string); got != token {
			return replyErr(req["id"], "unauthorized: missing or invalid control token")
		}
		if method == "engine.commands" {
			return replyOK(req["id"], sampleCommands)
		}
		return replyErr(req["id"], fmt.Sprintf("unknown method %q", method))
	})

	// 不带令牌：应明确报鉴权错误，而不是含糊的「命令被拒」。
	noToken, err := Dial(context.Background(), srv.endpoint(""), 2*time.Second)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	if _, err := noToken.Commands(context.Background()); err == nil {
		t.Fatal("没令牌却拿到了命令表")
	} else {
		var ae *AuthError
		if !errors.As(err, &ae) {
			t.Fatalf("错误类型不对: %T %v", err, err)
		}
	}
	_ = noToken.Close()

	// 带令牌：auth 方法被拒不算失败，请求顶层带着 token 就通了。
	before := len(srv.methods())
	c, err := Dial(context.Background(), srv.endpoint(token), 2*time.Second)
	if err != nil {
		t.Fatalf("带令牌连接失败: %v", err)
	}
	defer c.Close()
	cmds, err := c.Commands(context.Background())
	if err != nil {
		t.Fatalf("带令牌取命令表失败: %v", err)
	}
	if len(cmds) != 2 {
		t.Fatalf("命令表条数不对: %d", len(cmds))
	}
	if got := srv.methods()[before:]; len(got) == 0 || got[0] != "auth" {
		t.Fatalf("带令牌连接的第一件事应该是鉴权握手，实际顺序 %v", got)
	}
}

// genC：JSON-RPC，先 auth，方法名 ui.*，结果包在 {commands: […]} 里。
func TestClientGenerationCJSONRPC(t *testing.T) {
	const token = "fc73ac28"
	var authed bool
	var mu sync.Mutex
	srv := newFakeServer(t, func(method string, req map[string]any) string {
		mu.Lock()
		ok := authed
		mu.Unlock()
		if method == "auth" {
			params, _ := req["params"].(map[string]any)
			if got, _ := params["token"].(string); got != token {
				return replyRPCErr(req["id"], -32001, "invalid token")
			}
			mu.Lock()
			authed = true
			mu.Unlock()
			return replyRPC(req["id"], map[string]any{"ok": true})
		}
		if !ok {
			return replyRPCErr(req["id"], -32001, "authenticate first: auth {token}")
		}
		switch method {
		case "ui.commands":
			return replyRPC(req["id"], map[string]any{"commands": sampleCommands})
		case "engine.commands":
			return replyRPCErr(req["id"], -32000, `unknown method "engine.commands" (ui.commands, ui.command)`)
		}
		return replyRPCErr(req["id"], -32000, fmt.Sprintf("unknown method %q", method))
	})

	c, err := Dial(context.Background(), srv.endpoint(token), 2*time.Second)
	if err != nil {
		t.Fatalf("连接失败: %v", err)
	}
	defer c.Close()

	cmds, err := c.Commands(context.Background())
	if err != nil {
		t.Fatalf("取命令表失败: %v", err)
	}
	if len(cmds) != 2 {
		t.Fatalf("包在 {commands:…} 里的结果没解出来: %d", len(cmds))
	}
	if got := strings.Join(srv.methods(), ","); !strings.Contains(got, "engine.commands,ui.commands") {
		t.Fatalf("没有从 engine.commands 退化到 ui.commands: %v", srv.methods())
	}

	// 第二次调用应直接用记住的方法名，不再探一遍。
	before := len(srv.methods())
	if _, err := c.Commands(context.Background()); err != nil {
		t.Fatalf("第二次取命令表失败: %v", err)
	}
	after := srv.methods()
	if len(after)-before != 1 || after[len(after)-1] != "ui.commands" {
		t.Fatalf("方法名没有被记住: %v", after[before:])
	}
}

// 令牌错误应该当场失败，而不是退化成「连接失败」让人以为应用没起来。
func TestClientWrongToken(t *testing.T) {
	srv := newFakeServer(t, func(method string, req map[string]any) string {
		if method == "auth" {
			return replyRPCErr(req["id"], -32001, "invalid token")
		}
		return replyRPCErr(req["id"], -32001, "authenticate first")
	})
	_, err := Dial(context.Background(), srv.endpoint("wrong"), 2*time.Second)
	if err == nil {
		t.Fatal("错误的令牌没有报错")
	}
	var ae *AuthError
	if !errors.As(err, &ae) {
		t.Fatalf("错误类型不对: %T %v", err, err)
	}
}
