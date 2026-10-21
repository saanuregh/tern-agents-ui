# Features

## The conversation

- Messages render as Markdown. Your messages are right-aligned, and
  thinking is folded.
- Tool calls are native tool cards: commands with their output, edits as
  diffs with a +/− count, reads, searches, web fetches, MCP calls and
  images.
- Commands look the way omp draws them: "Bash" with what the command is
  for, the command as a highlighted script, the output, and a "✓ Done 0.1s"
  footer. Long scripts and outputs fold to a few lines with "N more lines".
- Consecutive reads are grouped into one "Read N files" row. Running
  commands show live output, and Claude Code adds a one-line summary after
  each batch of tools.
- Todo updates appear inline as a checklist.
- Subagents appear as agent rows with their model and effort, such as
  `haiku · low`, and their own tool calls nested inside. Background tasks
  sit in the dock with a Stop button.
- Hook output (a SessionStart greeting, a Stop check) shows as a folded
  card. "Running hook …" shows while one runs.
- A "Resumed" line marks where an earlier conversation ends.
- Right-click a message to copy it, rewind to it or fork from it.

## The composer

- Above it: a context bar (percent used and window size; click it for
  details) and a pill counting background jobs (click it for `/tasks`).
- Inside it: the model (with a menu), effort, folder, git branch,
  permission mode and cost. The active goal shows as the placeholder.
- Claude's predicted next message appears as ghost text; Tab accepts it.
- Rate-limit warnings and the number of queued messages show in the bar.

## Asking you

- **Permissions:** a card shows the command, diff or reason. Choose allow,
  allow for the session, always allow, deny with a reason (typed in the
  composer), or deny and stop.
- **Plans:** plan approval opens as an overlay: auto-accept, ask before
  edits, or keep planning with feedback typed in the composer.
- **Questions:** the agent's questions appear inside the composer, one tab
  per question, with numbered options and previews, multiple choice, an
  "Other" row you type in place, and a review tab before submitting.
- **MCP:** elicitation forms and URL prompts.

## Going back

Rewind works in place. The chosen message is marked and what would be
dropped is dimmed. You choose to restore the conversation, the code (Claude
Code's file checkpoints, with a count of the files first) or both.

## Pickers and settings

- `/model`, `/effort`, `/mode`, `/resume` and `/tasks` open searchable
  pickers with previews.
- `/config` covers model, effort, thinking budget, permission mode,
  context, compaction, MCP, display switches, prompt suggestions, every
  shortcut, and the account and its rate limits.

## Context viewer

Click the context bar, or run `/context`, to see Claude Code's own
breakdown:

- a grid of blocks, and a stacked bar marking the auto-compact point
- a legend
- folded lists of memory files, skills, agents, MCP tools and messages

It refreshes after every turn, and every few seconds while it is open or
while Claude works. The measurement happens inside Claude Code and makes no
API call.

## Notifications

The chat reports its state to Tern the way omp does (OSC 7501 program
status). The tab and Tern's attention inbox show when the agent is working,
waiting on you (with the question or permission it asks for), done, or
failed (with the error). Tern raises these only for panes you aren't
looking at.

## Starting up

- A new conversation starts the agent only when you send the first message.
  Opening a chat or running `/new` runs no hooks and fills no context. The
  model, effort and mode you pick before then apply when it starts.
- Once a conversation is running, tclaude keeps a spare Claude Code process
  ready so the next one starts at once. It costs memory, not tokens.
