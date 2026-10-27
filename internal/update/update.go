// Package update 把本机已安装的 Craft 应用升到最新发布。
//
// 与 internal/release 的分工：那边回答「最新版本是多少」，每次启动都跑、要缓存、要限流；
// 这边回答「怎么把它拿到并装上去」，只在用户点更新时跑一次 —— 它会下载上百 MB、
// 挂载磁盘镜像、替换 /Applications 里的应用包。两件事的风险差着量级，不该混在一处。
//
// 三条不妥协的规矩，照抄 craft-launcher 的做法：
//  1. 下载地址必须属于该应用自己的 GitHub 发布，重定向也只能落在 GitHub 的域名上；
//  2. 校验和必须对得上（GitHub 的 digest，或发布里的 SHA256SUMS.txt），
//     两处都拿不到校验和就拒绝安装 —— 宁可不装，也不装一个没验过的包；
//  3. 新副本先在暂存位置验明身份（开发者签名、bundle id、版本号），
//     验过了才换掉旧副本，旧副本留成备份。
package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"craft-agent/internal/craft"
	"craft-agent/internal/proxy"
)

const (
	githubAPI  = "https://api.github.com"
	githubHost = "github.com"
	// 单个资产的上限。套件里最大的安装包 90MB 上下，2GB 已经足够宽裕，
	// 但不该对「资产大小」这类远端字段毫无上限。
	maxAssetSize = 2 << 30
	// SHA256SUMS.txt 很小，单独给一个上限，避免被一个畸形条目拖住。
	maxSumsSize = 512 << 10
)

// Asset 是发布里的一个可下载文件。
type Asset struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256,omitempty"`
}

// Release 是一个应用的某次发布，只取安装用得上的字段。
type Release struct {
	Tag     string  `json:"tag"`
	Version string  `json:"version"`
	HTMLURL string  `json:"htmlURL"`
	Assets  []Asset `json:"assets"`
}

// Stage 是一次安装的进度。Step 的取值决定界面显示什么。
type Stage struct {
	Step    string `json:"step"` // download | verify | install | done | manual
	Percent int    `json:"percent"`
	Bytes   int64  `json:"bytes"`
	Total   int64  `json:"total"`
	Note    string `json:"note,omitempty"`
}

// Result 是一次安装的结果。
type Result struct {
	// Version 是装完之后磁盘上那一份的版本。
	Version string `json:"version"`
	// Path 是装到的位置（.app 或下载下来的安装包）。
	Path string `json:"path"`
	// Backup 是旧副本被移去的位置，装坏了可以拖回来。
	Backup string `json:"backup,omitempty"`
	// Manual 为 true 表示本平台只做到「下载并校验」，安装由用户完成。
	Manual bool `json:"manual,omitempty"`
}

// Client 查发布、下载资产。刻意没有缓存到磁盘：更新是一次性动作，
// 缓存只会让「刚发的版本查不到」这种问题更难解释。
type Client struct {
	http  *http.Client
	agent string
	// route 是出网走的那条路的一句话说明，出问题时要能一眼看出走没走代理。
	route string

	// 下载节奏。测试里会改小，正常情况用包级常量。
	attemptLimit time.Duration
	stallLimit   time.Duration
	// retryBase 是第一次重试前等多久，之后翻倍。
	retryBase time.Duration

	mu  sync.Mutex
	rel map[string]Release
}

// 下载的三个时间约束。
//
// 它们的分工：dial/header 管「连不上」，stall 管「连上了但不动」，attempt 管
// 「一直在动但慢得没边」。三种都不该让用户干等，而应该断开、续传、再来。
const (
	dialTimeout   = 20 * time.Second
	headerTimeout = 60 * time.Second
	attemptLimit  = 5 * time.Minute
	stallLimit    = 45 * time.Second
	// 连续这么多次「本次尝试一个字节都没多」才放弃。中间只要有一次下了点东西就重置。
	maxStalls = 8
)

// NewClient 构造客户端。agent 作为 User-Agent —— GitHub 会拒绝没有 UA 的请求。
func NewClient(agent string) *Client {
	if agent == "" {
		agent = "Craft-Agent"
	}
	// 代理在这里定一次：环境变量 → 系统设置 → 直连。图形程序从 Finder 启动时
	// 拿不到 shell 的环境变量，所以系统设置那一步不是可有可无的补充。
	route, note := proxy.Pick()
	log.Printf("出网：%s", note)

	transport := &http.Transport{
		Proxy: route,
		DialContext: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   dialTimeout,
		ResponseHeaderTimeout: headerTimeout,
		ExpectContinueTimeout: time.Second,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
	}

	return &Client{
		http: &http.Client{
			Timeout:   0, // 大文件下载不能用整体超时，靠 ctx 与单次尝试上限控制
			Transport: transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return errors.New("重定向次数过多")
				}
				// 资产会 302 到 objects.githubusercontent.com。只允许 GitHub 自己的域名，
				// 别把一个精心构造的重定向当成合法下载。
				host := strings.ToLower(req.URL.Hostname())
				if host == githubHost || strings.HasSuffix(host, "."+githubHost) || host == "githubusercontent.com" || strings.HasSuffix(host, ".githubusercontent.com") {
					return nil
				}
				return fmt.Errorf("下载被重定向到非 GitHub 域名：%s", host)
			},
		},
		agent:        agent,
		route:        note,
		attemptLimit: attemptLimit,
		stallLimit:   stallLimit,
		retryBase:    time.Second,
		rel:          map[string]Release{},
	}
}

// Route 说明这个客户端出网走哪条路（代理地址或「直连」）。
func (c *Client) Route() string { return c.route }

// Release 取某个应用某个 tag 的发布。tag 为空表示 latest。
func (c *Client) Release(ctx context.Context, repo, tag string) (Release, error) {
	repo = strings.TrimSpace(repo)
	if !validRepo(repo) {
		return Release{}, fmt.Errorf("仓库名不合法: %q", repo)
	}
	key := repo + "@" + tag
	c.mu.Lock()
	if r, ok := c.rel[key]; ok {
		c.mu.Unlock()
		return r, nil
	}
	c.mu.Unlock()

	endpoint := githubAPI + "/repos/" + repo + "/releases/latest"
	if tag = strings.TrimSpace(tag); tag != "" {
		if !validTag(tag) {
			return Release{}, fmt.Errorf("tag 不合法: %q", tag)
		}
		endpoint = githubAPI + "/repos/" + repo + "/releases/tags/" + url.PathEscape(tag)
	}

	body, status, err := c.get(ctx, endpoint, "application/vnd.github+json")
	if err != nil {
		switch status {
		case http.StatusNotFound:
			return Release{}, fmt.Errorf("%s 没有这个发布（%s）", repo, tag)
		case http.StatusForbidden, http.StatusTooManyRequests:
			return Release{}, errors.New("GitHub 限流，稍后再试（可设置 GITHUB_TOKEN）")
		}
		return Release{}, err
	}

	var raw ghRelease
	if err := json.Unmarshal(body, &raw); err != nil {
		return Release{}, fmt.Errorf("发布信息无法解析: %w", err)
	}
	if raw.Tag == "" || raw.Draft {
		return Release{}, errors.New("没有可用的正式发布")
	}
	out := Release{Tag: raw.Tag, Version: strings.TrimPrefix(strings.TrimPrefix(raw.Tag, "v"), "V"), HTMLURL: raw.HTMLURL}
	for _, a := range raw.Assets {
		asset := Asset{Name: a.Name, Size: a.Size, URL: a.URL, SHA256: digestHex(a.Digest)}
		if !validAssetURL(repo, asset.URL) || !safeName(asset.Name) {
			// 不合规的条目直接丢弃：宁可「找不到安装包」，也不要拿一个可疑地址去下载。
			continue
		}
		out.Assets = append(out.Assets, asset)
	}

	c.mu.Lock()
	c.rel[key] = out
	c.mu.Unlock()
	return out, nil
}

// Pick 在发布里挑出适合本机的那一个资产。
func Pick(app craft.App, rel Release) (Asset, error) {
	return pickFor(app, rel, runtime.GOOS, runtime.GOARCH)
}

// pickFor 是 Pick 的平台无关版本，测试因此能覆盖三个平台而不必真的换机器。
func pickFor(app craft.App, rel Release, goos, goarch string) (Asset, error) {
	for _, name := range candidateNames(app, rel.Version, goos, goarch) {
		for _, a := range rel.Assets {
			if strings.EqualFold(a.Name, name) {
				return a, nil
			}
		}
	}
	// 名字对不上（版本号写法变了之类）时按形态挑，仍然排除命令行版与网页版。
	want := preferredSuffixes(goos, goarch)
	for _, suffix := range want {
		suffix = strings.ToLower(suffix)
		for _, a := range rel.Assets {
			if strings.HasSuffix(strings.ToLower(a.Name), suffix) && !isSideArtifact(a.Name) {
				return a, nil
			}
		}
	}
	return Asset{}, fmt.Errorf("%s %s 没有适用于 %s/%s 的安装包", app.Name, rel.Version, goos, goarch)
}

// candidateNames 是「按名字精确命中」的候选，顺序即优先级。
//
// 先按名字找是有原因的：同一个发布里 .zip 有十几个（便携版、命令行版、网页版），
// 按形态挑容易挑错；名字对不上时才退回按键后缀挑。
func candidateNames(app craft.App, version, goos, goarch string) []string {
	id := strings.ToLower(app.ID)
	ver := strings.TrimPrefix(strings.TrimPrefix(version, "v"), "V")
	arch := archName(goos, goarch)
	switch goos {
	case "darwin":
		return []string{
			fmt.Sprintf("%s-%s-macos-universal.dmg", id, ver),
			fmt.Sprintf("%s-%s-macos-%s.dmg", id, ver, arch),
			fmt.Sprintf("%s-%s-macos.dmg", id, ver),
		}
	case "windows":
		return []string{
			fmt.Sprintf("%s-%s-windows-%s.msi", id, ver, arch),
			fmt.Sprintf("%s-%s-windows-%s-portable.zip", id, ver, arch),
		}
	default:
		return []string{
			fmt.Sprintf("%s-%s-linux-%s.AppImage", id, ver, arch),
			fmt.Sprintf("%s-%s-linux-%s.deb", id, ver, arch),
		}
	}
}

func preferredSuffixes(goos, goarch string) []string {
	arch := archName(goos, goarch)
	switch goos {
	case "darwin":
		return []string{"-macos-universal.dmg", "-macos.dmg", "-macos-" + arch + ".dmg", ".dmg"}
	case "windows":
		return []string{"-windows-" + arch + ".msi", "-windows-" + arch + "-portable.zip", ".msi", "-portable.zip"}
	default:
		return []string{"-linux-" + arch + ".AppImage", "-linux-" + arch + ".deb", ".AppImage", ".deb"}
	}
}

// archName 把 Go 的 GOARCH 换成发布文件里用的写法。
//
// 三个平台的写法各不相同，这里必须按平台分：DeckCraft 的发布里同时有
// linux-x86_64.deb、linux-aarch64.AppImage、windows-x64.msi、macos-universal.dmg ——
// 用同一套映射就永远对不上名字。
func archName(goos, goarch string) string {
	switch goos {
	case "windows":
		switch goarch {
		case "amd64":
			return "x64"
		case "386":
			return "x86"
		}
	case "linux":
		switch goarch {
		case "amd64":
			return "x86_64"
		case "arm64":
			return "aarch64"
		}
	case "darwin":
		if goarch == "amd64" {
			return "x86_64"
		}
	}
	return goarch
}

// isSideArtifact 排除同一发布里的命令行版、网页版与校验和文件 ——
// 它们也叫 .zip/.tar.gz，装上去就是个空壳。
func isSideArtifact(name string) bool {
	n := strings.ToLower(name)
	for _, bad := range []string{"-cli-", "-web-", "sha256sums", ".zsync", "-debug", "-symbols"} {
		if strings.Contains(n, bad) {
			return true
		}
	}
	return false
}

// EnsureSum 补齐资产缺失的校验和：从同一发布的 SHA256SUMS.txt 里找。
//
// GitHub 的 digest 字段是最近才有的，老发布只有这个 txt。
func (c *Client) EnsureSum(ctx context.Context, repo string, rel Release, a Asset) (Asset, error) {
	if a.SHA256 != "" {
		return a, nil
	}
	var sums Asset
	for _, candidate := range rel.Assets {
		if strings.EqualFold(candidate.Name, "SHA256SUMS.txt") {
			sums = candidate
			break
		}
	}
	if sums.URL == "" {
		return a, errors.New("发布里没有校验和，拒绝安装未校验的包")
	}
	body, _, err := c.get(ctx, sums.URL, "")
	if err != nil {
		return a, fmt.Errorf("下载校验和失败: %w", err)
	}
	if len(body) > maxSumsSize {
		return a, errors.New("校验和文件异常地大，已放弃")
	}
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		if strings.TrimPrefix(fields[1], "*") == a.Name {
			if h := digestHex("sha256:" + fields[0]); h != "" {
				a.SHA256 = h
				return a, nil
			}
		}
	}
	return a, fmt.Errorf("校验和文件里没有 %s 的条目", a.Name)
}

// Download 把资产下到 dest。
//
// 网络差的时候，能不能「接着上次继续」比「下得多快」重要得多：GitHub 的资产分发
// 在国内经常几十秒就断一次，一次成功的假设并不成立。所以这里按 craft-launcher
// 的做法来 —— 断开就把已下的字节留在 .part 里，下一次请求带上 Range 从断点续，
// 连着几次没有进展才放弃；只有校验不通过时才认输，且会从零重来一次
// （续传的字节可能被中间层拼错）。
func (c *Client) Download(ctx context.Context, a Asset, dest string, onProgress func(bytes, total int64)) error {
	if a.SHA256 == "" {
		return errors.New("缺少校验和，拒绝下载")
	}
	if a.Size > maxAssetSize {
		return fmt.Errorf("安装包过大（%d 字节），已拒绝", a.Size)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}

	// 上一轮已经下完并校验过：不再花一次流量。安装包按版本命名，同名就是同一份。
	if ok, err := fileMatches(dest, a); err == nil && ok {
		if onProgress != nil {
			onProgress(a.Size, a.Size)
		}
		return nil
	}

	err := c.fetch(ctx, a, dest, onProgress, false)
	if errors.Is(err, errChecksum) {
		// 断点续传有可能接错位置，而校验和是唯一能发现它的东西：从头再来一次。
		log.Printf("%s 校验不符，清掉重下一次", a.Name)
		err = c.fetch(ctx, a, dest, onProgress, true)
	}
	return err
}

// errChecksum 表示字节都拿到了，但和发布的 SHA-256 对不上。
var errChecksum = errors.New("校验和不符")

// fatalError 包住「重试也没用」的错误：磁盘写不进去、内容超过上限之类。
type fatalError struct{ err error }

func (e *fatalError) Error() string { return e.err.Error() }
func (e *fatalError) Unwrap() error { return e.err }

func fatal(err error) error { return &fatalError{err: err} }

// fetch 反复尝试直到 .part 的长度等于发布声明的大小，然后校验并改名。
// fresh 为 true 时先丢掉已有的 .part。
func (c *Client) fetch(ctx context.Context, a Asset, dest string, onProgress func(bytes, total int64), fresh bool) error {
	part := dest + ".part"
	if fresh {
		os.Remove(part)
	}
	stalls := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		have := fileSize(part)
		if a.Size > 0 && have > a.Size {
			// 比声明的还大：这份 .part 不属于现在这个版本，丢掉重来。
			os.Remove(part)
			have = 0
		}
		if a.Size > 0 && have == a.Size {
			break
		}

		done, err := c.attempt(ctx, a, part, have, onProgress)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			// 用户取消：.part 留着，下次接着下。
			return ctx.Err()
		}
		var fatalErr *fatalError
		if errors.As(err, &fatalErr) {
			return fatalErr.err
		}
		if done > have {
			stalls = 1
		} else {
			stalls++
		}
		if stalls > maxStalls {
			return fmt.Errorf("连续 %d 次没有进展，已下载 %d 字节（保留在 %s，可重试）：%w", maxStalls, have, part, err)
		}
		wait := c.waitFor(stalls)
		log.Printf("%s 下载中断（%v），%s 后从 %d 字节继续", a.Name, err, wait, have)
		if err := sleep(ctx, wait); err != nil {
			return err
		}
	}

	got, err := hashFile(part)
	if err != nil {
		return err
	}
	if !strings.EqualFold(got, a.SHA256) {
		os.Remove(part)
		return fmt.Errorf("%w：下到 %s…，发布声明 %s…", errChecksum, got[:12], a.SHA256[:12])
	}
	if err := os.Rename(part, dest); err != nil {
		os.Remove(part)
		return err
	}
	if onProgress != nil {
		size := fileSize(dest)
		onProgress(size, size)
	}
	return nil
}

// attempt 发一次请求，把从 have 开始的内容追加到 part。
//
// 返回的 done 是这次结束时 part 的长度 —— 调用方靠它判断「有没有进展」。
// 单次尝试有总时长上限，连接卡住则由 idle 计时器掐断，两种都算可续传的失败。
func (c *Client) attempt(ctx context.Context, a Asset, part string, have int64, onProgress func(bytes, total int64)) (int64, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, c.attemptLimit)
	defer cancel()

	req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, a.URL, nil)
	if err != nil {
		return have, fatal(err)
	}
	req.Header.Set("User-Agent", c.agent)
	// 安装包本来就是压缩过的，别让中间的代理再压一遍。
	req.Header.Set("Accept-Encoding", "identity")
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return have, err
	}
	defer resp.Body.Close()

	var start int64
	switch resp.StatusCode {
	case http.StatusPartialContent:
		start, err = contentRangeStart(resp.Header.Get("Content-Range"))
		if err != nil {
			return have, err
		}
		if start != have {
			// 从别的位置开始追加会把文件拼坏，宁可这次不要。
			return have, fmt.Errorf("服务器从第 %d 字节开始给，请求的是 %d", start, have)
		}
	case http.StatusOK:
		// 服务器不认 Range（或本来就没带）：从零开始，之前的 .part 作废。
		start = 0
	case http.StatusRequestedRangeNotSatisfiable:
		if have > 0 {
			// 要的范围已经越过末尾：长度够了，交给校验去判断对错。
			return have, nil
		}
		return have, fmt.Errorf("下载失败：HTTP %d", resp.StatusCode)
	default:
		return have, fmt.Errorf("下载失败：HTTP %d", resp.StatusCode)
	}

	flags := os.O_CREATE | os.O_WRONLY
	if start > 0 {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return have, fatal(err)
	}
	defer f.Close()

	total := a.Size
	if total <= 0 {
		total = resp.ContentLength
	}

	// 卡住的判断：这么久没有新字节，就掐掉这次请求回去续传 —— Go 的 http 没有
	// 读超时，不加这个，连接僵住就会一直挂着，看上去像「在下载」，其实一个字节都不动。
	idle := time.AfterFunc(c.stallLimit, cancel)
	defer idle.Stop()

	buf := make([]byte, 256<<10)
	written := start
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			idle.Reset(c.stallLimit)
			if written+int64(n) > maxAssetSize {
				return written, fatal(errors.New("下载内容超过大小上限，已中止"))
			}
			if _, err := f.Write(buf[:n]); err != nil {
				return written, fatal(err)
			}
			written += int64(n)
			if onProgress != nil {
				onProgress(written, total)
			}
		}
		if readErr != nil {
			switch {
			case errors.Is(readErr, io.EOF):
				if a.Size > 0 && written != a.Size {
					return written, fmt.Errorf("收到 %d 字节，发布声明 %d", written, a.Size)
				}
				return written, nil
			case ctx.Err() != nil:
				return written, ctx.Err()
			case attemptCtx.Err() != nil:
				return written, fmt.Errorf("这次连接停在 %d 字节（%v）", written, attemptCtx.Err())
			default:
				return written, readErr
			}
		}
	}
}

// contentRangeStart 读 "bytes 1000-1999/2000" 里的 1000。
func contentRangeStart(header string) (int64, error) {
	header = strings.TrimSpace(header)
	if !strings.HasPrefix(header, "bytes ") {
		return 0, fmt.Errorf("分段响应缺少 Content-Range：%q", header)
	}
	rest := strings.TrimPrefix(header, "bytes ")
	first, _, ok := strings.Cut(rest, "-")
	if !ok {
		return 0, fmt.Errorf("Content-Range 读不懂：%q", header)
	}
	start, err := strconv.ParseInt(strings.TrimSpace(first), 10, 64)
	if err != nil || start < 0 {
		return 0, fmt.Errorf("Content-Range 读不懂：%q", header)
	}
	return start, nil
}

// fileSize 返回文件长度，不存在或读不了算 0。
func fileSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

// fileMatches 判断 path 上那份文件是不是这个资产：先比大小，再比哈希。
func fileMatches(path string, a Asset) (bool, error) {
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if a.Size > 0 && st.Size() != a.Size {
		return false, nil
	}
	got, err := hashFile(path)
	if err != nil {
		return false, err
	}
	return strings.EqualFold(got, a.SHA256), nil
}

// hashFile 算整个文件的 SHA-256。
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// waitFor 是第 n 次「没有进展」之后等多久：1s、2s、4s…… 最多 30s。
func (c *Client) waitFor(stalls int) time.Duration {
	base := c.retryBase
	if base <= 0 {
		base = time.Second
	}
	if stalls < 1 {
		stalls = 1
	}
	if stalls > 5 {
		stalls = 5
	}
	d := base << uint(stalls-1)
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

// sleep 等一段时间，期间可以被打断。
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) get(ctx context.Context, target, accept string) ([]byte, int, error) {
	if !strings.HasPrefix(target, "https://") {
		return nil, 0, errors.New("只接受 https 地址")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", c.agent)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return body, resp.StatusCode, err
	}
	if resp.StatusCode != http.StatusOK {
		return body, resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return body, resp.StatusCode, nil
}

type ghRelease struct {
	Tag     string    `json:"tag_name"`
	HTMLURL string    `json:"html_url"`
	Draft   bool      `json:"draft"`
	Assets  []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	URL    string `json:"browser_download_url"`
	Digest string `json:"digest"`
}

// digestHex 把 GitHub 的 "sha256:<hex>" 归一成小写十六进制；不合格返回空串。
func digestHex(digest string) string {
	d := strings.ToLower(strings.TrimSpace(digest))
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) != 64 {
		return ""
	}
	for _, r := range d {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return ""
		}
	}
	return d
}

func validRepo(repo string) bool {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 {
		return false
	}
	for _, p := range parts {
		if p == "" || len(p) > 100 {
			return false
		}
		for _, r := range p {
			ok := r == '-' || r == '_' || r == '.' ||
				(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
			if !ok {
				return false
			}
		}
	}
	return true
}

func validTag(tag string) bool {
	if len(tag) == 0 || len(tag) > 80 {
		return false
	}
	for _, r := range tag {
		ok := r == '.' || r == '-' || r == '_' || r == '+' || r == '~' ||
			(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if !ok {
			return false
		}
	}
	return true
}

// validAssetURL 要求地址是本仓库自己的发布下载，且没有点段与查询串。
func validAssetURL(repo, raw string) bool {
	prefix := "https://" + githubHost + "/" + strings.ToLower(repo) + "/releases/download/"
	if !strings.HasPrefix(strings.ToLower(raw), prefix) {
		return false
	}
	rest := raw[len(prefix):]
	if rest == "" || strings.ContainsAny(rest, "?#\\") {
		return false
	}
	for _, seg := range strings.Split(rest, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

func safeName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 200 {
		return false
	}
	if strings.ContainsAny(name, `/\:*?"<>|`) {
		return false
	}
	for _, r := range name {
		if r < 0x20 {
			return false
		}
	}
	return true
}

// WorkDir 是下载与暂存的落点，放在数据目录下的 cache/updates。
func WorkDir(dataDir string) string {
	return filepath.Join(dataDir, "cache", "updates")
}

// now 单独拎出来，方便测试里替换（这里只用于备份目录命名）。
var now = time.Now

func stage(fn func(Stage), s Stage) {
	if fn != nil {
		fn(s)
	}
}
