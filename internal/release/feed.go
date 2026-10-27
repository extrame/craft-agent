package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"craft-agent/internal/craft"
)

// 远端最新版本的来源。
//
// craft-launcher 的做法值得照抄：先用 storyteller 的聚合 feed，再回落 GitHub。
// 原因是 GitHub 匿名限流只有 60 次/小时，而套件有十几个应用 —— 一开机就把额度烧光，
// 后面的检查全是 403。feed 由服务端轮询 GitHub 后缓存，还顺带给了 assets 与 notes。
const (
	feedURL      = "https://api.storyteller.ai/v1/craft_apps/release_info"
	githubAPI    = "https://api.github.com"
	feedMaxAge   = 6 * time.Hour // 本地缓存有效期：一天查四次足够
	httpTimeout  = 15 * time.Second
	maxNotesSize = 8 << 10
)

// Latest 是一个应用的最新发布。
type Latest struct {
	Tag         string    `json:"tag,omitempty"`
	Version     string    `json:"version,omitempty"`
	Name        string    `json:"name,omitempty"`
	URL         string    `json:"url,omitempty"`
	Notes       string    `json:"notes,omitempty"`
	Prerelease  bool      `json:"prerelease,omitempty"`
	PublishedAt time.Time `json:"publishedAt,omitempty"`
	// Source 记录这条信息从哪来：feed / github / cache。
	Source string `json:"source,omitempty"`
}

// Snapshot 是一次检查的结果。
type Snapshot struct {
	CheckedAt time.Time         `json:"checkedAt"`
	Apps      map[string]Latest `json:"apps"`
	// Errors 是逐个应用失败的原因；部分失败不影响其余结果。
	Errors []string `json:"errors,omitempty"`
	// Source 是整体来源：feed / github / cache / mixed。
	Source string `json:"source,omitempty"`
}

// Checker 负责查最新发布、读已安装版本，并把两者都缓存起来。
type Checker struct {
	dataDir string
	version string
	client  *http.Client

	mu       sync.Mutex
	snapshot Snapshot
	loaded   bool
	// renames 是 feed 公布的仓库改名（old → new，均已小写）。
	// 应用改名会牵动 <Name>.app 的安装路径，这里只跟仓库：发布查不到、
	// 下载地址对不上，都是从「仓库换了名字」开始的。
	renames map[string]string

	installedMu sync.Mutex
	installed   map[string]installedEntry
}

type installedEntry struct {
	value Installed
	at    time.Time
}

// NewChecker 构造检查器。dataDir 用来放缓存文件。
func NewChecker(dataDir, version string) *Checker {
	return &Checker{
		dataDir:   dataDir,
		version:   version,
		client:    &http.Client{Timeout: httpTimeout},
		installed: map[string]installedEntry{},
		renames:   map[string]string{},
	}
}

func (c *Checker) cachePath() string {
	return filepath.Join(c.dataDir, "releases.json")
}

// Installed 返回本机版本，带短缓存：界面每 2 秒来一次，不能每次都读 plist。
func (c *Checker) Installed(app craft.App) Installed {
	c.installedMu.Lock()
	defer c.installedMu.Unlock()
	if e, ok := c.installed[app.ID]; ok && time.Since(e.at) < 3*time.Minute {
		return e.value
	}
	v := Detect(app)
	c.installed[app.ID] = installedEntry{value: v, at: time.Now()}
	return v
}

// ForgetInstalled 丢掉某个应用的版本缓存 —— 刚装完/刚升级时调用。
func (c *Checker) ForgetInstalled(appID string) {
	c.installedMu.Lock()
	delete(c.installed, appID)
	c.installedMu.Unlock()
}

// RepoFor 返回这个应用当前应该问的仓库名（可能被 feed 改过名）。
func (c *Checker) RepoFor(app craft.App) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loadLocked()
	return c.repoForLocked(app.Repo)
}

func (c *Checker) repoForLocked(repo string) string {
	if next, ok := c.renames[strings.ToLower(strings.TrimSpace(repo))]; ok {
		return next
	}
	return repo
}

// Snapshot 返回当前缓存（不会发网络请求）。
func (c *Checker) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loadLocked()
	return c.snapshot
}

func (c *Checker) loadLocked() {
	if c.loaded {
		return
	}
	c.loaded = true
	data, err := os.ReadFile(c.cachePath())
	if err != nil {
		c.snapshot = Snapshot{Apps: map[string]Latest{}}
		return
	}
	var s Snapshot
	if err := json.Unmarshal(data, &s); err != nil || s.Apps == nil {
		c.snapshot = Snapshot{Apps: map[string]Latest{}}
		return
	}
	c.snapshot = s
}

// Check 查询最新发布。force 为 false 时，6 小时内的缓存直接复用。
//
// 单飞：同一时刻只跑一次检查，界面连点「检查更新」不会打出十几个请求。
func (c *Checker) Check(ctx context.Context, apps []craft.App, force bool) (Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loadLocked()

	if !force && !c.snapshot.CheckedAt.IsZero() && time.Since(c.snapshot.CheckedAt) < feedMaxAge {
		out := c.snapshot
		out.Source = "cache"
		return out, nil
	}

	next := Snapshot{Apps: map[string]Latest{}, CheckedAt: time.Now()}
	feed, feedErr := c.fetchFeed(ctx)

	sources := map[string]bool{}
	for _, app := range apps {
		if feed != nil {
			if entry, ok := feed[strings.ToLower(app.ID)]; ok && entry.Version != "" {
				next.Apps[app.ID] = entry
				sources[entry.Source] = true
				continue
			}
		}
		// feed 没覆盖（新应用、超出有效期、feed 整体失败）就走 GitHub。
		// 走之前先把仓库名换成 feed 公布的现名：GitHub 对改名会重定向，
		// 但重定向后的下载地址属于新仓库，按旧仓库名校验就会全部判为非法。
		latest, err := c.fetchGitHub(ctx, c.repoForLocked(app.Repo))
		if err != nil {
			// 失败时保留上一次的结果，并如实记下原因：宁可显示旧版本 + 错误，
			// 也不要清空后让人以为「没有更新」。
			if old, ok := c.snapshot.Apps[app.ID]; ok {
				old.Source = "cache"
				next.Apps[app.ID] = old
			}
			next.Errors = append(next.Errors, fmt.Sprintf("%s: %v", app.ID, err))
			continue
		}
		next.Apps[app.ID] = latest
		sources["github"] = true
	}

	switch {
	case len(sources) == 0:
		next.Source = "cache"
	case len(sources) == 1:
		for k := range sources {
			next.Source = k
		}
	default:
		next.Source = "mixed"
	}
	if feedErr != nil {
		next.Errors = append(next.Errors, "feed: "+feedErr.Error())
	}

	c.snapshot = next
	c.saveLocked(next)
	return next, nil
}

func (c *Checker) saveLocked(s Snapshot) {
	if c.dataDir == "" {
		return
	}
	if err := os.MkdirAll(c.dataDir, 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(c.cachePath(), data, 0o644)
}

func (c *Checker) agent() string {
	if c.version == "" {
		return "Craft-Agent"
	}
	return "Craft-Agent/" + c.version
}

func (c *Checker) getJSON(ctx context.Context, url string, headers map[string]string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", c.agent())
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode != http.StatusOK {
		return body, resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return body, resp.StatusCode, nil
}

type feedDoc struct {
	ReleaseInfo struct {
		Apps        map[string]feedApp `json:"apps"`
		LastFetched string             `json:"last_fetched"`
	} `json:"release_info"`
	RepoRenames []struct {
		Old string `json:"old_name"`
		New string `json:"new_name"`
	} `json:"repo_renames"`
}

type feedApp struct {
	Repo          string       `json:"repo"`
	LatestRelease *feedRelease `json:"latest_release"`
}

type feedRelease struct {
	Tag         string `json:"tag"`
	Version     string `json:"version"`
	Name        string `json:"name"`
	PublishedAt string `json:"published_at"`
	HTMLURL     string `json:"html_url"`
	Prerelease  bool   `json:"prerelease"`
	Notes       string `json:"notes_markdown"`
}

func (c *Checker) fetchFeed(ctx context.Context) (map[string]Latest, error) {
	body, _, err := c.getJSON(ctx, feedURL, nil)
	if err != nil {
		return nil, err
	}
	return c.parseFeed(body)
}

// parseFeed 把 feed 的 JSON 解析成「每个应用的最新发布」，并记下仓库改名。
// 单独拆出来是为了能用真实报文形状做测试，不必真的发一次网络请求。
func (c *Checker) parseFeed(body []byte) (map[string]Latest, error) {
	var doc feedDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("feed 不是合法 JSON: %w", err)
	}
	// 改名只在本账号内跟：feed 不能把我们指到别人的仓库去。
	for _, r := range doc.RepoRenames {
		oldRepo, newRepo := strings.ToLower(strings.TrimSpace(r.Old)), strings.ToLower(strings.TrimSpace(r.New))
		if oldRepo == "" || newRepo == "" || strings.EqualFold(oldRepo, newRepo) {
			continue
		}
		if strings.Split(oldRepo, "/")[0] != strings.Split(newRepo, "/")[0] {
			continue
		}
		c.renames[oldRepo] = newRepo
	}
	out := make(map[string]Latest, len(doc.ReleaseInfo.Apps))
	for id, app := range doc.ReleaseInfo.Apps {
		rel := app.LatestRelease
		if rel == nil || strings.TrimSpace(rel.Tag) == "" {
			continue
		}
		latest := Latest{
			Tag:         rel.Tag,
			Version:     strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(rel.Version), "v"), "V"),
			Name:        rel.Name,
			URL:         rel.HTMLURL,
			Notes:       truncate(rel.Notes, maxNotesSize),
			Prerelease:  rel.Prerelease,
			PublishedAt: parseTime(rel.PublishedAt),
			Source:      "feed",
		}
		if latest.Version == "" {
			if v, ok := Parse(rel.Tag); ok {
				latest.Version = v.String()
			}
		}
		out[strings.ToLower(id)] = latest
	}
	return out, nil
}

type ghRelease struct {
	TagName     string `json:"tag_name"`
	Name        string `json:"name"`
	HTMLURL     string `json:"html_url"`
	PublishedAt string `json:"published_at"`
	Prerelease  bool   `json:"prerelease"`
	Draft       bool   `json:"draft"`
	Body        string `json:"body"`
}

func (c *Checker) fetchGitHub(ctx context.Context, repo string) (Latest, error) {
	repo = strings.TrimSpace(repo)
	if repo == "" || !strings.Contains(repo, "/") {
		return Latest{}, errors.New("没有配置 GitHub 仓库")
	}
	headers := map[string]string{"Accept": "application/vnd.github+json"}
	if token := githubToken(); token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	body, status, err := c.getJSON(ctx, githubAPI+"/repos/"+repo+"/releases/latest", headers)
	if err != nil {
		switch status {
		case http.StatusNotFound:
			return Latest{}, errors.New("仓库还没有发布")
		case http.StatusForbidden, http.StatusTooManyRequests:
			return Latest{}, errors.New("GitHub 限流（可设置 GITHUB_TOKEN 提高额度）")
		}
		return Latest{}, err
	}
	var rel ghRelease
	if err := json.Unmarshal(body, &rel); err != nil {
		return Latest{}, fmt.Errorf("GitHub 返回无法解析: %w", err)
	}
	if rel.Draft || strings.TrimSpace(rel.TagName) == "" {
		return Latest{}, errors.New("没有正式发布")
	}
	latest := Latest{
		Tag:         rel.TagName,
		Name:        rel.Name,
		URL:         rel.HTMLURL,
		Notes:       truncate(rel.Body, maxNotesSize),
		Prerelease:  rel.Prerelease,
		PublishedAt: parseTime(rel.PublishedAt),
		Source:      "github",
	}
	if v, ok := Parse(rel.TagName); ok {
		latest.Version = v.String()
	}
	return latest, nil
}

func githubToken() string {
	for _, key := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// 不要在 UTF-8 中间截断，否则前端会显示乱码。
	cut := s[:n]
	for len(cut) > 0 && cut[len(cut)-1]&0xC0 == 0x80 {
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}
