package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"craft-agent/internal/control"
	"craft-agent/internal/craft"
)

// 这个文件是「文件 ↔ 文档身份」的对应关系，agent 端维护。
//
// 协议没有文档 id：file.info 只给 title / path，title 会随保存变（「未命名」→ 文件名），
// 另存为会连 path 一起变，应用重启后 document 序号全部作废。唯一稳定的是 **path** ——
// 文件落盘之后，它就是这份文档在磁盘上的身份。
//
// 所以锚点分两个阶段：
//
//	未保存：身份是 title 指纹，只能防「切文档」，防不了更多；
//	已保存：身份升级为 path，应用重启（活动文档读不到）后，
//	        agent 都能凭 path 把原文件找回来（file.open）继续操作。
//
// 锚点的跟随语义：不管切换来自人还是 AI，只要活动文档变了（读得到新身份），
// 锚点就跟着走 —— 界面上打开的是哪份，AI 操作的就是哪份。
//
// 锚点落盘在 <dataDir>/anchors.json，跨 MCP 会话存活：昨天的会话锚在
// /path/to/a.deck 上，今天新开的会话第一次写入时守卫仍然认得它。

// anchorDoc 是落盘的一份锚点：文档身份 + 记录时间。
type anchorDoc struct {
	docIdentity
	AnchoredAt time.Time `json:"anchoredAt"`
}

// anchorsFile 是 anchors.json 的形状：app id → 锚点。
type anchorsFile map[string]anchorDoc

func anchorFileName(dataDir string) string {
	return filepath.Join(dataDir, "anchors.json")
}

// loadAnchors 读回落盘的锚点。文件不存在、损坏、为空都当「没有」——
// 锚点是意图，不是事实，读不回来就重新锚定，不因它阻塞写入。
func loadAnchors(dataDir string) anchorsFile {
	raw, err := os.ReadFile(anchorFileName(dataDir))
	if err != nil {
		return anchorsFile{}
	}
	var out anchorsFile
	if err := json.Unmarshal(raw, &out); err != nil || out == nil {
		return anchorsFile{}
	}
	return out
}

// save 把锚点写回盘上。调用方持有 docMu。
// 写失败只影响跨会话恢复，不影响本次会话，静默即可（流水里会留痕）。
func (a anchorsFile) save(dataDir string) {
	raw, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return
	}
	path := anchorFileName(dataDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	// 0600：path 会暴露用户在编辑哪个文件，和 registry.json 同级敏感。
	_ = os.WriteFile(path, raw, 0o600)
}

// relation 是锚点与当前文档身份的关系。
type relation string

const (
	relSame     relation = "same"     // 还是锚定的那份
	relSaved    relation = "saved"    // 锚定时未保存，现在有了 path：首次保存，身份升级
	relSwitched relation = "switched" // 当前活动文档是另一份：打开/切换/另存为都是显式意图，锚点跟随
)

// classify 判断锚点与当前身份的关系。纯函数，不碰通道。
//
// 判定只依赖两个问题：「锚点有没有文件身份」「当前 path 是否一致」。
// title 不参与一致判定 —— 保存会把「未命名」变成文件名，title 是最不可靠的字段。
//
// 语义（2026-10 定调）：**打开一个文件之后，所有操作就应该锚到这个文件上。**
// 当前活动文档读得到身份，就是有人显式打开/切换/另存了 —— 锚点跟随，
// 并在结果里注明；绝不把旧文件抢回来顶掉用户眼前的文档。
// 「凭路径找回」只用在活动文档读不到的时候（应用重启、没有文档），
// 那是 guardDoc 的 readDoc 失败分支，不走这里。
func classify(anchor, cur docIdentity) relation {
	if anchor.same(cur) {
		return relSame
	}
	if anchor.Path == "" && cur.Path != "" {
		// 锚定时这份文档还没有落盘身份，出现的任何 path 都只能是它被保存了：
		// 没有第二种合理解释 —— 锚点没身份，谈不上「切到另一份有身份的文档」之后
		// 还写对地方的问题（写入永远落当前文档，锚点跟随即可）。
		return relSaved
	}
	return relSwitched
}

// anchorStore 是 bridge 里的锚点仓库。磁盘是唯一事实：每次访问都重读
// anchors.json —— 文件很小，而这样窗口进程对锚点的管理（解除、查看）
// 对还活着的 MCP 会话立即生效，不会出现「界面删了、会话内存里又写回去」。
type anchorStore struct {
	dataDir string
}

// get 返回某应用的锚点，直接读盘。
func (s *anchorStore) get(app string) (docIdentity, bool) {
	f := loadAnchors(s.dataDir)
	if d, ok := f[app]; ok {
		return d.docIdentity, true
	}
	return docIdentity{}, false
}

// put 记住并落盘：读-改-写，别覆盖别的应用刚写的锚点。
func (s *anchorStore) put(app string, doc docIdentity) {
	f := loadAnchors(s.dataDir)
	if f == nil {
		f = anchorsFile{}
	}
	f[app] = anchorDoc{docIdentity: doc, AnchoredAt: time.Now()}
	f.save(s.dataDir)
}

// drop 解除并落盘（读-改-写）。
func (s *anchorStore) drop(app string) {
	f := loadAnchors(s.dataDir)
	if _, ok := f[app]; !ok {
		return
	}
	delete(f, app)
	f.save(s.dataDir)
}

// reopenDoc 凭路径把锚点文件重新打开，返回打开后的身份。
//
// 「不管软件开关」就靠这一步：应用重启后活动文档是空的或新的，
// 守卫不再直接拒绝，而是先把锚点文件开回来再继续。
// 命令表里没有 file.open 的应用返回明确错误，调用方按 drift 报。
func (b *bridge) reopenDoc(ctx context.Context, app craft.App, c *control.Client, path string) (docIdentity, error) {
	set, ok := b.commandSet(ctx, app, c)
	if ok && !set[cmdFileOpen] {
		return docIdentity{}, fmt.Errorf("该应用没有 file.open，无法凭路径找回文件")
	}
	if _, err := c.Execute(ctx, cmdFileOpen, map[string]any{"path": path}); err != nil {
		return docIdentity{}, err
	}
	return b.readDoc(ctx, c)
}

// guardDoc 是所有写入路径的前置条件：确认还在锚定的那份文档上，不在就锚到当前这份。
//
// 语义（2026-10 定调）：**打开一个文件之后，所有的操作就应该锚到这个文件上。**
//
//	当前活动文档读得到身份且与锚点一致  → 直接放行；
//	读得到身份但变了（打开/切换/另存为）→ 锚点跟随当前文档，note 里说明 ——
//	                                      这是用户或 agent 的显式意图，绝不把旧文件
//	                                      抢回来顶掉眼前的文档；
//	读不到身份（应用重启、没有文档）    → 有锚点 path 就凭路径把原文件开回来再写，
//	                                      这是「不管软件开关」的主路径；找不回才拒绝。
//
// 没有锚点时自动锚定当前文档。
//
// 返回的 notes 是这次守卫自动做过的事（保存升级、锚点跟随、重新打开），
// 调用方要把它放进结果里 —— 模型必须知道锚点被动过了。
func (b *bridge) guardDoc(ctx context.Context, app craft.App, c *control.Client) (docIdentity, []string, error) {
	b.docMu.Lock()
	defer b.docMu.Unlock()

	anchored, hasAnchor := b.anchors.get(app.ID)

	cur, err := b.readDoc(ctx, c)
	if err != nil {
		// 读身份失败。有文件身份可凭就先恢复；没有的话这个错误就是真相。
		if !hasAnchor || anchored.Path == "" {
			return docIdentity{}, nil, fmt.Errorf("读取当前文档失败: %w", err)
		}
		again, rerr := b.reopenDoc(ctx, app, c, anchored.Path)
		if rerr != nil {
			return docIdentity{}, nil, fmt.Errorf(
				"锚定的文件是 %s，但它现在读不到：readDoc: %v；file.open 恢复也失败: %v。\n"+
					"应用可能刚重启且文件已被移动/删除，或应用还没准备好。确认文件存在后重试。",
				anchored.Path, err, rerr)
		}
		if !strings.EqualFold(again.Path, anchored.Path) {
			return docIdentity{}, nil, fmt.Errorf(
				"已重新打开 %s，但读回的路径是 %s，无法确认是同一份文件，命令未发送。",
				anchored.Path, again.Path)
		}
		b.anchors.put(app.ID, again)
		note := fmt.Sprintf("应用重启后文档不在了，已按锚点路径自动重新打开 %s 并继续对准它。", again.Path)
		return again, []string{note}, nil
	}

	if !hasAnchor {
		b.anchors.put(app.ID, cur)
		return cur, nil, nil
	}

	switch classify(anchored, cur) {
	case relSame:
		return cur, nil, nil

	case relSaved:
		b.anchors.put(app.ID, cur)
		note := fmt.Sprintf("文档已保存为 %s：锚点从「%s」升级为文件身份，后续操作继续对准这份文件。", cur.Path, anchored.Title)
		return cur, []string{note}, nil

	default: // relSwitched：当前活动文档是显式意图（打开/切换/另存为），锚点跟随。
		b.anchors.put(app.ID, cur)
		note := fmt.Sprintf("活动文档已变为 %s（打开/切换/另存为）：锚点已跟随，后续操作对准当前这份文档。", cur)
		return cur, []string{note}, nil
	}
}
