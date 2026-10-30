# Craft Agent architecture: the main app owns execution

This document records the design decisions behind Craft Agent — why it is shaped the way
it is, and what every Craft app we integrate must live up to. It complements
[control-protocol.md](control-protocol.md), which documents the apps' wire protocol as
measured; this one documents **our** side of the contract.

## The one principle: the main app owns execution

`craft-agent run` (CLI) and the MCP server are **thin clients**. They never launch a Craft
app themselves when the main app is running; they forward `{app, command, params}` to the
main app over a local unix socket (`internal/gateway`, `<dataDir>/run/gateway.sock`, mode
0600), and the main app executes the command through the host it already holds.

Why this shape exists — it was not the first version. The CLI originally launched the
Craft app as its own child process. AI clients (Trae, and sandboxes generally) run shell
commands inside a process group they reap when the session ends, so every command killed
the app it had just launched: the window flashed once per command and was gone. An app
launched by the long-lived main app belongs to the main app's process group and cannot be
reaped by anyone else.

Consequences that follow from the principle:

- **The window stays open for the human.** Once the main app has launched a Craft app, it
  stays up across commands, sessions and AI clients — the user can watch and edit while
  the agent works. This is the point of the whole project.
- **Fallback rules.** `craft-agent run` first tries the gateway. If no main app is
  running, it falls back to launching directly (the CLI remains usable standalone). But a
  **failed command is never retried over another path** — commands have side effects, and
  "try again somewhere else" means doing it twice.
- **Everything is journaled.** Forwarded commands are recorded in the activity feed with
  their source (`cli`, `mcp`, `gui`), so an AI agent working through the CLI is visible in
  the window like any other actor.
- **Open-by-path is deduplicated.** Because agents (human or AI) retry, the executor
  checks the app's active document identity before running `open` / `file.open`: when the
  target path is already the active document, it answers
  `{"ok":true,"alreadyOpen":true,...}` without asking the app again — a repeated open
  would otherwise pop a dialog or a fresh window each time. Unreadable identity falls
  through to a normal open.
- **One executor, one state.** Document guards, anchors, menus and the journal all live
  with the main app. Thin clients hold no state beyond the request in flight.

## What every integrated app must provide

The suite's apps share a control protocol but not its details (three generations, four
dialects — see control-protocol.md). Whatever else differs, an app we integrate must let
Craft Agent answer two questions. These are requirements, not nice-to-haves:

### 1. Read the active document's identity

The agent must be able to ask "which document is open right now" and get back at minimum
a **title**, a **path** (empty while unsaved) and a **dirty** flag. Measured sources so
far, in the order the bridge probes them:

| Source | Apps | Shape |
|---|---|---|
| `file.info` | DeckCraft 0.11.1 | `{title, path, slides, dirty}` — current document only |
| `doc.list` | FigCraft 0.11.2 | `[{index, name, path, dirty, active}]` — all documents, one call |
| `drawing.inspect` | CADCraft 0.13.0 | `{title, path, entityCount, dirty}` — same shape as `file.info` |

An app with none of these gets a degraded write guard (`guard=none`) and no anchors,
which is exactly what we do not want: the agent edits files the user cannot see anchored.
Identity sources are abstracted (`internal/mcp/doc.go`); adding an app means teaching
`identitySource` one more command, not changing the guard.

### 2. Open a document by path

Recovery ("keep operating the original file across app restarts and window switches")
needs a command that opens a file by path: `file.open {path}`, or the app's native
equivalent (CADCraft's `open {path}`). Without it, an app restart severs the agent from
its file permanently.

### 3. Writes land on the active document, so identity must follow it

The protocol has no document parameter: every write goes to whatever document is active.
Therefore Craft Agent maintains the document context agent-side (anchors), and the anchor
**follows the active document, whoever moves it** — the user switching files in the UI and
the agent running `file.open` are the same event: explicit intent, re-anchor after it.
The guard only pulls the anchored file back by path when no document can be read at all
(app restarted with nothing open). The goal is invariant: **the file the user sees and
the file the agent writes are the same file.**

## Supporting decisions

- **Save awareness by polling, not notification.** No generation of the protocol has
  push/subscribe. The main app polls `file.info`-equivalent identity for anchored,
  connected apps every few seconds; `dirty` true→false or `path` empty→set upgrades the
  anchor in place, journaled.
- **Two-phase anchors, persisted.** Unsaved documents anchor on a title fingerprint;
  once saved, the anchor upgrades to the file path and persists in
  `<dataDir>/anchors.json` (re-read on every access). Visible and releasable in the
  window's anchor panel.
- **MCP session bookkeeping.** The MCP server runs in the AI client's process (spawned
  over stdio), so its lifetime is the client's. The main app does not restart it; it
  records `runtime.json` (pid, version) and flags version-mismatched sessions in the UI.
- **Degrade, don't error.** When an app lacks a capability (no document model, no
  `file.open`), the agent side says so in the result and continues without the feature;
  it never lets an unsupported internal probe block a write the app could have executed.
