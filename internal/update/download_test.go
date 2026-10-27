package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// payload 造一段可重复的字节：用伪随机填充，压缩不了，长度也可控。
func payload(n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i*7 + i/253)
	}
	return out
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// flaky 是个会按剧本出问题的下载服务：第 n 次请求按 drops[n] 写到一半断开，
// 或者按 stalls[n] 写到一半卡住。Range 是照 GitHub 的行为实现的。
type flaky struct {
	data   []byte
	drops  []int64
	stalls []time.Duration

	mu     sync.Mutex
	calls  int
	ranges []string
}

func (f *flaky) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	n := f.calls
	f.calls++
	f.ranges = append(f.ranges, r.Header.Get("Range"))
	f.mu.Unlock()

	var start int64
	if v := r.Header.Get("Range"); v != "" {
		s, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(v, "bytes="), "-"), 10, 64)
		if err != nil || s < 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if s >= int64(len(f.data)) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(f.data)))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		start = s
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(f.data)-1, len(f.data)))
		w.Header().Set("Content-Length", strconv.Itoa(len(f.data)-int(start)))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", strconv.Itoa(len(f.data)))
		w.WriteHeader(http.StatusOK)
	}

	body := f.data[start:]
	cut := len(body)
	if n < len(f.drops) && f.drops[n] > 0 && int(f.drops[n]) < cut {
		cut = int(f.drops[n])
	}
	if cut > 0 {
		_, _ = w.Write(body[:cut])
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	if cut < len(body) {
		// 主动掐断：把连接拿走关掉，客户端读到的是「连接断了」而不是正常的结尾。
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close()
				return
			}
		}
		return
	}
	if n < len(f.stalls) && f.stalls[n] > 0 {
		time.Sleep(f.stalls[n])
	}
}

func (f *flaky) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *flaky) saw() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ranges...)
}

// newTestClient 用默认传输，只把等待时间调小，免得测试真的等 1 秒。
func newTestClient() *Client {
	c := NewClient("test")
	c.retryBase = 5 * time.Millisecond
	c.stallLimit = 5 * time.Second
	return c
}

func assetFor(data []byte, url string) Asset {
	return Asset{Name: "pkg.zip", Size: int64(len(data)), URL: url, SHA256: sum(data)}
}

// 第一次连接在对端断开后，第二次请求要带 Range 从断点续，最终文件完整。
func TestDownloadResumesAfterConnectionDrop(t *testing.T) {
	data := payload(1 << 20)
	f := &flaky{data: data, drops: []int64{300 << 10}}
	srv := httptest.NewServer(f)
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "pkg.zip")
	var lastDone int64
	err := newTestClient().Download(context.Background(), assetFor(data, srv.URL), dest, func(done, total int64) {
		lastDone = done
	})
	if err != nil {
		t.Fatalf("下载应当续传成功，得到 %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatalf("内容不对：%d 字节，期望 %d", len(got), len(data))
	}
	if lastDone != int64(len(data)) {
		t.Fatalf("最后一次进度是 %d，期望 %d", lastDone, len(data))
	}
	if f.count() < 2 {
		t.Fatalf("应当至少重连一次，实际请求 %d 次", f.count())
	}
	ranges := f.saw()
	if len(ranges) < 2 || ranges[1] != fmt.Sprintf("bytes=%d-", 300<<10) {
		t.Fatalf("第二次请求没带断点：%q", ranges)
	}
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
		t.Fatal("下完之后 .part 应当已经改名，不该残留")
	}
}

// 连接不断，但服务器写到一半就不动了：不能一直等，要掐掉重来并续传。
func TestDownloadRestartsWhenBodyStalls(t *testing.T) {
	data := payload(400 << 10)
	f := &flaky{data: data, stalls: []time.Duration{800 * time.Millisecond}}
	srv := httptest.NewServer(f)
	defer srv.Close()

	c := newTestClient()
	c.stallLimit = 100 * time.Millisecond

	dest := filepath.Join(t.TempDir(), "pkg.zip")
	started := time.Now()
	if err := c.Download(context.Background(), assetFor(data, srv.URL), dest, nil); err != nil {
		t.Fatalf("卡住之后应当续传完成，得到 %v", err)
	}
	if waited := time.Since(started); waited > 2*time.Second {
		t.Fatalf("卡住没有及时中断，等了 %s", waited)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatalf("内容不对：%d 字节", len(got))
	}
}

// 校验和不符：清掉重来一次，仍不对就报错，且不留下半个文件。
func TestDownloadRejectsBadChecksum(t *testing.T) {
	data := payload(64 << 10)
	f := &flaky{data: data}
	srv := httptest.NewServer(f)
	defer srv.Close()

	a := assetFor(data, srv.URL)
	a.SHA256 = strings.Repeat("0", 64)
	dest := filepath.Join(t.TempDir(), "pkg.zip")

	err := newTestClient().Download(context.Background(), a, dest, nil)
	if err == nil || !strings.Contains(err.Error(), "校验和不符") {
		t.Fatalf("期望校验失败，得到 %v", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatal("校验不过的文件不该留在最终位置")
	}
	if _, statErr := os.Stat(dest + ".part"); !os.IsNotExist(statErr) {
		t.Fatal("校验不过的 .part 应当被清掉")
	}
	if f.count() != 2 {
		t.Fatalf("校验失败后应当从零重下一次（共 2 次请求），实际 %d", f.count())
	}
}

// 已经有一份校验通过的同名文件（上一次装到一半、或者同名版本），不再花流量。
func TestDownloadReusesVerifiedFile(t *testing.T) {
	data := payload(32 << 10)
	f := &flaky{data: data}
	srv := httptest.NewServer(f)
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "pkg.zip")
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		t.Fatal(err)
	}
	var done, total int64
	if err := newTestClient().Download(context.Background(), assetFor(data, srv.URL), dest, func(d, tt int64) {
		done, total = d, tt
	}); err != nil {
		t.Fatalf("已有完整文件时不该报错：%v", err)
	}
	if f.count() != 0 {
		t.Fatalf("不该发请求，实际 %d 次", f.count())
	}
	if done != int64(len(data)) || total != int64(len(data)) {
		t.Fatalf("进度应当是 100%%，得到 %d/%d", done, total)
	}
}

// 服务器不接受 Range（200 而不是 206）：把 .part 作废，从头写。
func TestDownloadRestartsWhenServerIgnoresRange(t *testing.T) {
	data := payload(200 << 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "pkg.zip")
	if err := os.WriteFile(dest+".part", payload(50<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := newTestClient().Download(context.Background(), assetFor(data, srv.URL), dest, nil); err != nil {
		t.Fatalf("服务器忽略 Range 时应当从头下完，得到 %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatalf("内容不对：%d 字节", len(got))
	}
}

func TestContentRangeStart(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   int64
		ok     bool
	}{
		{"bytes 1000-1999/2000", 1000, true},
		{"bytes 0-99/100", 0, true},
		{"", 0, false},
		{"items 0-1/2", 0, false},
		{"bytes x-1/2", 0, false},
	} {
		got, err := contentRangeStart(tc.header)
		if tc.ok && (err != nil || got != tc.want) {
			t.Fatalf("%q：得到 %d, %v", tc.header, got, err)
		}
		if !tc.ok && err == nil {
			t.Fatalf("%q：应当报错", tc.header)
		}
	}
}
