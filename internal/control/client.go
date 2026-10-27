// Package control 是 craft 应用「JSON 控制通道」的客户端。
//
// 协议（实测 DeckCraft 0.11.1，与 docs/control-protocol.md 一致）：
// 服务端在 127.0.0.1 上监听，一行一个 JSON 请求、一行一个 JSON 回复：
//
//	{"id":1,"method":"engine.execute","params":{"command":"slide.inspect"}}
//	{"id":1,"ok":true,"result":{...}}
//
// 出错时回 {"id":1,"ok":false,"error":"unknown command `x`"}。
// 请求非法（不是 JSON、或 method 不是字符串）时服务端直接关连接，所以这里的
// 调用失败后要丢弃连接，不能复用。
package control

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// Endpoint 是控制通道地址。Network 取 "tcp" 或 "unix"。
//
// 同一套协议，两种承载：DeckCraft 要 TCP 端口，PdfCraft 要 socket 文件路径。
type Endpoint struct {
	Network string `json:"network"`
	Address string `json:"address"`
}

// 传输名。跟 net.Dial 的 network 参数一致，直接透传。
const (
	NetworkTCP  = "tcp"
	NetworkUnix = "unix"
)

func (e Endpoint) String() string {
	if e.Network == "unix" {
		return "unix:" + e.Address
	}
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

// Request 是发出去的一行。
type Request struct {
	ID     int64  `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
}

// Response 是收到的一行。
type Response struct {
	ID     *int64          `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
}

// Client 复用到同一个应用的连接：craft 应用逐条处理请求，串行反而最稳。
type Client struct {
	ep      Endpoint
	timeout time.Duration

	mu     sync.Mutex
	conn   net.Conn
	reader *bufio.Reader
	seq    int64
}

// Dial 建立并验证一条连接。
func Dial(ctx context.Context, ep Endpoint, timeout time.Duration) (*Client, error) {
	c := &Client{ep: ep, timeout: timeout}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.connectLocked(ctx); err != nil {
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
	return nil
}

func (c *Client) reset() {
	if c.conn != nil {
		_ = c.conn.Close()
	}
	c.conn = nil
	c.reader = nil
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
// 网络层出错时连接会被丢弃并重试一次；应用返回 ok:false 时直接返回 *RemoteError，
// 不重试（重试会重复执行命令）。
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		if err := c.connectLocked(ctx); err != nil {
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
	return c.roundTripLocked(ctx, method, params)
}

func (c *Client) roundTripLocked(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.seq++
	id := c.seq

	payload, err := json.Marshal(Request{ID: id, Method: method, Params: params})
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
	if !resp.OK {
		msg := resp.Error
		if msg == "" {
			msg = "未知错误"
		}
		return nil, &RemoteError{Method: method, Message: msg}
	}
	// 清掉读超时，别让空闲连接带着过期 deadline。
	_ = c.conn.SetDeadline(time.Time{})
	return resp.Result, nil
}

func isConnError(err error) bool {
	if err == nil {
		return false
	}
	var re *RemoteError
	if errors.As(err, &re) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return errors.Is(err, net.ErrClosed) || errors.Is(err, context.DeadlineExceeded)
}
