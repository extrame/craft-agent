# The Craft control protocol

Every app in the Craft App Suite can expose a local control endpoint when it is started
with `--control`. This document is what we measured against those endpoints — DeckCraft
0.11.1 and PdfCraft — and what `internal/control` implements. It is not a vendor spec;
if an app changes, the app is right and this file is wrong.

## Starting an endpoint

The argument's shape depends on the app. There is no single convention.

| App | Argument | Transport |
|---|---|---|
| DeckCraft 0.11.1 | `--control 7990` | TCP `127.0.0.1:7990` |
| PdfCraft | `--control /path/to/app.sock` | Unix domain socket |

So the transport is a property of each app (`craft.Transport` in `internal/craft/catalog.go`),
not a global constant. Apps that have not been measured are marked as unconfirmed rather
than guessed at.

The endpoint is single-connection-at-a-time in practice and the app reads requests one by
one. This client keeps one connection and serialises calls through a mutex; that is both
simpler and faster than a pool.

## Framing

One JSON object per line, in both directions. No length prefix, no chunking, no
`Content-Length`.

```
→ {"id":1,"method":"engine.execute","params":{"command":"slide.inspect"}}
← {"id":1,"ok":true,"result":{"slides":[…]}}
```

- `id` is chosen by the client and echoed back. It is the only correlation mechanism.
- `params` is optional; `null` and an absent field are equivalent.
- On failure the app answers `{"id":1,"ok":false,"error":"unknown command \`x\`"}`.

> **The `action={"key":"value"}` form that circulates online is wrong.** A server receiving
> it answers `not a control request (missing string 'method')`. If the request is not valid
> JSON at all, or `method` is not a string, the app drops the connection — so a parse
> failure must invalidate the connection, not just the call.

## Methods

Five methods carry everything this tool needs:

| Method | Params | Result |
|---|---|---|
| `engine.commands` | — | Array of command descriptors (see below) |
| `engine.execute` | `{"command": "<id>", "params": {…}}` | Command-specific |
| `document.inspect` | — | Current document structure: pages, shapes, selection, path |
| `ui.inspect` | — | UI state: active tool, window and canvas size, open dialogs, presenting |
| `ui.screenshot` | `{"path": "<file>"}` | Writes a PNG to that path |

`ui.inspect` is also the cheapest liveness probe; this client uses it as a ping.

The app exposes more method names than these. They are not documented here because this
client does not exercise them, and an unexercised method is a guess.

## The command table

`engine.commands` returns one entry per command, and this is the reason a single MCP server
can drive any app in the suite without per-app code:

```json
{ "id": "slide.new",
  "label": "New slide",
  "menu": ["Slide", "New"],
  "params": "layout?: string",
  "shortcut": "⌘⇧N",
  "enabled": true }
```

DeckCraft 0.11.1 returns **377** entries. `params` is prose documentation, not a schema.

`enabled` is **runtime context, not existence**. With no text selected, `format.*` and
`text.selectAll` report `enabled: false`. Filtering a list of available commands by
`enabled` therefore hides real commands from a model — existence is decided by `id` alone,
and `enabled` is only a hint.

## Documents

`file.new` answers `{"document":1,"slides":0}`, so the app has a notion of multiple open
documents. The protocol does not publish it:

| Need | What the protocol offers | Measured behaviour |
|---|---|---|
| Create a document | `file.new` | Returns a document number |
| List documents | *nothing* | `document.*` has only `document.inspect`, which reports the active one |
| Switch document | **`window.next {index}`** | Switches by ordinal; out of range is a silent no-op |
| Write to a specific document | *nothing* | Every write applies to the active document |

`window.next` is the missing `document.activate`, named after "next window". Because
out-of-range indexes are silent no-ops and no document id exists, enumerating documents
means walking indexes until the identity read back is the same twice in a row — and it
makes the UI flicker while it runs.

`file.info` reports `{title, path, slides, dirty}` and **no document id**, so document
identity can only be a `title + path` fingerprint. Save-as and rename look like "a
different document"; two windows on the same file are indistinguishable.

## Page and selection state

These are the four behaviours that make a raw command sequence fragile. They are protocol
facts, not bugs in this tool:

1. **`format.*` takes no `ids`.** It formats the current text selection. The selection does
   not survive across calls, so "set the font of this text box" is a fixed six-command
   dance (`insert.textBox → edit.select → text.edit → text.selectAll → format.fontDialog →
   format.align`), and any step failing just returns `null` or `select text or a shape`.

2. **A cross-page `edit.select` does not fail; it returns `{"selected":[]}`.** Shape ids are
   only valid on the page that holds them, and nothing says "that id lives elsewhere".

3. **`insert.*` takes no page argument.** It targets the active page. `slide.new` switches
   the page as a side effect, but commands like `design.background {index}` switch it back —
   so a following insert lands on the wrong page, silently.

4. **`slide.new` in `file.new`'s answer is a count, not a page id.** Any code that treats
   the writing target as implicit must re-read state afterwards to prove where it landed.

## Why the MCP layer exists

The raw channel is complete — it can do everything the app can do — but several commands
depend on state that an agent cannot see. Passing them straight through produces failures
that look like "the app is broken" while the app is behaving exactly as documented above.

`craft_text`, `craft_style`, `craft_page` and `craft_batch` fold that invisible state
handling into one call each, and `craft_doc` adds the document-anchor guard that the
protocol itself cannot express. Everything they do could be done with `engine.execute`;
they exist so that it does not have to be re-derived on every call.

## What this document does not claim

- Only two of the thirteen apps were measured. For the rest, `catalog.go` records the
  `--control` flag but no confirmed transport.
- Nothing here is a stability promise. One command per undo step is how the apps behave;
  batching commands does not merge them into an atomic operation.
