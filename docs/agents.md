# Claude Code and Codex

`tclaude` drives the `claude` CLI and `tcodex` drives the `codex` CLI. Both
share the same chat; this page covers what differs.

## Command-line flags

```text
tclaude [-model ID] [-mode MODE] [-effort LEVEL] [-resume ID | -continue] [-cwd DIR]
        [-add-dir DIR]... [-system-prompt TEXT] [-append-system-prompt TEXT]
        [-mcp-config FILE] [-no-save] [-title TITLE] [-log FILE [-debug]]
        [-agent NAME] [-fallback-model ID] [-max-turns N] [-max-budget USD]
        [-allowed-tools LIST] [-disallowed-tools LIST] [-allow-bypass]

tcodex  [the shared flags above, up to -debug]
        [-service-tier TIER] [-personality NAME] [-reasoning-summary LEVEL]
```

- `-help` describes each flag.
- `-mcp-config` takes Claude Code's `.mcp.json` format.
- With `-log FILE -debug`, tclaude also writes Claude Code's own debug log
  to `FILE.claude`. Debug logs can contain conversation text.

### Default model, effort and mode

- `tclaude` starts on `claude-haiku-5-5` to keep a new chat cheap
  (`DefaultModel` in `internal/chat/claude.go`). Without `-mode` or
  `-effort`, your Claude Code settings decide.
- `tcodex` passes no model or effort, so `~/.codex/config.toml` decides.

## Claude Code

### Sharing conversations with `claude`

Conversations are ordinary Claude Code sessions in `~/.claude/projects/`,
with your settings, `CLAUDE.md`, hooks, MCP servers and skills.

- A `tclaude` chat shows in `claude --resume` and `claude --continue`, and
  `claude --resume <id>` opens it. Sessions are recorded with the
  entrypoint `tclaude`, because Claude Code hides SDK-tagged sessions from
  its list.
- A `claude` chat shows in `/resume` here, and `tclaude -resume <id>` or
  `-continue` opens it.
- Don't keep one session open in both at once. Each appends to the same
  file without seeing the other's messages.

### Claude-only commands

| Command | Does |
| --- | --- |
| `/agents` | List the subagents Claude can use |
| `/permissions` | Show the permission rules |
| `/usage` | Plan usage |
| `/reload` | Reload skills, plugins and output styles |
| `/cd <folder>` | Move the conversation to another folder |
| `/mcp login\|logout <server>` | Sign in to or out of an MCP server |
| `/btw <question>` | A side question that stays out of the conversation |
| `/plan` | Show the current plan file |
| Ctrl+B | Move a running command to the background |

## Codex

### Sharing conversations with `codex`

Conversations are Codex threads, run through `codex app-server` with your
`~/.codex` config, `AGENTS.md` and MCP servers. Whether `codex resume` lists
threads started from `tcodex` is untested.

### Codex-only commands

| Command | Does |
| --- | --- |
| `/review [uncommitted\|branch <base>\|commit <sha> [title]\|instructions]` | A code review turn |
| `/goal [objective\|pause\|resume\|clear]` | The goal Codex works toward |
| `!command` | Run a shell command in the conversation |
| `/diff` | What the current or last turn changed |
| `/tasks clean` | Stop every background terminal (`/tasks` lists them; Enter stops one) |
| `/archive`, `/unarchive` | Archive or restore the thread |
| `/feedback [--logs] <what went wrong>` | Send feedback to Codex; logs are sent only with `--logs` |

tcodex also shows model-specific rate limits next to the main one, and
offers models from Codex's own catalog with each model's efforts.

### What tcodex leaves out

These Claude features have no Codex equivalent:

- the context breakdown (the bar shows token totals instead)
- `/btw`, `/plan`, `/agents`, `/permissions`, `/usage`, `/reload`, `/cd`,
  `/mcp login` and Ctrl+B
- restoring files on rewind (Codex rewinds only the conversation)
- prompt suggestions and the thinking budget
- the plan-approval overlay (leave plan mode with Shift+Tab)
