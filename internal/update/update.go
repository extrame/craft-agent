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
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"craft-agent/internal/craft"
)

const (
	githubAPI    = "https://api.github.com"
	githubHost   = "github.com"
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

	mu  sync.Mutex
	rel map[string]Release
}

// NewClient 构造客户端。agent 作为 User-Agent —— GitHub 会拒绝没有 UA 的请求。
func NewClient(agent string) *Client {
	if agent == "" {
		agent = "Craft-Agent"
	}
	return &Client{
		http: &http.Client{
			Timeout: 0, // 大文件下载不能用整体超时，靠 ctx 控制
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
		agent: agent,
		rel:   map[string]Release{},
	}
}

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

// Download 把资产下到 dest，边下边算 SHA-256，下完对不上就删掉。
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

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", c.agent)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载失败：HTTP %d", resp.StatusCode)
	}

	total := resp.ContentLength
	if total <= 0 {
		total = a.Size
	}
	part := dest + ".part"
	f, err := os.Create(part)
	if err != nil {
		return err
	}
	hasher := sha256.New()
	buf := make([]byte, 256<<10)
	var written int64
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if written+int64(n) > maxAssetSize {
				f.Close()
				os.Remove(part)
				return errors.New("下载内容超过大小上限，已中止")
			}
			if _, err := f.Write(buf[:n]); err != nil {
				f.Close()
				os.Remove(part)
				return err
			}
			hasher.Write(buf[:n])
			written += int64(n)
			if onProgress != nil {
				onProgress(written, total)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			f.Close()
			os.Remove(part)
			return readErr
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(part)
		return err
	}

	got := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(got, a.SHA256) {
		os.Remove(part)
		return fmt.Errorf("校验和不符：下载到 %s…，发布声明 %s…", got[:12], a.SHA256[:12])
	}
	if err := os.Rename(part, dest); err != nil {
		os.Remove(part)
		return err
	}
	return nil
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
