import { Events } from "@wailsio/runtime";
import { AppService } from "../bindings/craft-agent/services";
import type { Status } from "../bindings/craft-agent/internal/host/models";
import type {
  Item as MenuItem,
  Menu as MenuModel,
  Node as MenuNode,
  Summary as MenuSummary,
} from "../bindings/craft-agent/internal/menu/models";
import type {
  AnchorView,
  DocumentView,
  InstallResult,
  MCPSessionView,
  UpdateProgress,
  UpdateState,
} from "../bindings/craft-agent/services/models";
import { getLang, initI18n, locale, onLangChange, setLang, t, type Lang } from "./i18n";

// 流水条目的形状与 internal/journal.Entry 的 JSON 标签一一对应。
// 这里手写而不是从 bindings 引：流水是跨进程的稳定契约，前端不该因为它
// 频繁重生成类型而编译失败（bindings 由 `wails3 generate bindings` 产出）。
interface JournalEntry {
  at: string;
  source: string;
  session: string;
  client?: string;
  appId?: string;
  kind: string;
  command?: string;
  ok: boolean;
  detail?: string;
  durationMs?: number;
}

const $ = <T extends HTMLElement>(sel: string): T => document.querySelector(sel) as T;

const els = {
  appList: $("#app-list") as HTMLDivElement,
  appCount: $("#app-count") as HTMLSpanElement,
  connCount: $("#conn-count") as HTMLSpanElement,
  empty: $("#empty") as HTMLElement,
  detail: $("#detail") as HTMLElement,
  mark: $("#detail-mark") as HTMLDivElement,
  name: $("#detail-name") as HTMLHeadingElement,
  pill: $("#detail-pill") as HTMLSpanElement,
  meta: $("#detail-meta") as HTMLParagraphElement,
  actions: $("#detail-actions") as HTMLDivElement,
  statRow: $("#stat-row") as HTMLDivElement,
  docBody: $("#doc-body") as HTMLDivElement,
  menuBody: $("#menu-body") as HTMLDivElement,
  menuNote: $("#menu-note") as HTMLSpanElement,
  menuSearch: $("#menu-search") as HTMLInputElement,
  btnMenuRefresh: $("#btn-menu-refresh") as HTMLButtonElement,
  btnInspect: $("#btn-inspect") as HTMLButtonElement,
  btnUpdates: $("#btn-updates") as HTMLButtonElement,
  sessions: $("#sessions") as HTMLUListElement,
  sessionNote: $("#session-note") as HTMLSpanElement,
  anchorBody: $("#anchor-body") as HTMLDivElement,
  btnAnchorRefresh: $("#btn-anchor-refresh") as HTMLButtonElement,
  journal: $("#journal") as HTMLUListElement,
  filter: $("#journal-filter") as HTMLDivElement,
  langSwitch: $("#lang-switch") as HTMLDivElement,
  modal: $("#modal") as HTMLDivElement,
  mcpRuntime: $("#mcp-runtime") as HTMLDivElement,
  mcpCode: $("#mcp-code") as HTMLPreElement,
  statusDir: $("#status-dir") as HTMLButtonElement,
  updateState: $("#update-state") as HTMLSpanElement,
  statusVersion: $("#status-version") as HTMLSpanElement,
  toast: $("#toast") as HTMLDivElement,
};

let statuses: Status[] = [];
let entries: JournalEntry[] = [];
let selected: string | null = null;
let filter: "all" | "mcp" | "gui" = "all";
let statusKey = "";
let journalKey = "";
let toastTimer: number | undefined;
// MCP 配置与运行状态无关，取一次就缓存，弹窗打开时立即有内容。
let mcpConfig = "";
let inspecting = false;
let updates: UpdateState | null = null;
let checking = false;
// 正在进行的安装。进度到位就替换这个对象并重画操作区，不额外维护一份状态。
let install: UpdateProgress | null = null;
// 本平台能不能代用户完成安装（macOS 可以；其他平台只做到下载并校验）。
let autoInstall = false;
// 菜单：摘要随状态推送，模型只在选中应用时读一次。分开是为了不让 600 条命令
// 跟着两秒一次的快照来回跑。
let menuSummaries: MenuSummary[] = [];
let menuSummaryKey = "";
let menuModel: MenuModel | null = null;
let menuQuery = "";
let menuOpen = new Set<string>();
let menuBusy = false;
let menuLoadToken = 0;
// 锚点：undefined=还没读过，null=这个应用当前没有锚点。
// 只在选中应用与用户动作时读，不跟着 2 秒快照跑——它几乎不变。
let anchor: AnchorView | null | undefined = undefined;
let anchorBusy = false;
// 主程序自己的版本：MCP 会话的版本偏差提示要用。
let mainVersion = "";

/* ---------------- 工具 ---------------- */

function errText(err: unknown): string {
  if (!err) return t("update.unknown");
  if (typeof err === "string") return err;
  if (err instanceof Error) return err.message;
  return String(err);
}

function toast(message: string, isError = false) {
  els.toast.textContent = message;
  els.toast.classList.toggle("is-error", isError);
  els.toast.hidden = false;
  window.clearTimeout(toastTimer);
  toastTimer = window.setTimeout(() => {
    els.toast.hidden = true;
  }, isError ? 6000 : 2400);
}

function clock(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "--:--";
  return d.toLocaleTimeString(locale(), { hour12: false, hour: "2-digit", minute: "2-digit", second: "2-digit" });
}

// 更新时间只显示到分钟：这一栏是给人确认「刚查过」，不需要秒。
function shortTime(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "—";
  return d.toLocaleString(locale(), { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", hour12: false });
}

function statusOf(id: string): Status | undefined {
  return statuses.find((s) => s.app.id === id);
}

function appName(id: string): string {
  return statusOf(id)?.app.name ?? id;
}

function taglineOf(st: Status): string {
  return getLang() === "zh" ? st.app.tagline : st.app.taglineEn || st.app.tagline;
}

// 控制通道的形态是数据，文案在界面这层拼 —— 后端只给 control/port，
// 否则同一个「未确认」要在 Go 里再维护一份英文。
//
// 两种形态不描述细节：端口模式下令牌可能有也可能没有（取决于应用版本），
// 会合文件模式下端口和令牌都是应用当下写给我们的。
function controlText(st: Status): string {
  // bindings 把 Control 生成为 TS enum，直接和字符串比会被判为无交集，所以先取原始值。
  switch (st.app.control as string | undefined) {
    case "port":
      return st.app.port ? t("control.tcp", { port: st.app.port }) : t("control.tcpDynamic");
    case "file":
      return t("control.file");
    default:
      return t("control.unknown");
  }
}

function versionOf(st: Status): string {
  return st.installedVersion || "";
}

/* ---------------- 应用列表 ---------------- */

function renderApps() {
  els.appList.replaceChildren();
  for (const st of statuses) {
    const row = document.createElement("button");
    row.className = "app-row";
    row.style.setProperty("--row-accent", st.app.accent);
    if (st.app.id === selected) row.classList.add("is-active");
    if (!st.installed) row.classList.add("is-missing");
    if (st.updateAvailable) row.classList.add("is-updatable");

    const dot = document.createElement("span");
    dot.className = "app-dot";
    dot.textContent = st.app.name.slice(0, 1);

    const text = document.createElement("span");
    text.className = "app-row-text";
    const name = document.createElement("span");
    name.className = "app-row-name";
    name.textContent = st.app.name;
    const tag = document.createElement("span");
    tag.className = "app-row-tag";
    const ver = versionOf(st);
    if (!st.installed) {
      // 检查过更新之后就知道能装哪个版本，比只写「未安装」有用。
      tag.textContent = st.latestVersion ? t("app.installable", { v: st.latestVersion }) : t("app.missing");
    } else if (st.updateAvailable && st.latestVersion) {
      tag.textContent = `v${ver || "?"} → v${st.latestVersion}`;
      tag.classList.add("is-update");
      row.title = t("app.updateTip", { v: st.latestVersion });
    } else {
      tag.textContent = ver ? `v${ver} · ${taglineOf(st)}` : taglineOf(st);
    }
    text.append(name, tag);

    const state = document.createElement("span");
    state.className = "app-state";
    if (st.connected) state.classList.add("is-live");
    state.title = st.connected
      ? t("app.connected", { addr: st.endpoint.address })
      : st.installed
        ? t("app.installed")
        : t("app.missing");

    row.append(dot, text, state);
    row.addEventListener("click", () => selectApp(st.app.id));
    els.appList.append(row);
  }

  const installed = statuses.filter((s) => s.installed).length;
  const online = statuses.filter((s) => s.connected).length;
  els.appCount.textContent = `${installed}/${statuses.length}`;
  els.connCount.textContent = t("topbar.online", { online, total: statuses.length });
}

/* ---------------- 详情 ---------------- */

function selectApp(id: string) {
  selected = id;
  renderApps();
  renderDetail();
  els.empty.hidden = true;
  els.detail.hidden = false;
  els.docBody.replaceChildren(hintLine(t("doc.loading")));
  // 换应用就换树：展开状态与搜索词都是上一个应用的，带过来只会让人困惑。
  menuModel = null;
  menuQuery = "";
  menuOpen.clear();
  els.menuSearch.value = "";
  renderMenu();
  void loadMenu(id);
  // 锚点跟着应用走：先画加载态，读完再补。
  anchor = undefined;
  renderAnchor();
  void loadAnchor(id);
  void inspect();
}

function renderDetail() {
  const st = selected ? statusOf(selected) : undefined;
  if (!st) return;

  els.mark.textContent = st.app.name.slice(0, 1);
  els.mark.style.setProperty("--mark", st.app.accent);
  els.name.textContent = st.app.name;

  els.pill.className = "pill";
  if (st.updateAvailable && st.latestVersion) {
    els.pill.classList.add("is-warn");
    els.pill.textContent = t("pill.update", { v: st.latestVersion });
  } else if (st.connected) {
    els.pill.classList.add("is-live");
    els.pill.textContent = t("pill.online");
  } else if (st.installed) {
    els.pill.textContent = t("pill.installed");
  } else {
    els.pill.classList.add("is-warn");
    els.pill.textContent = t("pill.missing");
  }

  const meta = [taglineOf(st)];
  const ver = versionOf(st);
  if (st.installed) meta.push(ver ? `v${ver}` : t("version.unknown"));
  if (st.error && !st.connected) meta.push(st.error);
  els.meta.textContent = meta.join(" · ");
  els.meta.title = st.path || "";

  els.actions.replaceChildren();
  // 进度条的颜色跟着应用品牌色走，和左边的圆点对齐。
  els.actions.style.setProperty("--accent", st.app.accent);
  const busy = install !== null && install.appId === st.app.id;
  // 没装但已经知道最新版本号，就能直接装 —— 首次安装和升级走的是同一条路。
  const canInstall = !st.installed && !!st.latestVersion;
  if (busy) {
    els.actions.append(installProgress(install as UpdateProgress));
  } else if (st.updateAvailable && st.latestUrl) {
    els.actions.append(
      actionButton(
        autoInstall ? t("action.update", { v: st.latestVersion ?? "" }) : t("action.downloadUpdate"),
        () => void doInstall(),
        "primary",
      ),
      actionButton(t("action.openRelease"), () => void openRelease()),
    );
  } else if (canInstall) {
    els.actions.append(
      actionButton(
        autoInstall ? t("action.install", { v: st.latestVersion ?? "" }) : t("action.downloadUpdate"),
        () => void doInstall(),
        "primary",
      ),
      actionButton(t("action.openRelease"), () => void openRelease()),
    );
  }
  if (busy) {
    // 安装期间应用会被请退，这时候不放「启动 / 断开」这类动作，
    // 免得用户点到一半被自己打断。
  } else if (!st.installed) {
    if (st.latestUrl && !canInstall) {
      // 连版本号都没有（feed 没覆盖到这个应用）时，只能把发布页交出去。
      els.actions.append(actionButton(t("action.openRelease"), () => void openRelease()));
    }
    if (!canInstall) els.actions.append(actionHint(t("action.installHint")));
  } else if (st.connected) {
    els.actions.append(
      actionButton(t("action.screenshot"), () => void screenshot()),
      actionButton(t("action.disconnect"), () => void disconnect(), "danger"),
    );
  } else {
    els.actions.append(
      actionButton(t("action.launch"), () => void launch(), st.updateAvailable ? "" : "primary"),
      actionButton(t("action.connect"), () => void connect()),
    );
  }

  const versionTitle = st.versionError || st.versionSource || "";
  els.statRow.replaceChildren(
    stat(t("stat.control"), controlText(st)),
    stat(t("stat.endpoint"), st.endpoint.address || "—"),
    stat(t("stat.pid"), st.pid ? `PID ${st.pid}` : "—"),
    stat(t("stat.version"), ver ? `v${ver}` : "—", versionTitle),
    stat(t("stat.latest"), st.latestVersion ? `v${st.latestVersion}` : "—", st.updateError || st.latestNotes?.slice(0, 200) || ""),
  );
}

function stat(label: string, value: string, title = ""): HTMLDivElement {
  const box = document.createElement("div");
  box.className = "stat";
  const l = document.createElement("span");
  l.className = "stat-label";
  l.textContent = label;
  const v = document.createElement("span");
  v.className = "stat-value";
  v.textContent = value;
  v.title = title || value;
  box.append(l, v);
  return box;
}

function actionHint(text: string): HTMLSpanElement {
  const span = document.createElement("span");
  span.className = "hint";
  span.textContent = text;
  return span;
}

function actionButton(label: string, onClick: () => void, variant = ""): HTMLButtonElement {
  const btn = document.createElement("button");
  btn.className = variant ? `btn ${variant}` : "btn";
  btn.textContent = label;
  btn.addEventListener("click", onClick);
  return btn;
}

function hintLine(text: string): HTMLParagraphElement {
  const p = document.createElement("p");
  p.className = "hint";
  p.textContent = text;
  return p;
}

// 安装进度条。下载阶段把体积也写出来：83MB 下到一半时，
// 「还剩多少兆」比「42%」更能让人决定要不要继续等。
function installProgress(p: UpdateProgress): HTMLDivElement {
  const box = document.createElement("div");
  box.className = "install";

  const label = document.createElement("span");
  label.className = "install-label";
  switch (p.step) {
    case "download":
      label.textContent = t("update.downloading", { p: p.percent < 0 ? "…" : String(p.percent) });
      if (p.total > 0) label.textContent += ` · ${mb(p.bytes)} / ${mb(p.total)} MB`;
      break;
    case "verify":
      label.textContent = t("update.verifying");
      break;
    default:
      label.textContent = t("update.installing");
  }

  const track = document.createElement("div");
  track.className = "install-track";
  const fill = document.createElement("div");
  fill.className = "install-fill";
  if (p.percent < 0) {
    // 进度未知的阶段（复制应用包、换文件）不假装有百分比。
    fill.classList.add("is-indeterminate");
  } else {
    fill.style.width = `${Math.min(100, Math.max(2, p.percent))}%`;
  }
  track.append(fill);

  box.append(label, track);
  return box;
}

function mb(bytes: number): string {
  return (bytes / (1024 * 1024)).toFixed(1);
}

/* ---------------- 动作 ---------------- */

async function refresh() {
  try {
    applyStatus((await AppService.Suite()) ?? []);
  } catch (err) {
    toast(errText(err), true);
  }
}

function applyStatus(next: Status[]) {
  const key = JSON.stringify(
    next.map((s) => [
      s.app.id,
      s.installed,
      s.connected,
      s.endpoint.address,
      s.pid,
      s.error,
      s.installedVersion,
      s.latestVersion,
      s.updateAvailable,
    ]),
  );
  if (key === statusKey) return;
  statusKey = key;
  statuses = next;
  renderApps();
  if (selected) renderDetail();
}

async function launch() {
  if (!selected) return;
  const id = selected;
  try {
    await AppService.Launch(id);
    await refresh();
    await inspect();
  } catch (err) {
    toast(errText(err), true);
    await refresh();
  }
}

async function connect() {
  if (!selected) return;
  const id = selected;
  try {
    await AppService.Connect(id);
    await refresh();
    await inspect();
  } catch (err) {
    toast(errText(err), true);
    await refresh();
  }
}

async function disconnect() {
  if (!selected) return;
  try {
    await AppService.Disconnect(selected);
    await refresh();
    els.docBody.replaceChildren(hintLine(t("doc.disconnectedShort")));
  } catch (err) {
    toast(errText(err), true);
  }
}

async function screenshot() {
  if (!selected) return;
  try {
    const path = await AppService.Screenshot(selected);
    toast(t("toast.shotSaved"));
    await AppService.RevealScreenshot(path);
  } catch (err) {
    toast(errText(err), true);
  }
}

/* ---------------- 版本与更新 ---------------- */

async function loadUpdateState() {
  try {
    updates = await AppService.UpdateState();
    renderUpdateState();
  } catch {
    /* 状态栏拿不到不影响使用 */
  }
}

function renderUpdateState() {
  if (!updates || !updates.checkedAt) {
    els.updateState.textContent = t("update.never");
    els.updateState.title = "";
    return;
  }
  const parts = [t("update.checkedAt", { time: shortTime(updates.checkedAt) })];
  if (updates.updates > 0) parts.push(t("update.found", { n: updates.updates }));
  els.updateState.textContent = parts.join(" · ");
  els.updateState.title = updates.errors && updates.errors.length ? updates.errors.join("\n") : "";
}

async function checkUpdates() {
  if (checking) return;
  checking = true;
  els.btnUpdates.disabled = true;
  els.btnUpdates.textContent = t("topbar.checking");
  try {
    updates = await AppService.CheckUpdates(true);
    await refresh();
    renderUpdateState();
    if (updates.errors && updates.errors.length) {
      toast(t("update.partial", { n: updates.updates }), true);
    } else if (updates.updates > 0) {
      toast(t("update.found", { n: updates.updates }));
    } else {
      toast(t("update.none"));
    }
  } catch (err) {
    toast(t("update.failed", { msg: errText(err) }), true);
  } finally {
    checking = false;
    els.btnUpdates.disabled = false;
    // 语言可能在等待期间被切过，这里按当前语言重新取词条。
    els.btnUpdates.textContent = t("topbar.check");
  }
}

async function openRelease() {
  if (!selected) return;
  try {
    await AppService.OpenRelease(selected);
    toast(t("update.opened"));
  } catch (err) {
    toast(errText(err), true);
  }
}

// 点「安装 / 更新」走的路：后端会在同一个调用里下载、校验、替换，进度经事件推回来。
// 这段时间界面不能做别的动作 —— 后端也会拒绝同一个应用的第二次安装。
async function doInstall() {
  if (!selected || install) return;
  const id = selected;
  install = { appId: id, step: "download", percent: 0, bytes: 0, total: 0 };
  renderDetail();
  try {
    const res: InstallResult = await AppService.Install(id);
    if (res.manual) {
      toast(t("update.downloaded"));
    } else if (res.relaunched) {
      toast(t("update.restarted", { v: res.version }));
    } else if (res.fresh) {
      toast(t("update.installedFresh", { v: res.version }));
    } else {
      toast(t("update.installed", { v: res.version }));
    }
    await refresh();
    await loadUpdateState();
  } catch (err) {
    toast(t("update.installFailed", { msg: errText(err) }), true);
    await refresh();
  } finally {
    install = null;
    renderDetail();
  }
}

/* ---------------- 文档状态 ---------------- */

async function inspect() {
  const st = selected ? statusOf(selected) : undefined;
  if (!st) return;
  if (!st.connected) {
    els.docBody.replaceChildren(hintLine(t("doc.disconnected")));
    return;
  }
  if (inspecting) return;
  inspecting = true;
  els.btnInspect.disabled = true;
  try {
    renderDocument(await AppService.Inspect(st.app.id));
  } catch (err) {
    els.docBody.replaceChildren(hintLine(errText(err)));
  } finally {
    inspecting = false;
    els.btnInspect.disabled = false;
  }
}

function renderDocument(doc: DocumentView) {
  els.docBody.replaceChildren();
  // 后端正常都会给一个对象；真拿到空值时给一句人话，
  // 而不是把 "Cannot read properties of null" 摊在界面上。
  if (!doc) {
    els.docBody.append(hintLine(t("doc.noData")));
    return;
  }

  const grid = document.createElement("div");
  grid.className = "doc-grid";
  grid.append(
    metric(t("doc.metric.pages"), String(doc.sections?.length ?? 0)),
    metric(t("doc.metric.objects"), String(doc.objects ?? 0)),
    metric(t("doc.metric.dirty"), doc.dirty ? t("doc.yes") : t("doc.no")),
  );
  els.docBody.append(grid);

  if (doc.sections && doc.sections.length > 0) {
    const chips = document.createElement("div");
    chips.className = "page-chips";
    for (const name of doc.sections.slice(0, 20)) {
      const chip = document.createElement("span");
      chip.className = "chip";
      chip.textContent = name;
      chips.append(chip);
    }
    els.docBody.append(chips);
  }

  if (doc.selection) {
    els.docBody.append(hintLine(t("doc.selection", { v: doc.selection })));
  }

  const file = document.createElement("p");
  file.className = "doc-file";
  file.textContent = doc.path || t("doc.noPath");
  els.docBody.append(file);
}

function metric(label: string, value: string): HTMLDivElement {
  const box = document.createElement("div");
  box.className = "metric";
  const l = document.createElement("span");
  l.className = "metric-label";
  l.textContent = label;
  const v = document.createElement("span");
  v.className = "metric-value";
  v.textContent = value;
  box.append(l, v);
  return box;
}

/* ---------------- 菜单（预加载） ---------------- */

function menuSummaryKeyOf(list: MenuSummary[]): string {
  return JSON.stringify(list.map((s) => [s.app, s.commands, s.sections, s.fetchedAt, s.stale, s.error ?? ""]));
}

// 摘要由后端每 2 秒推一次。变了才重读被选中应用的模型，否则光刷新数字。
function applyMenus(next: MenuSummary[]) {
  const key = menuSummaryKeyOf(next);
  if (key === menuSummaryKey) return;
  menuSummaryKey = key;
  menuSummaries = next;
  if (selected) void loadMenu(selected);
  renderMenu();
}

async function loadMenu(id: string) {
  const token = ++menuLoadToken;
  const sum = menuSummaries.find((s) => s.app === id);
  if (!sum || sum.commands === 0) {
    if (token === menuLoadToken) {
      menuModel = null;
      renderMenu();
    }
    return;
  }
  try {
    const model = await AppService.Menu(id);
    if (token !== menuLoadToken) return;
    menuModel = model && model.commands > 0 ? model : null;
  } catch (err) {
    if (token !== menuLoadToken) return;
    menuModel = null;
    toast(errText(err), true);
  }
  renderMenu();
}

// 「重新抓取」是唯一会碰应用的入口：应用没开着时后端会启动它。
async function refreshMenu() {
  if (!selected || menuBusy) return;
  const id = selected;
  menuBusy = true;
  renderMenu();
  try {
    menuModel = await AppService.RefreshMenu(id);
    menuSummaries = (await AppService.Menus()) ?? [];
    menuSummaryKey = menuSummaryKeyOf(menuSummaries);
    menuOpen.clear();
    toast(t("menu.refreshed", { n: menuModel.commands }));
  } catch (err) {
    toast(t("menu.failed", { msg: errText(err) }), true);
  } finally {
    menuBusy = false;
    if (selected === id) renderMenu();
  }
}

function renderMenu() {
  const st = selected ? statusOf(selected) : undefined;
  // 展开/收起会整棵树重画，滚动位置得自己留着。
  const scrollTop = els.menuBody.scrollTop;
  els.menuBody.replaceChildren();
  els.menuNote.textContent = "";
  els.menuNote.classList.remove("is-warn", "menu-note");
  els.menuNote.classList.add("menu-note");
  els.btnMenuRefresh.disabled = !st || menuBusy;
  if (!st) return;

  const sum = menuSummaries.find((s) => s.app === st.app.id);
  if (sum && sum.commands > 0) {
    const parts = [t("menu.counts", { n: sum.commands, s: sum.sections })];
    if (sum.grouped > 0) parts.push(t("menu.groupedNote", { n: sum.grouped }));
    parts.push(t("menu.fetchedAt", { time: shortTime(sum.fetchedAt) }));
    if (sum.stale) {
      els.menuNote.classList.add("is-warn");
      parts.push(t("menu.stale"));
    }
    els.menuNote.textContent = parts.join(" · ");
    if (sum.error) els.menuNote.title = sum.error;
  }

  if (menuBusy) {
    els.menuBody.append(hintLine(t("menu.fetching")));
    return;
  }

  if (!menuModel || !menuModel.commands) {
    const box = document.createElement("div");
    box.className = "menu-empty";
    const text = document.createElement("span");
    text.textContent = sum?.error ? t("menu.error", { msg: sum.error }) : t("menu.empty");
    const btn = actionButton(t("menu.preload"), () => void refreshMenu(), "primary");
    btn.classList.add("mini");
    box.append(text, btn);
    els.menuBody.append(box);
    return;
  }

  if (menuQuery.trim()) {
    renderMenuHits(menuModel, menuQuery);
  } else {
    const tree = document.createElement("div");
    tree.className = "menu-tree";
    for (const section of menuModel.sections ?? []) {
      if (section) tree.append(menuNodeEl(section, 0));
    }
    els.menuBody.append(tree);
  }
  els.menuBody.scrollTop = scrollTop;
}

function menuNodeEl(node: MenuNode, depth: number): HTMLElement {
  const path = (node.path ?? []).join("/");
  const items = node.items ?? [];
  const children = (node.children ?? []).filter((c): c is MenuNode => !!c);
  const hasBody = items.length > 0 || children.length > 0;
  const open = menuOpen.has(path);

  const wrap = document.createElement("div");
  const head = document.createElement("button");
  head.className = open ? "menu-node is-open" : "menu-node";
  head.style.paddingLeft = `${10 + depth * 16}px`;

  const caret = document.createElement("span");
  caret.className = "menu-caret";
  caret.textContent = hasBody ? "▶" : "";
  const name = document.createElement("span");
  name.className = "menu-label";
  name.textContent = node.name || t("menu.unfiled");
  head.append(caret, name);

  if (node.grouped) {
    // 这一层不是应用给的菜单，是按命令 id 前缀补的 —— 界面上要说清楚。
    const badge = document.createElement("span");
    badge.className = "menu-badge";
    badge.textContent = t("menu.prefixBadge");
    head.append(badge);
  }

  const count = document.createElement("span");
  count.className = "menu-count";
  count.textContent = String(node.count);
  head.append(count);

  if (hasBody) {
    head.title = path;
    head.addEventListener("click", () => {
      if (menuOpen.has(path)) menuOpen.delete(path);
      else menuOpen.add(path);
      renderMenu();
    });
  } else {
    head.disabled = true;
  }
  wrap.append(head);

  if (open) {
    for (const item of items) wrap.append(menuItemEl(item, depth + 1));
    for (const child of children) wrap.append(menuNodeEl(child, depth + 1));
  }
  return wrap;
}

function menuItemEl(item: MenuItem, depth: number): HTMLElement {
  const row = document.createElement("div");
  row.className = "menu-item";
  row.style.paddingLeft = `${10 + depth * 16}px`;

  const label = document.createElement("span");
  label.className = "menu-item-label";
  label.textContent = item.label || item.id;
  const id = document.createElement("span");
  id.className = "menu-item-id";
  id.textContent = item.id;
  const key = document.createElement("span");
  key.className = "menu-item-key";
  key.textContent = item.shortcut ?? "";

  const tip = [item.id];
  if (item.params) tip.push(item.params);
  if (item.aliases && item.aliases.length) tip.push(`${t("menu.aliases")}: ${item.aliases.join(", ")}`);
  row.title = tip.join("\n");
  row.append(label, id, key);
  return row;
}

function menuItemMatches(item: MenuItem, q: string): boolean {
  const hay = [item.id, item.label, item.shortcut ?? "", ...(item.path ?? []), ...(item.aliases ?? [])];
  return hay.some((s) => s.toLowerCase().includes(q));
}

function renderMenuHits(model: MenuModel, query: string) {
  const q = query.trim().toLowerCase();
  const hits: MenuItem[] = [];
  const visit = (nodes: (MenuNode | null)[] | null | undefined) => {
    for (const n of nodes ?? []) {
      if (!n) continue;
      for (const item of n.items ?? []) if (menuItemMatches(item, q)) hits.push(item);
      visit(n.children);
    }
  };
  visit(model.sections);

  if (hits.length === 0) {
    els.menuBody.append(hintLine(t("menu.noMatch", { q: query.trim() })));
    return;
  }
  const box = document.createElement("div");
  for (const item of hits.slice(0, 120)) {
    const row = document.createElement("div");
    row.className = "menu-hit";
    const main = document.createElement("div");
    main.className = "menu-hit-main";
    const title = document.createElement("div");
    title.className = "menu-hit-title";
    const label = document.createElement("span");
    label.className = "menu-item-label";
    label.textContent = item.label || item.id;
    const id = document.createElement("span");
    id.className = "menu-item-id";
    id.textContent = item.id;
    title.append(label, id);
    const crumb = document.createElement("div");
    crumb.className = "menu-crumb";
    crumb.textContent = (item.path ?? []).join(" › ");
    main.append(title, crumb);
    const key = document.createElement("span");
    key.className = "menu-item-key";
    key.textContent = item.shortcut ?? "";
    row.append(main, key);
    box.append(row);
  }
  if (hits.length > 120) box.append(hintLine(t("menu.moreHits", { n: hits.length - 120 })));
  els.menuBody.append(box);
}

/* ---------------- AI 会话（从流水推导） ---------------- */

function renderSessions() {
  const open = new Map<string, { start: string; last: string; actions: number; apps: Set<string> }>();
  for (const e of entries.slice().reverse()) {
    if (e.source !== "mcp" || !e.session) continue;
    if (e.kind === "session-start") {
      open.set(e.session, { start: e.at, last: e.at, actions: 0, apps: new Set() });
      continue;
    }
    if (e.kind === "session-end") {
      open.delete(e.session);
      continue;
    }
    const s = open.get(e.session);
    if (!s) continue;
    s.actions += 1;
    s.last = e.at;
    if (e.appId) s.apps.add(e.appId);
  }

  els.sessions.replaceChildren();
  els.sessionNote.textContent = t("sessions.note", { n: open.size });

  if (open.size === 0) {
    const li = document.createElement("li");
    li.className = "s-empty";
    li.textContent = t("sessions.empty");
    els.sessions.append(li);
    return;
  }

  for (const [id, s] of open) {
    const li = document.createElement("li");

    const dot = document.createElement("span");
    dot.className = "s-dot";

    const main = document.createElement("div");
    main.className = "s-main";
    const title = document.createElement("b");
    title.textContent = id;
    const sub = document.createElement("span");
    const apps = Array.from(s.apps).map(appName).join(" · ");
    sub.textContent = t("sessions.actions", { n: s.actions }) + (apps ? ` · ${apps}` : "");
    main.append(title, sub);

    const last = document.createElement("time");
    last.textContent = clock(s.last);

    li.append(dot, main, last);
    els.sessions.append(li);
  }
}

/* ---------------- 操作流 ---------------- */

function applyJournal(next: JournalEntry[]) {
  const key = `${next.length}|${next[0]?.at ?? ""}|${next[0]?.command ?? ""}`;
  if (key === journalKey) return;
  journalKey = key;
  entries = next;
  renderJournal();
  renderSessions();
}

function renderJournal() {
  const rows = entries.filter((e) => filter === "all" || e.source === filter);
  els.journal.replaceChildren();

  if (rows.length === 0) {
    const li = document.createElement("li");
    li.className = "j-empty";
    li.textContent = t("journal.empty");
    els.journal.append(li);
    return;
  }

  for (const e of rows.slice(0, 200)) {
    const li = document.createElement("li");
    li.className = "j-row";
    if (!e.ok) li.classList.add("is-error");

    const time = document.createElement("span");
    time.className = "j-time";
    time.textContent = clock(e.at);

    const src = document.createElement("span");
    src.className = `j-src is-${e.source}`;
    src.textContent = e.source === "mcp" ? t("journal.sourceMcp") : t("journal.sourceGui");

    const app = document.createElement("span");
    app.className = "j-app";
    app.textContent = e.appId ? appName(e.appId) : "—";

    const act = document.createElement("span");
    act.className = "j-act";
    act.textContent = labelOf(e);

    const detail = document.createElement("span");
    detail.className = "j-detail";
    detail.textContent = e.detail ?? "";
    detail.title = e.detail ?? "";

    const dur = document.createElement("span");
    dur.className = "j-dur";
    dur.textContent = e.durationMs ? `${e.durationMs}ms` : "";

    li.append(time, src, app, act, detail, dur);
    els.journal.append(li);
  }
}

/* ---------------- 文件锚点 ---------------- */

// 锚点在选中应用与用户动作时读；此外保存感知（后端轮询 file.info 升级锚点）
// 会让它每隔几秒变化一次，5 秒轮询里静默对齐一次 —— 不走 busy，避免闪加载态。
async function loadAnchor(id: string, quiet = false) {
  if (!quiet) {
    anchorBusy = true;
    renderAnchor();
  }
  try {
    const all = (await AppService.Anchors()) ?? [];
    anchor = all.find((a) => a.app === id) ?? null;
  } catch {
    if (!quiet) anchor = null;
  }
  if (!quiet) anchorBusy = false;
  // 请求期间用户可能已切到别的应用，别画到别人的面板里。
  if (selected === id) renderAnchor();
}

async function releaseAnchor() {
  if (!selected || !anchor) return;
  const id = selected;
  anchorBusy = true;
  renderAnchor();
  try {
    await AppService.ReleaseAnchor(id);
    anchor = null;
    toast(t("anchor.released"));
  } catch (err) {
    toast(t("anchor.releaseFail", { msg: errText(err) }), true);
  }
  anchorBusy = false;
  renderAnchor();
}

function anchorRow(label: string, value: string, mono = false): HTMLDivElement {
  const row = document.createElement("div");
  row.className = "anchor-row";
  const l = document.createElement("span");
  l.className = "anchor-label";
  l.textContent = label;
  const v = document.createElement("span");
  v.className = mono ? "anchor-value is-mono" : "anchor-value";
  v.textContent = value;
  v.title = value;
  row.append(l, v);
  return row;
}

function renderAnchor() {
  const body = els.anchorBody;
  body.replaceChildren();
  if (!selected || anchor === undefined || anchorBusy) {
    // 首次读取或正在解除时都是加载态：动作很快，不值得做局部禁用。
    body.append(hintLine(t("anchor.loading")));
    return;
  }
  if (!anchor) {
    body.append(hintLine(t("anchor.none")));
    return;
  }
  const a = anchor;
  const box = document.createElement("div");
  box.className = "anchor-doc";

  const head = document.createElement("div");
  head.className = "anchor-head";
  const badge = document.createElement("span");
  badge.className = "menu-badge" + (a.saved ? " is-live" : "");
  badge.textContent = a.saved ? t("anchor.saved") : t("anchor.unsaved");
  const when = document.createElement("span");
  when.className = "anchor-when";
  when.textContent = `${t("anchor.anchoredAt")} ${a.anchoredAt}`;
  head.append(badge, when);
  box.append(head);

  box.append(anchorRow(t("anchor.title"), a.title));
  if (a.saved) {
    box.append(anchorRow(t("anchor.path"), a.path, true));
    const exist = document.createElement("p");
    exist.className = "anchor-exist" + (a.fileExists ? "" : " is-warn");
    exist.textContent = a.fileExists ? t("anchor.fileExists") : t("anchor.fileMissing");
    box.append(exist);
  }
  box.append(hintLine(t("anchor.hint")));

  const rel = document.createElement("button");
  rel.className = "btn mini";
  rel.textContent = t("anchor.release");
  rel.addEventListener("click", () => void releaseAnchor());
  box.append(rel);
  body.append(box);
}

function labelOf(e: JournalEntry): string {
  switch (e.kind) {
    case "session-start":
      return t("journal.sessionStart");
    case "session-end":
      return t("journal.sessionEnd");
    case "launch":
      return t("journal.launch");
    case "connect":
      return t("journal.connect");
    case "disconnect":
      return t("journal.disconnect");
    case "update":
      // 同一种 kind 里，「查」和「装」是两件事，流水里分开写。
      return e.command === "install" ? t("journal.install") : t("journal.update");
    case "menu":
      return t("journal.menu");
    case "anchor":
      return t("journal.anchor");
    case "command":
      return e.command || t("journal.command");
    default:
      return e.kind;
  }
}

/* ---------------- 弹层 ---------------- */

async function openMcp() {
  // 先把弹窗亮出来，再去取内容：取配置失败时用户能看到失败原因，而不是一个空框。
  els.modal.hidden = false;
  if (mcpConfig) {
    els.mcpCode.textContent = mcpConfig;
  } else {
    els.mcpCode.textContent = t("modal.reading");
  }
  renderMcpRuntime(null);
  void loadMcpRuntime();
  if (!mcpConfig) {
    try {
      mcpConfig = await AppService.MCPConfig();
      els.mcpCode.textContent = mcpConfig;
    } catch (err) {
      els.mcpCode.textContent = t("modal.readFail", { msg: errText(err) });
    }
  }
}

// MCP 执行端是 AI 客户端拉起的独立进程，主程序升级后旧会话还挂着旧版本。
// 这块就是把「谁连着、版本对不对」摆到用户眼前——升级后重启 AI 客户端，
// 提示就消失了。null 表示还在读。
async function loadMcpRuntime() {
  let views: MCPSessionView[] | null;
  try {
    views = await AppService.MCPRuntime();
  } catch {
    views = null;
  }
  renderMcpRuntime(views ?? []);
}

function renderMcpRuntime(views: MCPSessionView[] | null) {
  const box = els.mcpRuntime;
  box.replaceChildren();
  const head = document.createElement("div");
  head.className = "mcp-runtime-head";
  head.textContent = t("mcp.runtime");
  box.append(head);

  if (views === null) {
    const p = document.createElement("p");
    p.className = "hint";
    p.textContent = t("anchor.loading");
    box.append(p);
    return;
  }
  if (views.length === 0) {
    const p = document.createElement("p");
    p.className = "hint";
    p.textContent = t("mcp.none");
    box.append(p);
    return;
  }
  for (const v of views) {
    const row = document.createElement("div");
    row.className = "mcp-runtime-row" + (v.alive ? "" : " is-dead");

    const name = document.createElement("span");
    name.className = "mcp-runtime-session";
    name.textContent = v.session;
    const ver = document.createElement("span");
    ver.className = "mcp-runtime-version";
    ver.textContent = v.version ? `v${v.version}` : "—";
    const state = document.createElement("span");
    state.className = "mcp-runtime-state" + (v.outdated && v.alive ? " is-warn" : "");
    state.textContent = v.alive ? t("mcp.alive") : t("mcp.dead");
    row.append(name, ver, state);
    box.append(row);

    if (v.outdated && v.alive) {
      const warn = document.createElement("p");
      warn.className = "mcp-runtime-warn";
      warn.textContent = t("mcp.outdated", { v: v.version, cur: mainVersion });
      box.append(warn);
    }
  }
}

async function copyConfig() {
  try {
    await navigator.clipboard.writeText(els.mcpCode.textContent ?? "");
    toast(t("toast.copied"));
  } catch {
    toast(t("toast.copyFail"), true);
  }
}

/* ---------------- 语言 ---------------- */

function syncLangSwitch() {
  const lang = getLang();
  for (const btn of Array.from(els.langSwitch.querySelectorAll("button"))) {
    btn.classList.toggle("is-on", btn.dataset.lang === lang);
  }
}

function switchLang(lang: Lang) {
  setLang(lang);
  syncLangSwitch();
  // 静态文案已由 applyStatic 刷过；动态的这几块要重画一遍。
  renderApps();
  if (selected) renderDetail();
  renderSessions();
  renderJournal();
  renderMenu();
  renderAnchor();
  renderUpdateState();
  if (!els.modal.hidden && !mcpConfig) {
    els.mcpCode.textContent = t("modal.reading");
  }
}

/* ---------------- 装配 ---------------- */

function bind() {
  els.btnInspect.addEventListener("click", () => void inspect());
  els.btnMenuRefresh.addEventListener("click", () => void refreshMenu());
  els.btnAnchorRefresh.addEventListener("click", () => {
    if (selected) void loadAnchor(selected);
  });
  els.menuSearch.addEventListener("input", () => {
    menuQuery = els.menuSearch.value;
    renderMenu();
  });
  els.btnUpdates.addEventListener("click", () => void checkUpdates());
  $("#btn-mcp").addEventListener("click", () => void openMcp());
  $("#btn-modal-close").addEventListener("click", () => {
    els.modal.hidden = true;
  });
  els.modal.addEventListener("click", (ev) => {
    if (ev.target === els.modal) els.modal.hidden = true;
  });
  $("#btn-copy").addEventListener("click", () => void copyConfig());
  $("#btn-data-dir").addEventListener("click", () => void AppService.OpenDataDir());
  els.statusDir.addEventListener("click", () => void AppService.OpenDataDir());

  els.langSwitch.addEventListener("click", (ev) => {
    const btn = (ev.target as HTMLElement).closest("button");
    const lang = btn?.dataset.lang;
    if (lang === "zh" || lang === "en") switchLang(lang);
  });

  els.filter.addEventListener("click", (ev) => {
    const btn = (ev.target as HTMLElement).closest("button");
    if (!btn?.dataset.filter) return;
    filter = btn.dataset.filter as typeof filter;
    for (const b of Array.from(els.filter.querySelectorAll("button"))) {
      b.classList.toggle("is-on", b === btn);
    }
    renderJournal();
  });

  // 后端每 2 秒推一次全量快照；这里只做接收，不自己再轮询。
  Events.On("craft:status", (ev) => {
    const data = ev.data as Status[] | undefined;
    if (Array.isArray(data)) applyStatus(data);
  });
  Events.On("craft:menus", (ev) => {
    const data = ev.data as MenuSummary[] | undefined;
    if (Array.isArray(data)) applyMenus(data);
  });
  Events.On("craft:journal", (ev) => {
    const data = ev.data as JournalEntry[] | undefined;
    if (Array.isArray(data)) applyJournal(data);
  });
  // 单条活动用于即时反馈（不等下一次快照）。
  Events.On("craft:activity", () => {
    void refresh();
  });
  // 安装进度：后端在下载/校验/替换的每个阶段推一条。
  Events.On("craft:update", (ev) => {
    const p = ev.data as UpdateProgress | undefined;
    if (!p) return;
    if (p.step === "error") {
      // 失败由 doInstall 的 catch 报文案，这里只把进度条收掉。
      install = null;
      if (selected === p.appId) renderDetail();
      return;
    }
    install = p;
    if (selected === p.appId) renderDetail();
  });

  // 切语言时把静态标注与动态块都刷新（setLang 里已经调过 applyStatic，这里只补重绘）。
  onLangChange(() => {
    void AppService.SetTrayLanguage(getLang());
    renderApps();
    if (selected) renderDetail();
    renderSessions();
    renderJournal();
    renderMenu();
    renderUpdateState();
  });
}

async function boot() {
  initI18n();
  // 托盘菜单在系统 chrome 里，跟随不了页面语言，开机时先把当前语言报过去。
  void AppService.SetTrayLanguage(getLang());
  bind();
  syncLangSwitch();
  try {
    els.statusDir.textContent = await AppService.DataDir();
    els.statusVersion.textContent = `v${await AppService.Version()}`;
    mainVersion = await AppService.Version();
    autoInstall = await AppService.CanAutoInstall();
  } catch {
    /* 状态栏拿不到不影响使用 */
  }
  // 流水与状态都由后端每 2 秒推一次；这里只准备空态。
  applyJournal([]);
  await refresh();
  try {
    menuSummaries = (await AppService.Menus()) ?? [];
    menuSummaryKey = menuSummaryKeyOf(menuSummaries);
  } catch {
    /* 菜单摘要是辅助信息，拿不到不影响使用 */
  }
  await loadUpdateState();
  // 后端启动时已经在后台查过一次；这里隔一会儿再读一次缓存，把结果显示到状态栏。
  window.setTimeout(() => void loadUpdateState(), 4000);
  window.setInterval(() => void loadUpdateState(), 15000);
  // 配置在启动时就取好，避免点开弹窗还要等一次进程往返。
  void AppService.MCPConfig()
    .then((cfg) => {
      mcpConfig = cfg;
    })
    .catch(() => {
      /* 打开弹窗时会再试一次并显示失败原因 */
    });
  // 选中应用的文档每 5 秒对齐一次，保持「看到的就是当下」；锚点一并静默刷新，
  // 用户在应用里保存文件后几秒内，「文件锚点」面板就会从「未保存」变成文件身份。
  window.setInterval(() => {
    if (selected) {
      void inspect();
      void loadAnchor(selected, true);
    }
  }, 5000);
}

void boot();
