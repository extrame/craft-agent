// Package control 是 craft 应用「JSON 控制通道」的客户端。
//
// 一行一个 JSON 请求、一行一个 JSON 回复：
//
//	{"id":1,"method":"engine.execute","params":{"command":"slide.inspect"}}
//	{"id":1,"ok":true,"result":{...}}
//
// 这套协议在套件里已经演化出三代，同一个客户端都要能说：
//
//	                DeckCraft 0.11   CADCraft 0.13      PdfCraft 0.14
//	启动参数        --control PORT   --control PORT     --control PATH
//	端口            PORT 自己听       PORT 自己听        应用自己挑，写在会合文件里
//	鉴权            无              请求顶层 token     先 auth {token}
//	方法名          engine.commands ui.commands        ui.commands
//	回复            {ok:true}        {ok:true}          {"jsonrpc":"2.0","result":…}
//
// 所以这里不做「按版本分支」：鉴权按端点里有没有 token 决定，方法名第一次用哪个由
// 服务端答复「unknown method」自己说出来，之后记住。新增一代应用不需要改这个包。
//
// 请求非法（不是 JSON、或 method 不是字符串）时服务端直接关连接，所以这里的
// 调用失败后要丢弃连接，不能复用。
package control

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// Endpoint 是控制通道地址。
//
// Token 不进 JSON：它会随 Status 一路推到界面和 MCP 工具结果里，而那是应用
// 用来证明「请求来自本机这个进程」的凭据。落盘由 host 的注册表单独存。
type Endpoint struct {
	Network string `json:"network"`
	Address string `json:"address"`

	Token string `json:"-"`
}

// 传输名。跟 net.Dial 的 network 参数一致，直接透传。
const (
	NetworkTCP = "tcp"
)

func (e Endpoint) String() string {
	return e.Address
}

// RemoteError 是应用端明确返回的失败，不是网络问题 —— 调用方可以原样展示给用户/模型。
type RemoteError struct {
	Method  string
	Message string
}

func (e *RemoteError) Error() string {
	return fmt.Sprintf("%s 被拒绝: %s", e.Method, e.Message)
}

// AuthError 是令牌缺失或不对。它和普通「命令被拒」不一样：再试也没用，
// 得回到启动参数或会合文件那一层去查。
type AuthError struct {
	Method  string
	Message string
}

func (e *AuthError) Error() string {
	return fmt.Sprintf("%s 鉴权失败: %s", e.Method, e.Message)
}

// Request 是发出去的一行。Token 只在需要它的那一代服务端上有意义，
// 其余的实现会忽略多出来的字段。
type Request struct {
	ID     int64  `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
	Token  string `json:"token,omitempty"`
}

// Response 是收到的一行，兼容三种信封：
//
//	{"id":1,"ok":true,"result":…}
//	{"id":1,"ok":false,"error":"unauthorized: …"}
//	{"jsonrpc":"2.0","id":1,"result":…}
//	{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"authenticate first: auth {token}"}}
//
// Error 用 RawMessage 是因为它既可能是字符串也可能是对象；按 string 解会让
// 整个回复解析失败，连「为什么失败」都丢掉了。
type Response struct {
	ID      *int64          `json:"id"`
	OK      *bool           `json:"ok"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
	JSONRPC string          `json:"jsonrpc"`
}

func (r Response) errText() string {
	body := bytes.TrimSpace(r.Error)
	if len(body) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(body, &s); err == nil {
		return s
	}
	var obj struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &obj); err == nil {
		return obj.Message
	}
	return string(body)
}

// 方法族。同一个语义在不同代里换了名字，候选按「老一代优先」排：
// 新应用只认 ui.*，第一次探测失败一次就学会了。
const (
	kindCommands = "commands"
	kindExecute  = "execute"
	kindInspect  = "inspect"
	kindState    = "state"
)

var methodCandidates = map[string][]string{
	kindCommands: {"engine.commands", "ui.commands"},
	kindExecute:  {"engine.execute", "ui.command"},
	kindInspect:  {"document.inspect", "ui.inspect"},
	kindState:    {"ui.inspect", "ui.state"},
}

// Client 复用到同一个应用的连接：craft 应用逐条处理请求，串行反而最稳。
type Client struct {
	ep      Endpoint
	timeout time.Duration

	mu     sync.Mutex
	conn   net.Conn
	reader *bufio.Reader
	seq    int64

	// 探测出来的方法名，按 kind 记。连接断了不清：换的是连接，不是应用的世代。
	methods map[string]string
	authed  bool
}

// Dial 建立并验证一条连接。端点上带 token 时先做鉴权握手。
func Dial(ctx context.Context, ep Endpoint, timeout time.Duration) (*Client, error) {
	c := &Client{ep: ep, timeout: timeout, methods: map[string]string{}}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.connectLocked(ctx); err != nil {
		return nil, err
	}
	if err := c.authenticateLocked(ctx); err != nil {
		c.reset()
		return nil, err
	}
	return c, nil
}

// Endpoint 返回这条连接对应的地址。
func (c *Client) Endpoint() Endpoint { return c.ep }

func (c *Client) connectLocked(ctx context.Context) error {
	dialer := net.Dialer{Timeout: c.timeout}
	conn, err := dialer.DialContext(ctx, c.ep.Network, c.ep.Address)
	if err != nil {
		return fmt.Errorf("连接 %s 失败: %w", c.ep, err)
	}
	c.conn = conn
	c.reader = bufio.NewReaderSize(conn, 1<<20)
	c.authed = false
	return nil
}

// authenticateLocked 做 auth 握手：有的应用要求先认证才接受任何方法。
//
// 不做成「版本判断」：没有 token 就跳过；有 token 就试一次 auth，服务端不认这个方法
// （老一代把 token 放在每条请求的顶层）不影响后续调用，认这个方法但令牌错则直接失败。
func (c *Client) authenticateLocked(ctx context.Context) error {
	if c.ep.Token == "" || c.authed {
		return nil
	}
	raw, err := c.roundTripLocked(ctx, "auth", map[string]any{"token": c.ep.Token})
	if err != nil {
		var re *RemoteError
		if errors.As(err, &re) && isUnknownMethod(re.Message) {
			// 这一代没有 auth 方法：token 随每条请求走顶层字段。
			c.authed = true
			return nil
		}
		return err
	}
	_ = raw
	c.authed = true
	return nil
}

func (c *Client) reset() {
	if c.conn != nil {
		_ = c.conn.Close()
	}
	c.conn = nil
	c.reader = nil
	c.authed = false
}

// Close 断开连接。
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	c.reader = nil
	c.authed = false
	return err
}

// Alive 报告底层连接是否还开着（不发起 IO）。
func (c *Client) Alive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

// Call 发一条请求并读回一条回复。
//
// 网络层出错时连接会被丢弃并重试一次；应用返回错误时直接返回，不重试
// （重试会重复执行命令）。
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.callLocked(ctx, method, params)
}

func (c *Client) callLocked(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if c.conn == nil {
		if err := c.connectLocked(ctx); err != nil {
			return nil, err
		}
		if err := c.authenticateLocked(ctx); err != nil {
			return nil, err
		}
	}

	raw, err := c.roundTripLocked(ctx, method, params)
	if err == nil {
		return raw, nil
	}
	if !isConnError(err) {
		return nil, err
	}
	// 连接被对端关了（例如上次请求格式非法）：重连后再试一次。
	c.reset()
	if err := c.connectLocked(ctx); err != nil {
		return nil, err
	}
	if err := c.authenticateLocked(ctx); err != nil {
		return nil, err
	}
	return c.roundTripLocked(ctx, method, params)
}

// callKind 按方法族调用：先试已知名字，没有就按候选顺序探测，命中的记下来。
func (c *Client) callKind(ctx context.Context, kind string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if method := c.methods[kind]; method != "" {
		return c.callLocked(ctx, method, params)
	}
	var lastErr error
	for _, method := range methodCandidates[kind] {
		raw, err := c.callLocked(ctx, method, params)
		if err == nil {
			c.methods[kind] = method
			return raw, nil
		}
		lastErr = err
		var re *RemoteError
		if !errors.As(err, &re) || !isUnknownMethod(re.Message) {
			return nil, err
		}
	}
	return nil, lastErr
}

func (c *Client) roundTripLocked(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.seq++
	id := c.seq

	payload, err := json.Marshal(Request{ID: id, Method: method, Params: params, Token: c.ep.Token})
	if err != nil {
		return nil, fmt.Errorf("编码请求失败: %w", err)
	}
	payload = append(payload, '\n')

	deadline := time.Now().Add(c.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = c.conn.SetDeadline(deadline)

	if _, err := c.conn.Write(payload); err != nil {
		return nil, fmt.Errorf("发送 %s 失败: %w", method, err)
	}
	line, err := c.reader.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("读取 %s 回复失败: %w", method, err)
	}

	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("回复不是合法 JSON: %w", err)
	}
	if resp.ID != nil && *resp.ID != id {
		// 理论上不会发生（请求串行），出现了说明通道被别的客户端插了话。
		return nil, fmt.Errorf("回复 id 不匹配: 期望 %d, 得到 %d", id, *resp.ID)
	}
	if msg := resp.errText(); msg != "" {
		if isAuthProblem(msg) {
			return nil, &AuthError{Method: method, Message: msg}
		}
		return nil, &RemoteError{Method: method, Message: msg}
	}
	if resp.OK != nil && !*resp.OK {
		msg := resp.errText()
		if msg == "" {
			msg = "未知错误"
		}
		return nil, &RemoteError{Method: method, Message: msg}
	}
	// 清掉读超时，别让空闲连接带着过期 deadline。
	_ = c.conn.SetDeadline(time.Time{})
	return resp.Result, nil
}

// isUnknownMethod 认出「这一代没有这个方法」。服务端把可用方法列在消息里，
// 正好用来换下一个候选。
func isUnknownMethod(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "unknown method") || strings.Contains(m, "no such method")
}

func isAuthProblem(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "unauthorized") ||
		strings.Contains(m, "authenticate") ||
		strings.Contains(m, "control token") ||
		strings.Contains(m, "invalid token")
}

func isConnError(err error) bool {
	if err == nil {
		return false
	}
	var re *RemoteError
	if errors.As(err, &re) {
		return false
	}
	var ae *AuthError
	if errors.As(err, &ae) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return errors.Is(err, net.ErrClosed) || errors.Is(err, context.DeadlineExceeded)
}
