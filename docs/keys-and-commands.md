# Keys and commands

## Keys

### Writing and sending

| Key | Does |
| --- | --- |
| Enter | Send. While the agent works, the message is queued above the composer, with Edit and × on each |
| Ctrl+Enter | Steer: add the message to the running turn |
| Shift+Enter, Alt+Enter, Ctrl+J | New line |
| Tab | Complete `/commands` and `@files` (↑↓ choose, Enter accepts), or accept Claude's suggested next message (also →) |
| ↑ / ↓ on the first or last line | Earlier messages |
| Ctrl+Z / Ctrl+Y | Undo / redo in the composer |
| Ctrl+V, Cmd+V, Alt+V | Paste the clipboard: an image is attached, text is pasted |

Tern's own editing works in the composer: selecting with Shift+arrows or
the mouse, Cmd/Ctrl+A, copy and cut, and IME input.

### Controlling the agent

| Key | Does |
| --- | --- |
| Esc | Stop the agent. Queued messages come back to the composer |
| Esc Esc on an empty composer | Open Rewind (↑↓ message, Tab chooses what to restore, Enter) |
| Ctrl+C | Clear the draft, or stop the agent; twice within 2 seconds quits |
| Shift+Tab | Cycle the permission mode: ask, accept edits, plan, and auto when the model supports it |
| Ctrl+B | Move a running command to the background (Claude) |

### Viewing

| Key | Does |
| --- | --- |
| Ctrl+O | Expand every tool call and thought |
| Ctrl+T | Show the full todo list instead of the pill |

### Answering the agent

| Key | Does |
| --- | --- |
| 1 / 2 / a / 3 / Esc (empty composer) | Permission: allow, allow for the session, always allow, deny, deny and stop |
| Text, then Enter | Deny a permission and tell the agent why |
| 1–9, ↑↓, Space, Tab | Question: pick an option, toggle one in a multiple choice, go to the next question |
| Enter / Esc | MCP form: submit / decline |

## Commands

`/help` lists every command available in the current chat.

### Conversations

| Command | Does |
| --- | --- |
| `/new` (or `/clear`) | Start a new conversation |
| `/resume [id]` | Open an earlier conversation. In the picker, Ctrl+R renames one and Ctrl+D deletes one (press twice) |
| `/continue` | Resume the most recent conversation here, or the current one if the agent exited |
| `/fork` | Continue in a copy of this conversation |
| `/rewind` | Go back to an earlier message |
| `/compact [instructions]` | Summarize the conversation to free context |
| `/title [title]` | Name the conversation. In tclaude, with no title, Claude names it |
| `/export` | Save the conversation |
| `/quit` | Quit |

### Model and settings

| Command | Does |
| --- | --- |
| `/model [id]` | Pick the model |
| `/effort [level]` | Pick the reasoning effort |
| `/mode [mode]` | Pick the permission mode (`/mode auto` for auto mode) |
| `/config` | Settings sheet: ↑↓ rows, ←→ pages, Enter changes |

### Status

| Command | Does |
| --- | --- |
| `/context` | What fills the context window |
| `/status` | Session details |
| `/cost` | What the conversation has cost |
| `/tasks` | Background tasks; Enter on a running one stops it |
| `/mcp [reconnect\|enable\|disable name]` | MCP servers |
| `/doctor` | Check the agent's setup |

### Other

| Command | Does |
| --- | --- |
| `/btw <question>` | Ask a side question without adding it to the conversation (Claude) |
| `/plan` | Show the current plan file (Claude) |

Claude Code and Codex each add their own commands; see
[Claude Code and Codex](agents.md).

Anything else starting with `/`, such as skills and custom or plugin
commands, goes to the agent. `//name` runs the agent's own command when a
local command has the same name.

## Attachments

- **Paste:** Ctrl+V (or Cmd+V, Alt+V) attaches a screenshot or copied image,
  and pastes text otherwise. On Linux this uses `wl-paste`, `xclip` or
  `xsel`; on macOS `pngpaste`, `osascript` or `pbpaste`.
- **Drop:** dropping files on the pane, or pasting their paths, attaches
  them instead of typing the paths.
- **Mention:** `@path/to/image.png` in a message attaches the image. Other
  `@paths` are passed to the agent, which reads the files itself.

Attachments show as thumbnails above the composer and in your sent message.
Backspace on an empty composer removes the last one.

## Not supported

Ctrl+G (edit the draft in `$EDITOR`): the Tern Go SDK cannot pause its input
reader, so an external editor would compete with it for keystrokes.
