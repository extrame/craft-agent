// Package menu 把每个应用自己的命令表压成同一种菜单模型。
//
// 为什么需要这一层：同一套引擎，几代应用对「命令在菜单里的位置」有四种写法
// （数组路径 / 单段带斜杠 / 单个顶层名 / 干脆不写），agent 与界面不该各写一套兼容。
// 这里把它们归一成一棵树，并且**只保留观察到的信息** —— 应用没给菜单的命令
// 按 id 前缀单独归组并标出来，而不是编一个像模像样的菜单名。
package menu

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"craft-agent/internal/control"
	"craft-agent/internal/craft"
)

// Item 是菜单里的一条命令。字段与 control.Command 一一对应，
// 但 Path 已经是拆好的层级（"Text/Adjust" ↦ Text › Adjust）。
type Item struct {
	ID       string   `json:"id"`
	Label    string   `json:"label"`
	Shortcut string   `json:"shortcut,omitempty"`
	Params   string   `json:"params,omitempty"`
	Aliases  []string `json:"aliases,omitempty"`
	Path     []string `json:"path,omitempty"`
	Group    string   `json:"group"`
	Enabled  *bool    `json:"enabled,omitempty"`
	Undoable bool     `json:"undoable,omitempty"`
}

// Node 是菜单树的一个节点。根节点的子节点就是顶层菜单（File / Edit / …）。
type Node struct {
	Name string `json:"name"`
	Path []string `json:"path"`
	// Grouped 表示这一层不是应用给的菜单，而是按命令 id 前缀补出来的兜底分组。
	Grouped  bool    `json:"grouped,omitempty"`
	Count    int     `json:"count"` // 整个子树里有多少条命令
	Items    []Item  `json:"items,omitempty"`
	Children []*Node `json:"children,omitempty"`

	index map[string]*Node
}

// Group 是按命令 id 前缀统计出来的一行，和菜单树是两个视角：
// 菜单树回答「用户在哪儿点得到」，分组回答「这个应用有哪些能力域」。
type Group struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// Menu 是一个应用的完整菜单模型，也是落盘的那份文档。
type Menu struct {
	App       string    `json:"app"`
	AppName   string    `json:"name,omitempty"`
	Version   string    `json:"version,omitempty"`
	FetchedAt time.Time `json:"fetchedAt"`
	Commands  int       `json:"commands"`
	Mapped    int       `json:"mapped"`  // 应用给了菜单位置的条数
	Grouped   int       `json:"grouped"` // 落到 id 前缀分组里的条数
	Sections  []*Node   `json:"sections"`
	Groups    []Group   `json:"groups"`
}

// ItemCount 是树里所有条目的总数（等于 Commands，除非文件被改坏了）。
func (m Menu) ItemCount() int {
	n := 0
	for _, s := range m.Sections {
		n += s.Count
	}
	return n
}

// Build 把一张命令表变成菜单模型。
//
// 顺序保留应用自己的顺序（命令表就是按菜单顺序发的），
// 只有「前缀兜底」的节点被挪到真实菜单之后。
func Build(app craft.App, version string, cmds []control.Command) Menu {
	m := Menu{
		App:       app.ID,
		AppName:   app.Name,
		Version:   version,
		FetchedAt: time.Now().UTC(),
		Commands:  len(cmds),
	}
	root := newNode("")
	groups := map[string]int{}
	var groupOrder []string

	for _, c := range cmds {
		path := c.Menu.Path()
		item := Item{
			ID:       c.ID,
			Label:    c.Label,
			Shortcut: c.Shortcut,
			Params:   c.Params,
			Aliases:  c.Aliases,
			Group:    c.Group(),
			Enabled:  c.Enabled,
			Undoable: c.Undoable,
		}
		grouped := false
		if len(path) == 0 {
			// 应用没给位置：用 id 前缀当兜底分组，并标出来。
			grouped = true
			m.Grouped++
			path = []string{c.Group()}
		} else {
			m.Mapped++
		}
		item.Path = path

		node := root
		for _, name := range path {
			node = node.child(name)
		}
		if grouped {
			node.Grouped = true
		}
		node.Items = append(node.Items, item)
		node.Count++

		if groups[item.Group] == 0 {
			groupOrder = append(groupOrder, item.Group)
		}
		groups[item.Group]++
	}

	// 子树计数：每个节点记自己这一层 + 所有后代。
	tally(root)
	m.Sections = sortSections(root.Children)
	for _, name := range groupOrder {
		m.Groups = append(m.Groups, Group{Name: name, Count: groups[name]})
	}
	return m
}

func newNode(name string) *Node {
	return &Node{Name: name, Path: nil, index: map[string]*Node{}}
}

// child 按名字取子节点，没有就建一个；子节点挂在父节点的 slice 上保持顺序。
func (n *Node) child(name string) *Node {
	if child, ok := n.index[name]; ok {
		return child
	}
	if n.index == nil {
		n.index = map[string]*Node{}
	}
	child := newNode(name)
	child.Path = append(append([]string{}, n.Path...), name)
	n.index[name] = child
	n.Children = append(n.Children, child)
	return child
}

// tally 自底向上累加。叶子节点的 Count 已经在插入时数过自己的条数。
func tally(n *Node) int {
	total := len(n.Items)
	for _, c := range n.Children {
		total += tally(c)
	}
	n.Count = total
	return total
}

// sortSections 把真实菜单排在前面、前缀兜底排后面，各自保持应用给的顺序。
func sortSections(nodes []*Node) []*Node {
	if len(nodes) < 2 {
		return nodes
	}
	out := make([]*Node, 0, len(nodes))
	for _, pass := range []bool{false, true} {
		for _, n := range nodes {
			if n.Grouped == pass {
				out = append(out, n)
			}
		}
	}
	return out
}

// Walk 深度优先遍历整棵树，回调拿到节点和深度。
func (m Menu) Walk(fn func(n *Node, depth int) bool) {
	var visit func(n *Node, depth int) bool
	visit = func(n *Node, depth int) bool {
		if !fn(n, depth) {
			return false
		}
		for _, c := range n.Children {
			if !visit(c, depth+1) {
				return false
			}
		}
		return true
	}
	for _, s := range m.Sections {
		if !visit(s, 0) {
			return
		}
	}
}

// Find 按路径找节点，路径大小写不敏感（模型更可能写 "file" 而不是 "File"）。
func (m Menu) Find(path ...string) (*Node, bool) {
	if len(path) == 0 {
		return nil, false
	}
	nodes := m.Sections
	var found *Node
	for _, want := range path {
		found = nil
		for _, n := range nodes {
			if strings.EqualFold(n.Name, want) {
				found = n
				break
			}
		}
		if found == nil {
			return nil, false
		}
		nodes = found.Children
	}
	return found, true
}

// Search 在整棵树里按子串匹配命令 id / 标签 / 菜单路径 / 别名。
func (m Menu) Search(query string, limit int) []Item {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return nil
	}
	if limit <= 0 {
		limit = 40
	}
	var out []Item
	m.Walk(func(n *Node, _ int) bool {
		for _, it := range n.Items {
			if itemMatches(it, q) {
				out = append(out, it)
				if len(out) >= limit {
					return false
				}
			}
		}
		return true
	})
	return out
}

func itemMatches(it Item, q string) bool {
	if strings.Contains(strings.ToLower(it.ID), q) ||
		strings.Contains(strings.ToLower(it.Label), q) ||
		strings.Contains(strings.ToLower(it.Shortcut), q) {
		return true
	}
	for _, seg := range it.Path {
		if strings.Contains(strings.ToLower(seg), q) {
			return true
		}
	}
	for _, a := range it.Aliases {
		if strings.Contains(strings.ToLower(a), q) {
			return true
		}
	}
	return false
}

/* ---------------- 预加载存储 ---------------- */

// Store 是「预加载」的落点：一份内存缓存 + 每个应用一个 JSON 文件。
//
// 为什么要落盘：界面要在连接应用之前就能把菜单画出来，MCP 进程也要能在没有
// GUI 的情况下拿到同一份模型。菜单只随应用版本变，几 KB 到几百 KB，
// 每次连接都重新拉一遍是浪费，也是每次切应用都要等一秒的原因。
type Store struct {
	dir string

	mu      sync.Mutex
	entries map[string]*entry
}

type entry struct {
	menu     Menu
	commands []control.Command
	at       time.Time
	err      string
}

// NewStore 构造存储。目录是 <dataDir>/menus。
func NewStore(dataDir string) *Store {
	return &Store{
		dir:     filepath.Join(dataDir, "menus"),
		entries: map[string]*entry{},
	}
}

// Dir 是菜单文件所在目录（界面会显示它）。
func (s *Store) Dir() string { return s.dir }

// Load 读回上次落盘的菜单。启动时调一次；解析不了的文件直接跳过 ——
// 它们是缓存，不是用户数据。
func (s *Store) Load() error {
	items, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, it := range items {
		if it.IsDir() || !strings.HasSuffix(it.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.dir, it.Name()))
		if err != nil {
			continue
		}
		var m Menu
		if err := json.Unmarshal(raw, &m); err != nil || m.App == "" {
			continue
		}
		s.mu.Lock()
		s.entries[m.App] = &entry{menu: m, at: m.FetchedAt}
		s.mu.Unlock()
	}
	return nil
}

// Get 返回缓存里的菜单（不发起任何调用）。
func (s *Store) Get(appID string) (Menu, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[appID]
	if !ok {
		return Menu{}, false
	}
	return e.menu, true
}

// LastError 返回这个应用最近一次抓取失败的原因。
func (s *Store) LastError(appID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[appID]; ok {
		return e.err
	}
	return ""
}

// All 按套件顺序返回所有已缓存的菜单。
func (s *Store) All() []Menu {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Menu, 0, len(s.entries))
	for _, app := range craft.Suite {
		if e, ok := s.entries[app.ID]; ok {
			out = append(out, e.menu)
		}
	}
	return out
}

// Fetcher 是「能拿到命令表」的东西。生产里是 *control.Client，
// 测试里换成假的 —— 菜单模型的正确性不该依赖真起一个应用。
type Fetcher interface {
	Commands(ctx context.Context) ([]control.Command, error)
}

// Commands 返回命令表：缓存够新就复用，否则重新拉。菜单模型与命令表一起返回，
// 调用方不用连着请求两次。
func (s *Store) Commands(ctx context.Context, f Fetcher, app craft.App, version string, maxAge time.Duration) ([]control.Command, Menu, error) {
	s.mu.Lock()
	if e, ok := s.entries[app.ID]; ok && e.commands != nil && time.Since(e.at) < maxAge && e.menu.Version == version {
		cmds, m := e.commands, e.menu
		s.mu.Unlock()
		return cmds, m, nil
	}
	s.mu.Unlock()
	m, err := s.Refresh(ctx, f, app, version)
	if err != nil {
		return nil, Menu{}, err
	}
	s.mu.Lock()
	cmds := s.entries[app.ID].commands
	s.mu.Unlock()
	return cmds, m, nil
}

// Refresh 向应用要一次完整命令表，重建成模型并落盘。
func (s *Store) Refresh(ctx context.Context, f Fetcher, app craft.App, version string) (Menu, error) {
	cmds, err := f.Commands(ctx)
	if err != nil {
		s.noteError(app.ID, err)
		return Menu{}, err
	}
	m := Build(app, version, cmds)
	s.mu.Lock()
	s.entries[app.ID] = &entry{menu: m, commands: cmds, at: time.Now()}
	s.mu.Unlock()
	if err := s.save(m); err != nil {
		// 写不进缓存不等于这次抓取失败：这一份已经在内存里，界面和 agent 都能用。
		// 把原因记在摘要上，下次启动会重抓。
		s.noteError(app.ID, fmt.Errorf("菜单已抓取，但没能写入缓存: %w", err))
	}
	return m, nil
}

func (s *Store) noteError(appID string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[appID]
	if !ok {
		e = &entry{}
		s.entries[appID] = e
	}
	e.err = err.Error()
}

// Forget 丢掉一个应用的缓存（应用被替换或版本变了）。
func (s *Store) Forget(appID string) {
	s.mu.Lock()
	delete(s.entries, appID)
	s.mu.Unlock()
	_ = os.Remove(filepath.Join(s.dir, appID+".json"))
}

func (s *Store) save(m Menu) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir, m.App+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return replaceFile(tmp, path)
}

// replaceFile 先改名，不行就退化成「整份重写」。
//
// 这台机器上 rename 偶发 EPERM（下载落盘的 .part → .dmg、装应用时的临时文件
// 都碰过同一类瞬时限制）。缓存文件不值得为它整份作废：改名三次不成，
// 直接把内容写到目标文件。
func replaceFile(tmp, path string) error {
	var err error
	for i := 0; i < 3; i++ {
		if err = os.Rename(tmp, path); err == nil {
			return nil
		}
		time.Sleep(time.Duration(50*(i+1)) * time.Millisecond)
	}
	raw, rerr := os.ReadFile(tmp)
	if rerr != nil {
		return err
	}
	if werr := os.WriteFile(path, raw, 0o644); werr != nil {
		return err
	}
	_ = os.Remove(tmp)
	return nil
}

// Summary 是界面和 MCP 列表用的轻量摘要：不带条目，只有规模与时间。
type Summary struct {
	App       string    `json:"app"`
	Name      string    `json:"name"`
	Version   string    `json:"version,omitempty"`
	FetchedAt time.Time `json:"fetchedAt"`
	Commands  int       `json:"commands"`
	Mapped    int       `json:"mapped"`
	Grouped   int       `json:"grouped"`
	Sections  int       `json:"sections"`
	Stale     bool      `json:"stale"`
	Error     string    `json:"error,omitempty"`
}

// Summaries 汇总所有已缓存的菜单。stale 由调用方给出当前安装的版本决定 ——
// 换了应用版本就该重新预加载，这件事只有服务层知道。
func (s *Store) Summaries(installedVersion func(craft.App) string) []Summary {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Summary, 0, len(craft.Suite))
	for _, app := range craft.Suite {
		e, ok := s.entries[app.ID]
		if !ok || e.menu.App == "" {
			continue
		}
		sum := Summary{
			App:       e.menu.App,
			Name:      e.menu.AppName,
			Version:   e.menu.Version,
			FetchedAt: e.menu.FetchedAt,
			Commands:  e.menu.Commands,
			Mapped:    e.menu.Mapped,
			Grouped:   e.menu.Grouped,
			Sections:  len(e.menu.Sections),
			Error:     e.err,
		}
		if installedVersion != nil {
			if cur := installedVersion(app); cur != "" && e.menu.Version != "" && cur != e.menu.Version {
				sum.Stale = true
			}
		}
		out = append(out, sum)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].App < out[j].App })
	return out
}
