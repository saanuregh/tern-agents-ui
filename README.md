# tern-agents-ui

Chats with Claude Code and Codex that Tern draws natively. `tclaude` and
`tcodex` run in a pane and speak the [Tern Surface Protocol](https://docs.stencil.so/tern/protocol/index.html)
through the [Go SDK](https://github.com/stencil-hq/tern-sdk/tree/main/go/tern):
an inline surface with the transcript above and the composer and status bar
docked below. [agent-cli-connector](https://github.com/saanuregh/agent-cli-connector)
drives the `claude` or `codex` CLI in the same process. Both share one
interface (`internal/chat`); `internal/chat/claude.go` and `codex.go` hold
what differs.

## Install

```sh
./build.sh                          # bin/tclaude and bin/tcodex; needs Go and the claude or codex CLI
tern plugin link "$PWD/plugin"      # optional: palette commands
```

Run `bin/tclaude` or `bin/tcodex` in any Tern pane, or use the palette:
**New Claude chat**, **New Codex chat** and their **Continue last …**
commands open one beside the focused pane, in that pane's directory.
Outside Tern they fall back to a plain line-by-line chat.

```text
tclaude [-model ID] [-mode MODE] [-effort LEVEL] [-resume ID | -continue] [-cwd DIR]
        [-add-dir DIR]... [-system-prompt TEXT] [-append-system-prompt TEXT]
        [-mcp-config FILE] [-no-save] [-title TITLE] [-log FILE [-debug]]
        [-agent NAME] [-fallback-model ID] [-max-turns N] [-max-budget USD]
        [-allowed-tools LIST] [-disallowed-tools LIST] [-allow-bypass]
tcodex  (the shared flags, then) [-service-tier TIER] [-personality NAME] [-reasoning-summary LEVEL]
```

`-help` describes each. `-mcp-config` takes Claude Code's `.mcp.json`
format. With `-log` and `-debug`, tclaude also writes Claude Code's own
debug log to `FILE.claude`.

`tclaude` starts on `claude-haiku-5-5` (`DefaultModel` in
`internal/chat/claude.go`). `tcodex` passes no model or effort, so
`~/.codex/config.toml` decides. On exit each prints the command that
resumes the conversation.

## Working with the regular CLIs

Claude conversations are ordinary Claude Code sessions in
`~/.claude/projects/`, using your settings, CLAUDE.md, hooks, MCP servers
and skills. Without a `-mode` flag the permission mode is your settings'
default, as in `claude`.

- A `tclaude` chat shows in `claude --resume` and `claude --continue`, and
  `claude --resume <id>` opens it. Sessions are recorded with entrypoint
  `tclaude`, because Claude Code hides SDK-tagged sessions from its list.
- A `claude` chat shows in `/resume` here, and `tclaude -resume <id>` or
  `-continue` opens it.
- Don't keep one session open in both at once: each appends to the same
  file without seeing the other's messages.

Codex conversations are Codex threads, run through `codex app-server` with
your `~/.codex` config, AGENTS.md and MCP servers. Whether `codex resume`
lists threads started from `tcodex` is unverified.

## Codex

`tcodex` has what Codex offers and Claude doesn't:

- `/review [uncommitted|branch <base>|commit <sha> [title]|instructions]`: a
  code review turn.
- `/goal [objective|pause|resume|clear]`: the goal Codex works toward.
- `!command`: run a shell command in the conversation.
- `/diff`: what the current or last turn changed.
- `/tasks` also lists background terminals; Enter terminates one, and
  `/tasks clean` stops them all.
- `/archive`, `/unarchive`, and `/feedback [--logs] <what went wrong>`
  (logs are sent only with `--logs`).
- Model-specific rate limits next to the main one, and the model picker
  from Codex's own catalog with each model's efforts.

It leaves out what Codex lacks: the context breakdown (the ring shows the
token totals), `/btw`, `/plan`, `/agents`, `/permissions`, `/usage`,
`/reload`, `/cd`, `/mcp login`, Ctrl+B, file restore on rewind (Codex
rewinds the conversation only), prompt suggestions, the thinking budget,
and the plan-approval sheet (leave plan mode with Shift+Tab).

## Keys

| Key | Does |
| --- | --- |
| Enter | Send; while the agent works the message is queued above the composer (Edit or × on each) |
| Ctrl+Enter | Steer: inject the message into the running turn |
| Shift+Enter, Alt+Enter, Ctrl+J | New line |
| Esc | Interrupt (queued messages come back to the composer); Esc Esc on an empty composer opens Rewind (↑↓ message, Tab what to restore, Enter) |
| Ctrl+C | Clear the draft, else interrupt; twice within 2 seconds quits |
| Shift+Tab | Cycle permission mode: ask, accept edits, plan, and auto when the model supports it |
| Tab | Complete `/commands` and `@files` (↑↓ choose, Enter accepts); accept Claude's suggested next message (also →) |
| ↑ / ↓ on the first or last line | Earlier messages |
| Ctrl+O | Expand every tool call and thought |
| Ctrl+T | Full todo list instead of the pill |
| Ctrl+Z / Ctrl+Y | Undo / redo in the composer |
| 1 / 2 / a / 3 / Esc (empty composer) | Allow, allow for the session, always allow, deny, deny and stop |
| Text, then Enter | Deny a permission and tell the agent why |
| Enter / Esc (MCP form) | Submit / decline |
| 1–9, ↑↓, Space, Tab | Pick a question's option, toggle in a multiple choice, next question |
| Ctrl+V, Cmd+V, Alt+V | Paste the clipboard: an image is attached, text is pasted |

Tern's native editing works in the composer: selection with Shift+arrows or
the mouse, Cmd/Ctrl+A, copy and cut, IME.

## Commands

`/help` lists them all. Local: `/config` (settings sheet: ↑↓ rows, ←→ pages, Enter changes), `/new` (`/clear`), `/resume [id]`,
`/continue`, `/fork`, `/rewind`, `/model [id]`, `/effort [level]`,
`/mode [mode]` (`/mode auto` for auto mode), `/compact [instructions]`, `/title [title]`, `/context`,
`/status`, `/cost`, `/mcp [reconnect|enable|disable name]`, `/btw <question>`,
`/tasks`, `/plan`, `/export`, `/doctor`, `/quit`. Claude also has `/agents`,
`/permissions`, `/usage`, `/reload` (skills, plugins, output styles),
`/cd <folder>`, `/mcp login|logout <server>`, and `/title` with no title to
let Claude name the conversation. In the `/resume` picker, Ctrl+R renames
a conversation and Ctrl+D deletes one (press twice). Ctrl+B moves a running
command to the background. Anything else starting with `/` (skills,
custom and plugin commands) goes to the agent; `//name` runs the agent's
command where a local one has the same name. If the agent's process exits,
`/continue` resumes the same conversation.

Attachments: Ctrl+V (or Cmd+V, Alt+V) attaches a screenshot or copied image
from the clipboard, and pastes text otherwise (via `wl-paste`, `xclip` or
`xsel` on Linux, `pngpaste`, `osascript` or `pbpaste` on macOS). Dropping files on the pane, or pasting their paths, attaches them
instead of typing the paths. They show as thumbnails above the composer and
in your sent message; Backspace on an empty composer removes the last one.
`@path/to/image.png` in a message also attaches the image; other `@paths` are
resolved by Claude Code.

## What it shows

- Messages as Markdown (user messages right-aligned), thinking folded.
- Commands as omp draws them: "Bash" and what the command is for, the
  command as a highlighted script (long ones fold to five lines with "N more
  lines"), the output clamped the same way, and a "✓ Done 0.1s" footer.
- Above the composer: a context bar (percent used and window size, click
  for details) and a pill counting background jobs (click for `/tasks`).
  In the composer: the model (with a menu), effort, folder, git branch,
  permission mode and cost; the active goal quoted as the placeholder.
- Subagents with their model and effort, such as `haiku · low`.
- What hooks print (a SessionStart greeting, a Stop check), "Running hook …"
  while one runs, and a "Resumed" line where an earlier conversation ends.
- Tern's agent notifications, through OSC 7501 program status as omp sends
  it: the tab and the attention inbox show when the agent works, waits on
  you (with the question or the permission it asks for), is done, or failed
  (with the error). Tern raises these for panes you aren't looking at.
- A new conversation starts the agent only when you send the first message,
  so opening the app or `/new` runs no hooks and fills no context; model,
  effort and mode picked before then apply at that start.
- Once a conversation is running, tclaude keeps a spare Claude Code process
  ready so the next one starts at once; it costs memory, not tokens.
- Tool calls as native tool cards: commands with their output, edits as
  diffs with a +/− count, reads, searches, web fetches, MCP calls, images.
- Subagents as agent rows with their own tool calls nested; background tasks
  in the dock with Stop.
- Consecutive reads grouped into one "Read N files" row; todo updates
  inline as a checklist; live output of running commands; Claude Code's
  one-line summaries of finished tool batches.
- Permission cards (command, diff, reason) with allow, allow for the
  session, always allow, deny with a reason (the composer's text), and deny
  and stop. Plan approval as an overlay: auto-accept, ask before edits, or
  keep planning with feedback typed in the composer.
- Claude's questions inside the composer, one tab per question: numbered
  options with previews, multi-select, an Other row typed in place, and a
  review tab before submitting. MCP elicitation forms and URL prompts.
- Rewind in place: the chosen message is marked, what would be dropped is
  dimmed, and you choose to restore the conversation, the code (Claude
  Code's file checkpoints, with a dry-run count of files) or both.
- Searchable pickers with previews for `/model`, `/effort`, `/mode`,
  `/resume` and `/tasks` (Enter on a running task stops it).
- `/config`: model, effort, thinking budget, permission mode, context,
  compaction, MCP, display switches, prompt suggestions, every shortcut, and
  the account and rate limits.
- Claude's predicted next message as ghost text in the composer.
- A live context viewer (click the ring in the composer bar, or `/context`):
  Claude Code's own breakdown as a block grid, a stacked bar with the
  auto-compact point, a legend, and folded memory files, skills, agents,
  MCP tools and messages. It refreshes after every turn and every few
  seconds while open or while Claude works; the measurement is local to
  Claude Code and makes no API call.
- The todo list, rate-limit warnings, goal, queued count and cost in the
  composer bar. Right-click a message to copy it, rewind to it or
  fork from it.

Not included: Ctrl+G (edit the draft in `$EDITOR`). The Tern Go SDK cannot
pause its input reader, so an external editor would compete with it for
keystrokes.

## Develop

```sh
go test -race ./...
bin/tclaude -demo chat   # sample screens without the agent; -help lists them
bin/tclaude -log /tmp/tclaude.log   # diagnostics; add -debug for the agent CLI's own output
```

`-debug` logs can contain conversation text.

The look comes from Tern's own chat skin: the transcript and composer use
the roles Tern styles for its built-in agent (`omp.user`, `omp.assistant`,
`omp.editor`, `omp.composer.*`, `omp.working`, `omp.welcome`). Those roles
are not a documented API, so a Tern update can change how they look. The
stylesheet in `internal/chat/main.go` adds the rest.
