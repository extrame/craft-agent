// 零依赖 i18n：词条 + 静态标注 + 语言持久化。
//
// 为什么不引 i18next / vue-i18n：这个界面只有一个页面、一套模板，
// 需要的只是「按 key 取词条 + 把 data-i18n 标一遍 + 记住选择」。
// 引一个库要多几十 KB 和一层抽象，收益为零。
//
// 约定：
//   data-i18n="key"        → textContent 换成词条
//   data-i18n-html="key"   → innerHTML（仅用于词条里带 <code> 的少量文案，内容由本文件维护）
//   data-i18n-title="key"  → title 属性
// 动态文案在渲染函数里用 t(key, vars)。

export type Lang = "zh" | "en";

type Dict = Record<string, string>;

const zh: Dict = {
  "app.sub": "Craft 套件的 MCP 中枢",
  "topbar.online": "{online}/{total} 在线",
  "topbar.mcp": "MCP 配置",
  "topbar.check": "检查更新",
  "topbar.checking": "检查中…",
  "sidebar.apps": "应用",
  "sidebar.openData": "打开数据目录",

  "empty.title": "选择一个应用看状态",
  "empty.body":
    "左边列的是本机 Craft 套件。启动应用后，它会带着控制通道打开，状态和操作流会实时出现在这里；第三方 AI 通过 MCP 调用同一批命令。",
  "empty.step1": "<b>启动 / 连接</b> —— 应用带 <code>--control</code> 起来，端点写进注册表",
  "empty.step2": "<b>保持更新</b> —— 状态每 2 秒对齐一次，AI 的动作立即出现在流水里",
  "empty.step3": "<b>统一出口</b> —— 所有编辑能力只通过 MCP 暴露，界面不放编辑器",

  "pill.online": "在线",
  "pill.installed": "已安装",
  "pill.missing": "未安装",
  "pill.update": "可更新 {v}",
  "pill.prerelease": "预发布",

  "panel.document": "文档状态",
  "panel.refresh": "刷新",
  "panel.menu": "菜单",
  "panel.sessions": "AI 会话",
  "panel.anchor": "文件锚点",
  "panel.journal": "会话操作流",
  "anchor.loading": "加载中…",
  "anchor.none": "未锚定。AI 第一次对这个应用执行写入时，会自动锚定它的当前文档。",
  "anchor.saved": "已保存为文件",
  "anchor.unsaved": "未保存",
  "anchor.title": "文档",
  "anchor.path": "文件",
  "anchor.anchoredAt": "锚定于",
  "anchor.fileExists": "文件在磁盘上，应用重启也能凭路径找回。",
  "anchor.fileMissing": "文件已不在磁盘上，写入会被守卫拒绝。",
  "anchor.hint": "AI 的写入始终落在锚定的这份文档上；解除后下一次写入会就地重新锚定。",
  "anchor.release": "解除锚点",
  "anchor.released": "已解除锚点",
  "anchor.releaseFail": "解除失败：{msg}",
  "filter.all": "全部",
  "filter.mcp": "AI",
  "filter.gui": "本机",

  "stat.control": "控制通道",
  "stat.endpoint": "端点",
  "stat.pid": "进程",
  "stat.version": "版本",
  "stat.latest": "最新",

  "action.launch": "启动",
  "action.connect": "连接已运行",
  "action.disconnect": "断开",
  "action.screenshot": "截图",
  "action.install": "安装 v{v}",
  "action.installHint": "还没有这个应用的版本信息，先点右上角「检查更新」",
  "action.update": "更新到 {v}",
  "action.downloadUpdate": "下载更新包",
  "action.openRelease": "打开发布页",
  "action.checkUpdates": "检查更新",

  "app.missing": "未安装",
  "app.installable": "可安装 v{v}",
  "app.installed": "已安装，未连接",
  "app.connected": "已连接 {addr}",
  "app.updateTip": "可更新到 {v}",

  "doc.loading": "读取中…",
  "doc.disconnected": "未连接。启动或连接后这里会显示当前文档。",
  "doc.disconnectedShort": "已断开。",
  "doc.metric.pages": "页 / 段",
  "doc.metric.objects": "对象",
  "doc.metric.dirty": "未保存",
  "doc.yes": "是",
  "doc.no": "否",
  "doc.selection": "当前选中：{v}",
  "doc.noPath": "（未保存到文件）",
  "doc.noData": "这个应用没有回报文档结构。",

  "menu.counts": "{n} 条命令 · {s} 个菜单",
  "menu.groupedNote": "{n} 条按前缀归组",
  "menu.fetchedAt": "{time} 抓取",
  "menu.stale": "应用已升级，建议重新抓取",
  "menu.search": "搜索命令、快捷键…",
  "menu.refresh": "重新抓取",
  "menu.fetching": "正在向应用要命令表…",
  "menu.empty": "这个应用还没有预加载菜单。抓一次需要应用在运行。",
  "menu.preload": "预加载菜单",
  "menu.refreshed": "已抓取 {n} 条命令",
  "menu.failed": "抓取菜单失败：{msg}",
  "menu.error": "菜单读取失败：{msg}",
  "menu.noMatch": "没有匹配「{q}」的命令",
  "menu.moreHits": "还有 {n} 条未显示，换个更具体的词",
  "menu.unfiled": "未归类",
  "menu.prefixBadge": "按前缀",
  "menu.aliases": "别名",

  "sessions.note": "{n} 在线",
  "sessions.empty": "还没有第三方 AI 连上来。用右上角「MCP 配置」把它接进来。",
  "sessions.actions": "{n} 个动作",

  "journal.empty": "暂无记录。启动应用或让 AI 调用一次工具就会出现在这里。",
  "journal.sessionStart": "连接上来",
  "journal.sessionEnd": "断开",
  "journal.launch": "启动",
  "journal.connect": "已连接",
  "journal.disconnect": "已断开",
  "journal.menu": "预加载菜单",
  "journal.anchor": "锚点",
  "journal.command": "执行命令",
  "journal.update": "检查更新",
  "journal.install": "安装更新",
  "journal.sourceMcp": "AI",
  "journal.sourceGui": "本机",

  "modal.title": "接入第三方 AI 工具",
  "modal.close": "关闭",
  "modal.copy":
    "把下面这段放进客户端的 <code>mcp.json</code>（Cursor、Claude Desktop 等）。接上之后，AI 通过 <code>craft_*</code> 工具操作本机所有 Craft 应用，每次调用都会出现在「会话操作流」里。",
  "modal.hint": "保存后重启客户端生效。",
  "modal.copyBtn": "复制配置",
  "modal.reading": "读取配置…",
  "modal.readFail": "读取配置失败：{msg}",
  "toast.copied": "已复制",
  "toast.copyFail": "复制失败，请手动选中",
  "toast.shotSaved": "已保存截图",

  "update.never": "还没检查过更新",
  "update.checkedAt": "更新检查于 {time}",
  "update.found": "{n} 个应用可更新",
  "update.none": "所有应用都是最新",
  "update.partial": "{n} 个应用可更新（部分查询失败）",
  "update.failed": "检查更新失败：{msg}",
  "update.opened": "已在浏览器打开发布页",
  "update.downloading": "下载中 {p}%",
  "update.verifying": "校验安装包…",
  "update.installing": "正在安装…",
  "update.installed": "已更新到 v{v}",
  "update.installedFresh": "已安装 v{v}",
  "update.restarted": "已更新到 v{v}，应用已重新启动",
  "update.downloaded": "已下载并校验，安装包已交给系统",
  "update.installFailed": "更新失败：{msg}",
  "update.unknown": "—",

  "control.tcpDynamic": "TCP（端口由宿主分配）",
  "control.tcp": "TCP 127.0.0.1:{port}",
  "control.file": "会合文件（应用自选端口）",
  "control.unknown": "未确认",
  "version.unknown": "版本未知",
  "version.source.bundle": "应用包",
  "version.source.msi": "安装程序",
  "version.source.cli": "命令行",
};

const en: Dict = {
  "app.sub": "MCP hub for the Craft suite",
  "topbar.online": "{online}/{total} online",
  "topbar.mcp": "MCP setup",
  "topbar.check": "Check updates",
  "topbar.checking": "Checking…",
  "sidebar.apps": "Apps",
  "sidebar.openData": "Open data folder",

  "empty.title": "Pick an app to see its status",
  "empty.body":
    "The list on the left is the Craft suite installed on this machine. Once an app is running with its control channel, its state and activity show up here; third-party AI drives the same commands over MCP.",
  "empty.step1": "<b>Launch / connect</b> — the app starts with <code>--control</code>, its endpoint lands in the registry",
  "empty.step2": "<b>Always current</b> — status realigns every 2 seconds, AI actions appear immediately",
  "empty.step3": "<b>One way in</b> — every editing capability goes through MCP; the window is not an editor",

  "pill.online": "Online",
  "pill.installed": "Installed",
  "pill.missing": "Not installed",
  "pill.update": "Update {v}",
  "pill.prerelease": "pre-release",

  "panel.document": "Document",
  "panel.refresh": "Refresh",
  "panel.menu": "Menu",
  "panel.sessions": "AI sessions",
  "panel.anchor": "File anchor",
  "panel.journal": "Activity",
  "anchor.loading": "Loading…",
  "anchor.none": "No anchor yet. The first AI write to this app anchors its active document.",
  "anchor.saved": "Saved to file",
  "anchor.unsaved": "Unsaved",
  "anchor.title": "Document",
  "anchor.path": "File",
  "anchor.anchoredAt": "anchored",
  "anchor.fileExists": "File is on disk; the guard can re-open it by path after an app restart.",
  "anchor.fileMissing": "File is gone from disk; writes will be refused.",
  "anchor.hint": "AI writes always land on the anchored document; releasing it makes the next write re-anchor in place.",
  "anchor.release": "Release anchor",
  "anchor.released": "Anchor released",
  "anchor.releaseFail": "Release failed: {msg}",
  "filter.all": "All",
  "filter.mcp": "AI",
  "filter.gui": "Local",

  "stat.control": "Control",
  "stat.endpoint": "Endpoint",
  "stat.pid": "Process",
  "stat.version": "Version",
  "stat.latest": "Latest",

  "action.launch": "Launch",
  "action.connect": "Connect running",
  "action.disconnect": "Disconnect",
  "action.screenshot": "Screenshot",
  "action.install": "Install v{v}",
  "action.installHint": "No version info for this app yet — run Check updates first",
  "action.update": "Update to {v}",
  "action.downloadUpdate": "Download update",
  "action.openRelease": "Open releases",
  "action.checkUpdates": "Check updates",

  "app.missing": "Not installed",
  "app.installable": "Can install v{v}",
  "app.installed": "Installed, not connected",
  "app.connected": "Connected {addr}",
  "app.updateTip": "Update available: {v}",

  "doc.loading": "Reading…",
  "doc.disconnected": "Not connected. Launch or connect to read the current document.",
  "doc.disconnectedShort": "Disconnected.",
  "doc.metric.pages": "Pages",
  "doc.metric.objects": "Objects",
  "doc.metric.dirty": "Unsaved",
  "doc.yes": "Yes",
  "doc.no": "No",
  "doc.selection": "Selected: {v}",
  "doc.noPath": "(not saved to a file)",
  "doc.noData": "This app did not report a document.",

  "menu.counts": "{n} commands · {s} sections",
  "menu.groupedNote": "{n} grouped by prefix",
  "menu.fetchedAt": "fetched {time}",
  "menu.stale": "app was upgraded — fetch again",
  "menu.search": "Search commands, shortcuts…",
  "menu.refresh": "Fetch again",
  "menu.fetching": "Asking the app for its command table…",
  "menu.empty": "No menu preloaded for this app yet. Fetching needs the app running.",
  "menu.preload": "Preload menu",
  "menu.refreshed": "Fetched {n} commands",
  "menu.failed": "Could not fetch the menu: {msg}",
  "menu.error": "Menu unavailable: {msg}",
  "menu.noMatch": "Nothing matches “{q}”",
  "menu.moreHits": "{n} more not shown — narrow the search",
  "menu.unfiled": "Unfiled",
  "menu.prefixBadge": "prefix",
  "menu.aliases": "alias",

  "sessions.note": "{n} online",
  "sessions.empty": "No third-party AI connected yet. Use “MCP setup” at the top right to plug one in.",
  "sessions.actions": "{n} actions",

  "journal.empty": "Nothing yet. Launch an app, or let the AI call one tool.",
  "journal.sessionStart": "Connected",
  "journal.sessionEnd": "Disconnected",
  "journal.launch": "Launch",
  "journal.connect": "Connected",
  "journal.disconnect": "Disconnected",
  "journal.menu": "Preload menu",
  "journal.anchor": "Anchor",
  "journal.command": "Command",
  "journal.update": "Update check",
  "journal.install": "Install update",
  "journal.sourceMcp": "AI",
  "journal.sourceGui": "Local",

  "modal.title": "Connect a third-party AI",
  "modal.close": "Close",
  "modal.copy":
    "Paste this into your client's <code>mcp.json</code> (Cursor, Claude Desktop, …). Once connected, the AI drives every local Craft app through <code>craft_*</code> tools, and each call shows up in Activity.",
  "modal.hint": "Restart the client after saving.",
  "modal.copyBtn": "Copy config",
  "modal.reading": "Reading config…",
  "modal.readFail": "Could not read the config: {msg}",
  "toast.copied": "Copied",
  "toast.copyFail": "Copy failed — select the text manually",
  "toast.shotSaved": "Screenshot saved",

  "update.never": "Updates not checked yet",
  "update.checkedAt": "Checked {time}",
  "update.found": "{n} app(s) can update",
  "update.none": "Everything is up to date",
  "update.partial": "{n} app(s) can update (some lookups failed)",
  "update.failed": "Update check failed: {msg}",
  "update.opened": "Release page opened in your browser",
  "update.downloading": "Downloading {p}%",
  "update.verifying": "Verifying the package…",
  "update.installing": "Installing…",
  "update.installed": "Updated to v{v}",
  "update.installedFresh": "Installed v{v}",
  "update.restarted": "Updated to v{v}; the app is running again",
  "update.downloaded": "Downloaded and verified — the installer is opening",
  "update.installFailed": "Update failed: {msg}",
  "update.unknown": "—",

  "control.tcpDynamic": "TCP (port assigned by host)",
  "control.tcp": "TCP 127.0.0.1:{port}",
  "control.file": "Rendezvous file (app picks the port)",
  "control.unknown": "Not confirmed",
  "version.unknown": "Version unknown",
  "version.source.bundle": "App bundle",
  "version.source.msi": "Installer",
  "version.source.cli": "CLI",
};

const dicts: Record<Lang, Dict> = { zh, en };
const STORAGE_KEY = "craft-agent.lang";

function detect(): Lang {
  try {
    const saved = localStorage.getItem(STORAGE_KEY);
    if (saved === "zh" || saved === "en") return saved;
  } catch {
    /* 隐私模式下 localStorage 可能抛错，按系统语言走 */
  }
  const nav = (navigator.language || "zh").toLowerCase();
  return nav.startsWith("zh") ? "zh" : "en";
}

let current: Lang = detect();
const listeners: Array<() => void> = [];

export function getLang(): Lang {
  return current;
}

export function locale(): string {
  return current === "zh" ? "zh-CN" : "en-US";
}

export function t(key: string, vars?: Record<string, string | number>): string {
  const raw = dicts[current][key] ?? zh[key] ?? key;
  if (!vars) return raw;
  return raw.replace(/\{(\w+)\}/g, (_, name: string) => String(vars[name] ?? ""));
}

export function onLangChange(fn: () => void): void {
  listeners.push(fn);
}

export function setLang(next: Lang): void {
  if (next === current) return;
  current = next;
  try {
    localStorage.setItem(STORAGE_KEY, next);
  } catch {
    /* 记不住就记不住，本次会话内仍然生效 */
  }
  document.documentElement.lang = next === "zh" ? "zh-CN" : "en";
  applyStatic();
  for (const fn of listeners) fn();
}

// 把带 data-i18n 标注的静态文案刷一遍。切语言和启动时各调一次。
export function applyStatic(root: ParentNode = document): void {
  root.querySelectorAll<HTMLElement>("[data-i18n]").forEach((el) => {
    const key = el.dataset.i18n;
    if (key) el.textContent = t(key);
  });
  root.querySelectorAll<HTMLElement>("[data-i18n-html]").forEach((el) => {
    const key = el.dataset.i18nHtml;
    if (key) el.innerHTML = t(key);
  });
  root.querySelectorAll<HTMLElement>("[data-i18n-title]").forEach((el) => {
    const key = el.dataset.i18nTitle;
    if (key) el.title = t(key);
  });
  root.querySelectorAll<HTMLElement>("[data-i18n-placeholder]").forEach((el) => {
    const key = el.dataset.i18nPlaceholder;
    if (key) el.setAttribute("placeholder", t(key));
  });
}

export function initI18n(): void {
  document.documentElement.lang = current === "zh" ? "zh-CN" : "en";
  applyStatic();
}
