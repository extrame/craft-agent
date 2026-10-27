# The Craft control protocol

Every app in the Craft App Suite can expose a local control endpoint when it is started
with `--control`. This document is what we measured against those endpoints, and what
`internal/control` implements. It is not a vendor spec; if an app changes, the app is
right and this file is wrong.

Four apps have been measured, each by launching it, reading its command table, and running
commands against a live document:

| App | Version | `--control` takes | Requests | Methods | Commands |
|---|---|---|---|---|---|
| DeckCraft | 0.11.1 | `7990` (a port) | plain JSON | `engine.*` | 377 |
| FigCraft | 0.11.2 | `7991` (a port) | plain JSON | `engine.*` | 597 |
| CADCraft | 0.13.0 | `7994` (a port) | + top-level `token` | `engine.*` | 498 |
| PdfCraft | 0.14.0 | `/path/to/app.control` (a file) | JSON-RPC 2.0, `auth` first | `ui.*` | 206 |

They are **three generations of the same protocol**, not one protocol with flags. The rest
of this document is mostly about how little you can assume across them.

## Starting an endpoint

The argument's shape depends on the app — and, crucially, the argument is not always a
port:

| Generation | `--control` argument | Who picks the port | Apps |
|---|---|---|---|
| genA | port | the caller | DeckCraft 0.11.1, FigCraft 0.11.2 |
| genB | port + token file | the caller | CADCraft 0.13.0 |
| genC | **file path** | the app | PdfCraft 0.14.0 |

In genA and genB, `--control 7990` means "listen on `127.0.0.1:7990`", and a busy port is
the caller's problem. In genC, `--control <path>` means "write your coordinates into this
file and listen wherever you like": on this machine PdfCraft picked `60048`, and the next
launch picked a different one. The file it writes is the only way to find it:

```json
{"pid":41893,"port":60048,"token":"…"}
```

So the transport is a property of each app (`craft.Control` in `internal/craft/catalog.go`),
not a global constant. The host writes the rendezvous file to `<dataDir>/run/<id>.control`
and polls it after launch; the file existing is not the same as the app listening, so the
first successful call is what counts as "up".

> An earlier version of this file claimed PdfCraft took a Unix socket path. It does not.
> There is no Unix socket in this protocol at all.

The endpoint is single-connection-at-a-time in practice and the app reads requests one by
one. This client keeps one connection and serialises calls through a mutex; that is both
simpler and faster than a pool.

## Credentials

genB and genC both require proof that the request comes from the process that started them,
but they deliver it differently:

| | genB (CADCraft 0.13.0) | genC (PdfCraft 0.14.0) |
|---|---|---|
| Where the token comes from | `~/.cache/cadcraft-control-7994.token` | the `token` field of the rendezvous file |
| How it is sent | `token` at the **top level of every request** | one `auth {token}` call after connecting |
| Rejected how | `unauthorized: …` | `authenticate first: auth {token}`, code `-32001` |

Two traps worth writing down:

1. **`~/.cache` is literal, not `~/Library/Caches`.** CADCraft resolves `$HOME/.cache` on
   macOS, where `os.UserCacheDir()` returns `~/Library/Caches`. Reading only the second one
   makes every call fail with `unauthorized` while the token sits one directory away.
2. **"This generation has no `auth` method" is not an error.** genB has no `auth`; sending
   `{"method":"auth"}` answers *unknown method*. The client therefore sends `auth` when it
   has a token and treats an unknown-method answer as "this generation carries the token
   top-level" rather than as failure. The rule is: try, and let the server say which
   generation it is.

A token file is written `0600` and is **never put in JSON that leaves the process** — it
would otherwise flow into `craft_apps` output, the window, and any agent reading them.
`Endpoint.Token` is `json:"-"` for exactly that reason; the registry stores it on the side.

## Framing

One JSON object per line, in both directions. No length prefix, no chunking, no
`Content-Length`. What differs is the envelope:

```
genA/genB
→ {"id":1,"method":"engine.execute","params":{"command":"slide.inspect"}}
← {"id":1,"ok":true,"result":{"slides":[…]}}

genB only (same envelope, extra field)
→ {"id":1,"method":"engine.execute","params":{…},"token":"…"}

genC
→ {"jsonrpc":"2.0","id":1,"method":"auth","params":{"token":"…"}}
← {"jsonrpc":"2.0","id":1,"result":{}}
→ {"jsonrpc":"2.0","id":2,"method":"ui.command","params":{"id":"file.new"}}
← {"jsonrpc":"2.0","id":2,"result":{…}}
```

- `id` is chosen by the client and echoed back. It is the only correlation mechanism.
- `params` is optional; `null` and an absent field are equivalent.
- Failure is either `{"id":1,"ok":false,"error":"unknown command \`x\`"}` (genA/genB) or
  `{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"…"}}` (genC).

> **`error` is a string in one generation and an object in another.** Decoding it as
> `string` fails the whole reply and throws away the only explanation the server gave. It
> has to be a `json.RawMessage` that is then tried as a string, then as `{code,message}`.

> **The `action={"key":"value"}` form that circulates online is wrong.** A server receiving
> it answers `not a control request (missing string 'method')`. If the request is not valid
> JSON at all, or `method` is not a string, the app drops the connection — so a parse
> failure must invalidate the connection, not just the call.

## Methods

Two name families, with the same four meanings:

| Meaning | genA / genB | genC |
|---|---|---|
| List the commands | `engine.commands` | `ui.commands` |
| Run one command | `engine.execute` | `ui.command` |
| Read the document | `document.inspect` | *not measured* |
| Read UI state | `ui.inspect` | *not measured* |

The names are not a version check. The client keeps a candidate list per meaning and
dispatches on what the server says:

```
commands → engine.commands, ui.commands
execute  → engine.execute,  ui.command
inspect  → document.inspect, ui.inspect
state    → ui.inspect,       ui.state
```

The first call tries candidates in order; a reply whose message contains `unknown method`
(or `no such method`) means "try the next one", and the name that worked is remembered for
the life of the client. Any other error is reported as-is. Adding a generation that renames
methods costs one entry in that table, not a branch anywhere else.

> Which candidate list is a **guess until it succeeds** — `document.inspect` and `ui.state`
> on genC are untested because PdfCraft is a reader, not an editor. They are listed so the
> client can find them if they exist, not because they were observed.

### `ui.command` does not take the same parameters

This is the one that silently breaks a naive port:

```
genA/genB  {"command":"slide.inspect","params":{"index":0}}
genC       {"id":"file.new"}                              ← flat, and named id
```

Passing `{"command":…}` to genC answers `missing string parameter id`; passing `{"id":…}` to
genA answers `unknown command`. The parameter shape is a property of the *method name*, so
the client assembles it there (`execParams` in `internal/control/api.go`): anything that is
not `ui.command` gets `{command, params}`, and `ui.command` gets the command id flat, with
the caller's parameters merged in beside it.

### `ui.commands` wraps the list

`engine.commands` returns a bare array. `ui.commands` returns `{"commands":[…]}`. Decoding
genC's reply as an array yields nothing at all — not an error, an empty table. Both shapes
are accepted, array first.

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

`params` is prose documentation, not a schema. Nothing here is a contract: the apps agree
on the *idea* of a command table and disagree on how to write one.

### The same fields, four encodings

| Field | DeckCraft 0.11.1 | FigCraft 0.11.2 | CADCraft 0.13.0 | PdfCraft 0.14.0 |
|---|---|---|---|---|
| `menu` | `["Slide","New"]` — array path | `"Edit"` — one string, or `null` | `["Text/Adjust"]` — one segment, slash inside | `"File"` — one string, and never deeper |
| deepest path | 3 levels | 2 levels | 5 levels | 1 level |
| `shortcut` | `"⌘⇧N"` / `""` | `"Cmd+Z"` / `null` | `"⌘N"` | `⌘O` glyph form |
| `enabled` | `true` / `false` | absent | present | absent |
| also has | — | `undoable` | `aliases`, `interactive` | — |

Note the third column: CADCraft reports five levels of nesting inside a *single* array
element, `["File/New Drawing…/…"]`, so splitting on the array alone would produce one menu
named `File/New Drawing…/…`. The path has to be split on `/` as well as on the array
boundary — which is why the normaliser flattens both, in that order.

This matters more than it looks. `encoding/json` stops at the first wrong-typed field, so a
single `"menu": "Edit"` does not degrade into a missing menu — it fails decoding **the entire
table**, and the caller sees "cannot unmarshal string into Go struct field" instead of 597
commands. Decode `menu` as a union (string, array, or null), treat an absent `enabled` as
unknown rather than false, and keep `id` as the only authority on whether a command exists.

`enabled` is **runtime context, not existence**. With no text selected, `format.*` and
`text.selectAll` report `enabled: false`. Filtering a list of available commands by
`enabled` therefore hides real commands from a model — existence is decided by `id` alone,
and `enabled` is only a hint.

## The menu model

Agents and the window need the same thing: "what can this app do, and where is it". Every
app describes that differently, and none of them describes it completely: on all four, a
third to two thirds of the commands have `menu: null`. `internal/menu` normalises the four
into one tree, and the normalisation rule is:
**keep what was observed, mark what was inferred.**

Four encodings, all becoming `[]string`:

| Input | Path |
|---|---|
| `["Format","Text"]` | `Format › Text` |
| `["Text/Adjust"]` | `Text › Adjust` — one segment, split again on `/` |
| `"File"` | `File` |
| `null` / absent | *no path* — see below |

Commands with no path are not dropped and not given an invented menu. They are grouped by
the segment of their id before the first dot (`drawing.inspect` → `drawing`) and the node
carrying them is flagged `grouped: true`, so a reader can tell "the app has a Drawing menu"
from "these commands all start with `drawing.`". When the two are mixed, real menus sort
first and the prefix buckets follow, each keeping the app's own ordering.

That gives, measured on this machine:

| App | Commands | With a menu path | Grouped by id prefix | Real menus | Prefix buckets |
|---|---|---|---|---|---|
| DeckCraft 0.11.1 | 377 | 241 | 136 | 31 | 24 |
| FigCraft 0.11.2 | 597 | 213 | 384 | 12 | 52 |
| CADCraft 0.13.0 | 498 | 338 | 160 | 11 | 102 |
| PdfCraft 0.14.0 | 206 | 75 | 131 | 5 | 17 |

The two views overlap in name and that is not a bug to be normalised away: DeckCraft has a
real `File` menu *and* a `file` bucket for the 136 commands the menu system does not reach,
and merging them would claim the app publishes a File menu twice as large as it does. A
reader who wants "where can I click it" reads the real menus; a reader who wants "what can
this app do at all" reads the buckets. Both are in the same tree, labelled.

PdfCraft is the extreme case and the reason the fallback exists at all: 5 real menus
(`File`, `Edit`, `View`, `Pages`, `Help`) carry 75 of its 206 commands. Everything else —
`measure.*`, `comment.*`, `form.*`, `redact.*`, `protect.*` — has no menu anywhere in the
protocol, so an agent that only looked at real menus would conclude a PDF app cannot
redact.

The shape is the same for all four:

```
Menu{app, version, fetchedAt, commands, mapped, grouped, sections[], groups[]}
  └ section (Node){name, path, count, grouped, items[], children[]}
      └ item (Item){id, label, shortcut, params, aliases, path, group, enabled}
```

`count` on a node is its whole subtree, because "how many commands are under Format" is the
question a caller actually has. `groups[]` is the second view — one row per id prefix with a
count — because the menu answers "where does a user click it" and the prefix answers "what
kinds of things can this app do", and neither substitutes for the other.

Two consequences worth stating:

- **A menu is a fetch, not a constant.** The table is several hundred entries and changes
  with the app version, so it is cached at `<dataDir>/menus/<app>.json` and keyed by
  version: an app that was upgraded reports `stale` instead of quietly serving the old
  menus. Nothing about the channel needs the cache; the window and the MCP tools do.
- **Only preloaded for apps that are already running.** Fetching a menu means talking to a
  live process, and starting a window to read a menu is not a trade a status panel gets to
  make. Apps that are not running show as not-yet-loaded, and load when they connect.

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
identity has to be agent-side bookkeeping. While a document is unsaved, the only
fingerprint is its title — the agent anchors on it and refuses writes when it changes.
Once the file is saved, `path` becomes the identity: the anchor upgrades to the file path
and is persisted (`<dataDir>/anchors.json`), so a restarted app does not lose the
connection to the file — the agent re-opens it by path with `file.open` and keeps writing
to it. Save-as is not guessed through: a changed path with a live anchor means "bring the
anchored file back", not "follow the new path", because nothing in the protocol can tell
a save-as apart from switching to a different document. Two windows on the same file are
still indistinguishable.

The channel never announces a save. No generation has a notification or subscription
mechanism — everything is request/response — so a user-side save is only visible by
re-reading `file.info` (`dirty` going true→false, `path` going empty→set). Craft Agent's
window polls anchored, connected apps every few seconds for exactly this; the MCP guard
performs the same upgrade before its next write.

This layer is DeckCraft's, and not even the whole suite shares it: FigCraft 0.11.2 has no
`file.info` at all, and PdfCraft's unit is a page in a PDF rather than a slide.

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

`deckcraft_text`, `deckcraft_style`, `deckcraft_page` and `craft_batch` fold that invisible
state handling into one call each, `craft_doc` adds the document-anchor guard that the
protocol itself cannot express, and `craft_menu` serves the normalised tree above so a model
can read *where* a command lives instead of guessing at its name. Everything they do could
be done with `engine.execute`; they exist so that it does not have to be re-derived on every
call. The `deckcraft_` prefix marks the three that assume this particular slide-and-text-box
model — they refuse on any other app, because nothing else here was measured with text boxes
on slides.

## What this document does not claim

- **Four of thirteen apps were measured.** WordCraft, SoundCraft and GridCraft carry the
  port convention in `catalog.go` because they ship alongside FigCraft 0.11.2, but neither
  was on this machine — that is an inference, not a measurement, and it may be wrong in
  either direction (a port when they want a file, or a token nobody expected). The other six
  record the `--control` flag and no transport at all.
- **genC's editor surface is unknown.** PdfCraft is a reader. `ui.command` was confirmed by
  running one (`measure.info`, which answered `disabled right now` — the envelope was
  accepted, the command was not applicable in that state), but nothing here says how a
  generation-C editor would express insert, format or select.
- **The command table is not stable across versions.** All four numbers above are tied to
  the versions in the first table. Treat any count as "at that version".
- Nothing here is a stability promise. One command per undo step is how the apps behave;
  batching commands does not merge them into an atomic operation.
