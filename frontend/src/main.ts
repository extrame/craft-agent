import { Events } from "@wailsio/runtime";
import { AppService } from "../bindings/craft-agent/services";
import type { Status } from "../bindings/craft-agent/internal/host/models";
import type { DocumentView, InstallResult, UpdateProgress, UpdateState } from "../bindings/craft-agent/services/models";
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
  btnInspect: $("#btn-inspect") as HTMLButtonElement,
  btnUpdates: $("#btn-updates") as HTMLButtonElement,
  sessions: $("#sessions") as HTMLUListElement,
  sessionNote: $("#session-note") as HTMLSpanElement,
  journal: $("#journal") as HTMLUListElement,
  filter: $("#journal-filter") as HTMLDivElement,
  langSwitch: $("#lang-switch") as HTMLDivElement,
  modal: $("#modal") as HTMLDivElement,
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

// 控制通道的形态是数据，文案在界面这层拼 —— 后端只给 transport/port，
// 否则同一个「未确认」要在 Go 里再维护一份英文。
function controlText(st: Status): string {
  // bindings 把 Transport 生成为 TS enum，直接和字符串比会被判为无交集，所以先取原始值。
  switch (st.app.transport as string | undefined) {
    case "tcp":
      return st.app.defaultPort ? t("control.tcp", { port: st.app.defaultPort }) : t("control.tcpDynamic");
    case "unix":
      return t("control.unix");
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
    return;
  }
  els.mcpCode.textContent = t("modal.reading");
  try {
    mcpConfig = await AppService.MCPConfig();
    els.mcpCode.textContent = mcpConfig;
  } catch (err) {
    els.mcpCode.textContent = t("modal.readFail", { msg: errText(err) });
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
  renderUpdateState();
  if (!els.modal.hidden && !mcpConfig) {
    els.mcpCode.textContent = t("modal.reading");
  }
}

/* ---------------- 装配 ---------------- */

function bind() {
  els.btnInspect.addEventListener("click", () => void inspect());
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
    renderApps();
    if (selected) renderDetail();
    renderSessions();
    renderJournal();
    renderUpdateState();
  });
}

async function boot() {
  initI18n();
  bind();
  syncLangSwitch();
  try {
    els.statusDir.textContent = await AppService.DataDir();
    els.statusVersion.textContent = `v${await AppService.Version()}`;
    autoInstall = await AppService.CanAutoInstall();
  } catch {
    /* 状态栏拿不到不影响使用 */
  }
  // 流水与状态都由后端每 2 秒推一次；这里只准备空态。
  applyJournal([]);
  await refresh();
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
  // 选中应用的文档每 5 秒对齐一次，保持「看到的就是当下」。
  window.setInterval(() => {
    if (selected) void inspect();
  }, 5000);
}

void boot();
