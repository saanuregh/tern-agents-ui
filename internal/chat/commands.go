package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/claude"
	"github.com/saanuregh/agent-cli-connector/codex"
	"github.com/stencil-hq/tern-sdk/go/tern/ui"
)

// localCommand is a slash command this program runs itself. Commands it
// doesn't know (skills, custom and plugin commands) go to Claude as text.
type localCommand struct {
	Name  string
	Args  string
	Help  string
	Run   func(a *app, arg string)
	Needs feature // offered only when the agent has it
}

var localCommands []localCommand

// commandsLocked lists the local commands this agent offers.
func (a *app) commandsLocked() []localCommand {
	cmds := slices.DeleteFunc(slices.Clone(localCommands), func(c localCommand) bool {
		return c.Needs != featNone && !a.backend.Has(c.Needs)
	})
	for i, c := range cmds {
		switch {
		case c.Name == "context" && !a.backend.Has(featContextBreakdown):
			cmds[i].Help = "Show how much of the context window is used (also click the ring)"
		case c.Name == "config" && !a.backend.Has(featThinkingBudget):
			cmds[i].Help = "Settings: model, effort, permissions, display, keys"
		}
	}
	return cmds
}

// aliases run like the command they copy but stay out of suggestions.
var aliases = map[string]bool{"clear": true, "exit": true}

func init() {
	localCommands = []localCommand{
		{"help", "", "Show commands and keys", (*app).cmdHelp, featNone},
		{"new", "", "Start a new conversation", (*app).cmdNew, featNone},
		{"clear", "", "Start a new conversation", (*app).cmdNew, featNone},
		{"resume", "[session id]", "Resume an earlier conversation in this directory", (*app).cmdResume, featNone},
		{"continue", "", "Resume the most recent conversation in this directory", (*app).cmdContinue, featNone},
		{"fork", "", "Continue in a copy of this conversation", (*app).cmdFork, featNone},
		{"rewind", "", "Go back to an earlier message (also Esc Esc)", (*app).cmdRewind, featNone},
		{"model", "[model]", "Choose the model", (*app).cmdModel, featNone},
		{"effort", "[level]", "Choose the reasoning effort", (*app).cmdEffort, featNone},
		{"mode", "[mode]", "Choose the permission mode (also Shift+Tab)", (*app).cmdMode, featNone},
		{"compact", "[instructions]", "Summarize the conversation to free context", (*app).cmdCompact, featNone},
		{"title", "[title]", "Name this conversation; with no title, Claude names it", (*app).cmdTitle, featNone},
		{"context", "", "Show what fills the context window, live (also click the ring)", (*app).cmdContext, featNone},
		{"config", "", "Settings: model, effort, permissions, thinking, display, keys", (*app).cmdConfig, featNone},
		{"status", "", "Show session, account and connection details", (*app).cmdStatus, featNone},
		{"cost", "", "Show this conversation's cost and token use", (*app).cmdCost, featNone},
		{"mcp", "[reconnect|enable|disable|login|logout name]", "Show or manage MCP servers", (*app).cmdMCP, featNone},
		{"btw", "<question>", "Ask a side question that stays out of the conversation", (*app).cmdBtw, featSideQuestion},
		{"tasks", "[clean]", "Show background tasks and stop them", (*app).cmdTasks, featNone},
		{"plan", "", "Show the plan file", (*app).cmdPlan, featPlanFile},
		{"export", "", "Save the conversation as JSON in this directory", (*app).cmdExport, featExport},
		{"review", "[uncommitted|branch <base>|commit <sha>|instructions]", "Review code changes", (*app).cmdReview, featReview},
		{"goal", "[objective|pause|resume|clear]", "Show or set the goal Codex works toward", (*app).cmdGoal, featGoals},
		{"diff", "", "Show what the current or last turn changed", (*app).cmdDiff, featTurnDiff},
		{"agents", "", "List the subagents Claude can start", (*app).cmdAgents, featAgents},
		{"permissions", "", "Show the permission rules in effect", (*app).cmdPermissions, featPermissionRules},
		{"usage", "", "Show plan usage and rate limits", (*app).cmdUsage, featPlanUsage},
		{"reload", "", "Reload skills, plugins and output styles", (*app).cmdReload, featReload},
		{"cd", "<folder>", "Move this conversation to another folder", (*app).cmdCd, featCd},
		{"doctor", "", "Check the agent's CLI: version, sign-in, capabilities", (*app).cmdDoctor, featNone},
		{"archive", "", "Archive this conversation", (*app).cmdArchive, featArchive},
		{"unarchive", "", "Bring this conversation back from the archive", (*app).cmdUnarchive, featArchive},
		{"feedback", "[--logs] <what went wrong>", "Send feedback to OpenAI; --logs adds Codex's logs", (*app).cmdFeedback, featFeedback},
		{"quit", "", "Quit", (*app).cmdQuit, featNone},
		{"exit", "", "Quit", (*app).cmdQuit, featNone},
	}
}

// runCommandLocked runs a local command; false sends the text to Claude.
func (a *app) runCommandLocked(name, arg string) bool {
	for _, c := range a.commandsLocked() {
		if c.Name == name {
			if a.demo && slices.Contains([]string{"new", "clear", "resume", "continue", "fork"}, name) {
				a.toastLocked("info", "The demo doesn't start "+a.backend.Name())
				return true
			}
			c.Run(a, arg)
			return true
		}
	}
	return false
}

func (a *app) commandSuggestionsLocked(prefix string) []suggestion {
	prefix = strings.ToLower(prefix)
	var out []suggestion
	seen := map[string]bool{}
	add := func(name, args, help string) {
		if seen[name] || !strings.HasPrefix(strings.ToLower(name), prefix) {
			return
		}
		seen[name] = true
		value := "/" + name
		if args != "" {
			value += " "
		}
		out = append(out, suggestion{Value: value, Label: strings.TrimSpace("/" + name + " " + args), Detail: help})
	}
	local := map[string]bool{}
	for _, c := range a.commandsLocked() {
		local[c.Name] = true
		if aliases[c.Name] && c.Name != prefix {
			continue
		}
		add(c.Name, c.Args, c.Help)
	}
	// A Claude Code command named like a local one is reached as //name.
	for _, c := range a.info.Commands {
		if local[c.Name] {
			add("/"+c.Name, c.ArgumentHint, c.Description+" ("+a.backend.Product()+")")
		} else {
			add(c.Name, c.ArgumentHint, c.Description)
		}
	}
	// An exact name goes first, so Enter on "/mode" never picks "/model".
	for i, s := range out {
		if strings.TrimSpace(s.Value) == "/"+prefix && i > 0 {
			out[0], out[i] = out[i], out[0]
			break
		}
	}
	return out[:min(len(out), 12)]
}

// Commands. Each runs with mu held; slow Claude calls go through async and
// take the lock again for their results.

func (a *app) cmdHelp(string) {
	var b strings.Builder
	b.WriteString("**Commands**\n\n")
	for _, c := range a.commandsLocked() {
		if aliases[c.Name] {
			continue
		}
		fmt.Fprintf(&b, "- `/%s` — %s\n", strings.TrimSpace(c.Name+" "+c.Args), c.Help)
	}
	if len(a.info.Commands) > 0 {
		b.WriteString("\n**From " + a.backend.Product() + "** (skills, custom and plugin commands; `//name` runs one named like a command above)\n\n")
		local := map[string]bool{}
		for _, c := range a.commandsLocked() {
			local[c.Name] = true
		}
		for _, c := range a.info.Commands {
			slash := "/"
			if local[c.Name] {
				slash = "//"
			}
			fmt.Fprintf(&b, "- `%s%s` — %s\n", slash, strings.TrimSpace(c.Name+" "+c.ArgumentHint), c.Description)
		}
	}
	b.WriteString("\n**Keys**\n\n" +
		"- Enter sends; while " + a.backend.Name() + " works it queues the message. Ctrl+Enter steers the running turn instead.\n" +
		"- Shift+Enter, Alt+Enter or Ctrl+J: new line. ↑/↓ on the first or last line: earlier messages.\n" +
		"- Esc interrupts; Esc Esc on an empty composer rewinds. Ctrl+C clears, interrupts, or quits when pressed twice.\n" +
		"- Shift+Tab cycles the permission mode (auto mode is included when the model supports it). Ctrl+O expands every tool call. Ctrl+T shows the full todo list.\n" +
		"- Tab completes `/commands` and `@files`; `@image.png` attaches the image.\n" +
		"- Ctrl+V (or Cmd+V, Alt+V) pastes: an image from the clipboard is attached, text is pasted. Dropping or pasting files attaches them. Backspace on an empty composer removes the last one.\n" +
		"- With a permission request waiting: 1 allow, 2 allow for the session, a always allow, 3 deny, Esc deny and stop. Type a reason and press Enter to deny with it.\n" +
		"- `/config` opens the settings: ↑↓ rows, ←→ pages, Enter changes.\n")
	if a.backend.Has(featShell) {
		b.WriteString("- `!command` runs a shell command in the conversation; its output is shared with " + a.backend.Name() + ".\n")
	}
	a.addLocal(localCard{Title: "Help", Text: b.String()})
}

func (a *app) cmdNew(string) { go a.start(a.sessionConfigLocked("")) }

// sessionConfigLocked is the config for a new or resumed session: the
// command line's, with the model, effort and mode chosen since.
func (a *app) sessionConfigLocked(resume string) agent.Config {
	cfg := a.base
	cfg.Model, cfg.Effort, cfg.PermissionMode, cfg.Resume = a.model, a.effort, a.mode, resume
	cfg.Title = "" // -title names the first conversation only
	return cfg
}

func (a *app) cmdQuit(string) { a.quit() }

func (a *app) cmdResume(arg string) {
	if arg != "" {
		// Check the id first: a typo must not end the conversation.
		cwd := a.cwd
		a.async("Resume", func(ctx context.Context) error {
			if err := a.backend.SessionExists(ctx, arg, cwd); err != nil {
				a.mu.Lock()
				a.toastLocked("error", fmt.Sprintf("No conversation %s in %s", arg, tildePath(cwd)))
				a.mu.Unlock()
				return nil
			}
			a.mu.Lock()
			cfg := a.sessionConfigLocked(arg)
			a.mu.Unlock()
			a.start(cfg)
			return nil
		})
		return
	}
	a.listSessionsLocked(func(sessions []agent.SessionSummary) {
		var items []choice
		for _, s := range sessions {
			id := s.ID
			label := firstNonEmpty(s.Title, firstLine(s.FirstPrompt), id)
			var pv strings.Builder
			fmt.Fprintf(&pv, "### %s\n\n", label)
			if s.FirstPrompt != "" {
				fmt.Fprintf(&pv, "**First** %s\n\n", firstLine(s.FirstPrompt))
			}
			if s.LastPrompt != "" && s.LastPrompt != s.FirstPrompt {
				fmt.Fprintf(&pv, "**Latest** %s\n\n", firstLine(s.LastPrompt))
			}
			fmt.Fprintf(&pv, "| | |\n| --- | --- |\n| Updated | %s |\n", s.UpdatedAt.Local().Format("Jan 2 15:04"))
			if s.Model != "" {
				fmt.Fprintf(&pv, "| Model | %s |\n", s.Model)
			}
			fmt.Fprintf(&pv, "| Session | `%s` |\n", id)
			items = append(items, choice{Label: label, Icon: "message", Current: id == a.info.ID,
				Facts: map[string]any{"when": relTime(s.UpdatedAt)}, Preview: pv.String(), Run: func() {
					go a.start(a.sessionConfigLocked(id))
				}})
		}
		ids := make([]string, len(sessions))
		for i, s := range sessions {
			ids[i] = s.ID
		}
		a.openPickerLocked(&menu{title: "Resume a conversation", subtitle: tildePath(a.cwd), noun: "conversations", icon: "history",
			items: items, columns: []ui.PickerColumn{{ID: "when", Format: ui.FormatText}}, actions: a.sessionActionsLocked(ids, items)})
	})
}

// sessionRenamer renames a stored conversation (Claude Code).
type sessionRenamer interface {
	RenameSession(ctx context.Context, sessionID, cwd, title string) error
}

// sessionActionsLocked are /resume's rename and delete, where the agent
// supports them; ids are the rows' conversations.
func (a *app) sessionActionsLocked(ids []string, items []choice) []menuAction {
	var acts []menuAction
	if _, ok := a.backend.Provider().(sessionRenamer); ok {
		acts = append(acts, menuAction{id: "rename", label: "Rename", key: "ctrl+r", run: func(i int) {
			a.menu = nil
			a.renaming = &renameTarget{id: ids[i], title: items[i].Label, draft: a.composer.String()}
			a.composer.set(items[i].Label)
		}})
	}
	if del, ok := a.backend.Provider().(agent.SessionDeleter); ok {
		acts = append(acts, menuAction{id: "delete", label: "Delete", key: "ctrl+d", confirm: true, run: func(i int) {
			if ids[i] == a.info.ID {
				a.toastLocked("info", "This conversation is open; /new first to delete it")
				return
			}
			a.menu = nil
			id, cwd := ids[i], a.cwd
			a.async("Delete", func(ctx context.Context) error {
				if err := del.DeleteSession(ctx, id, cwd); err != nil {
					return err
				}
				a.mu.Lock()
				a.toastLocked("success", "Deleted “"+items[i].Label+"”")
				a.mu.Unlock()
				return nil
			})
		}})
	}
	return acts
}

// renameTarget is the stored conversation the composer is naming.
type renameTarget struct{ id, title, draft string } // draft is what the composer held before

// renameLocked saves the composer's text as the conversation's title.
func (a *app) renameLocked(title string) {
	r := a.renaming
	a.renaming = nil
	rn, ok := a.backend.Provider().(sessionRenamer)
	if !ok || strings.TrimSpace(title) == "" {
		return
	}
	cwd := a.cwd
	a.async("Rename", func(ctx context.Context) error {
		if err := rn.RenameSession(ctx, r.id, cwd, strings.TrimSpace(title)); err != nil {
			return err
		}
		a.mu.Lock()
		if r.id == a.info.ID {
			a.info.Title = strings.TrimSpace(title)
		}
		a.toastLocked("success", "Renamed to “"+strings.TrimSpace(title)+"”")
		a.mu.Unlock()
		return nil
	})
}

func (a *app) cmdContinue(string) {
	current := a.info.ID
	if a.sess == nil && a.status == "closed" && current != "" {
		// Claude exited: pick this conversation up again.
		go a.start(a.sessionConfigLocked(current))
		return
	}
	a.listSessionsLocked(func(sessions []agent.SessionSummary) {
		for _, s := range sessions {
			if s.ID != current {
				go a.start(a.sessionConfigLocked(s.ID))
				return
			}
		}
		a.toastLocked("info", "No earlier conversation in this directory")
	})
}

// listSessionsLocked lists stored sessions for the directory, newest first,
// and calls fn with mu held.
func (a *app) listSessionsLocked(fn func([]agent.SessionSummary)) {
	lister, ok := a.backend.Provider().(agent.SessionLister)
	if !ok {
		a.toastLocked("error", "Listing sessions is not supported")
		return
	}
	cwd := a.cwd
	a.async("List sessions", func(ctx context.Context) error {
		sessions, err := lister.ListSessions(ctx, agent.ListSessionsOptions{Cwd: cwd, Limit: 500})
		if err != nil {
			return err
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		fn(sessions)
		a.changed()
		return nil
	})
}

func (a *app) cmdFork(arg string) { a.forkLocked(arg) }

// forkLocked continues in a copy of the conversation, through turnID when
// given. The original stays stored and resumable.
func (a *app) forkLocked(turnID string) {
	sess, _, err := a.liveSession()
	if err != nil {
		a.toastLocked("error", err.Error())
		return
	}
	forker, ok := sess.(agent.Forker)
	if !ok {
		a.toastLocked("error", "Forking is not supported")
		return
	}
	cfg := agent.Config{ForkAtTurn: turnID, ProviderOptions: a.base.ProviderOptions, Env: a.base.Env}
	gen := a.gen
	a.async("Fork", func(ctx context.Context) error {
		branch, err := forker.ForkSession(ctx, cfg)
		if err != nil {
			return err
		}
		if !a.adopt(gen, branch) {
			return nil
		}
		a.mu.Lock()
		a.toastLocked("success", "Forked; the original conversation is still in /resume")
		a.mu.Unlock()
		return nil
	})
}

func (a *app) cmdRewind(string) { a.startRewindLocked("") }

// rewindLocked drops the message it and everything after it, and puts its
// text back in the composer to edit.
func (a *app) rewindLocked(it agent.Item) {
	sess, gen, err := a.liveSession()
	if err != nil {
		a.toastLocked("error", err.Error())
		return
	}
	rw, ok := sess.(agent.Rewinder)
	if !ok {
		a.toastLocked("error", "Rewinding is not supported")
		return
	}
	turn := firstNonEmpty(it.TurnID, it.ID)
	a.async("Rewind", func(ctx context.Context) error {
		if err := rw.Rewind(ctx, turn); err != nil {
			return err
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if gen == a.gen {
			a.composer.set(it.Text)
			a.changed()
		}
		return nil
	})
}

func (a *app) cmdModel(arg string) {
	if arg != "" {
		a.setModelLocked(arg)
		return
	}
	// Claude reports which models take auto mode; the shared list doesn't.
	auto := map[string]bool{}
	if cs, err := a.claudeSession(); err == nil {
		for _, m := range cs.SupportedModels() {
			auto[m.Value] = m.SupportsAutoMode
		}
	}
	var items []choice
	for _, m := range a.models {
		id := m.ID
		c := choice{Label: firstNonEmpty(m.DisplayName, m.ID), Detail: m.Description, Icon: "cpu",
			Current: m.ID == a.model || (m.ResolvedID != "" && m.ResolvedID == a.model), Run: func() { a.setModelLocked(id) }}
		if len(m.Efforts) > 0 {
			c.Badges = append(c.Badges, "effort")
		}
		if auto[m.ID] {
			c.Badges = append(c.Badges, "auto")
		}
		if m.IsDefault {
			c.Badges = append(c.Badges, "default")
		}
		items = append(items, c)
	}
	if _, isClaude := a.backend.(claudeBackend); len(items) == 0 && isClaude {
		for _, id := range []string{"claude-haiku-5-5", "claude-sonnet-5-5", "claude-opus-5-5", "claude-fable-5-1"} {
			items = append(items, choice{Label: id, Icon: "cpu", Current: id == a.model, Run: func() { a.setModelLocked(id) }})
		}
	}
	if len(items) == 0 {
		a.fetchModelsLocked(a.gen, a.sess)
		a.toastLocked("info", "The model list is still loading; try again in a moment, or /model <id>")
		return
	}
	a.openPickerLocked(&menu{title: "Model", subtitle: "for this conversation", noun: "models", icon: "cpu", items: items})
}

func (a *app) setModelLocked(model string) {
	sess, gen, err := a.liveSession()
	if err != nil {
		a.model, a.pending.model = model, model
		return
	}
	a.async("Model", func(ctx context.Context) error {
		if err := sess.SetModel(ctx, model); err != nil {
			return err
		}
		a.mu.Lock()
		if gen == a.gen {
			a.model = model
		}
		a.mu.Unlock()
		a.changed()
		return nil
	})
}

func (a *app) cmdEffort(arg string) {
	levels := a.effortLevelsLocked()
	if arg != "" {
		if !slices.Contains(levels, strings.ToLower(arg)) {
			a.toastLocked("error", fmt.Sprintf("%s takes effort %s", shortModel(a.model), strings.Join(levels, ", ")))
			return
		}
		a.setEffortLocked(agent.Effort(strings.ToLower(arg)))
		return
	}
	var items []choice
	for _, l := range levels {
		effort := agent.Effort(l)
		items = append(items, choice{Label: l, Icon: "brain", Current: effort == a.effort, Run: func() { a.setEffortLocked(effort) }})
	}
	a.openPickerLocked(&menu{title: "Reasoning effort", subtitle: shortModel(a.model), noun: "levels", icon: "brain", items: items})
}

func (a *app) setEffortLocked(effort agent.Effort) {
	sess, gen, err := a.liveSession()
	if err != nil {
		a.effort, a.pending.effort = effort, effort
		return
	}
	a.async("Effort", func(ctx context.Context) error {
		if err := sess.SetEffort(ctx, effort); err != nil {
			return err
		}
		a.mu.Lock()
		if gen == a.gen {
			a.effort = effort
		}
		a.mu.Unlock()
		a.changed()
		return nil
	})
}

func (a *app) cmdMode(arg string) {
	modes := a.modeListLocked()
	if arg != "" {
		if !slices.Contains(modes, agent.PermissionMode(arg)) {
			var names []string
			for _, m := range modes {
				names = append(names, string(m))
			}
			a.toastLocked("error", "Modes: "+strings.Join(names, ", "))
			return
		}
		a.setModeLocked(agent.PermissionMode(arg))
		return
	}
	var items []choice
	for _, m := range modes {
		mode := m
		items = append(items, choice{Label: modeLabel(mode), Detail: modeHelp[mode], Icon: "shield", Current: mode == a.mode,
			Run: func() { a.setModeLocked(mode) }})
	}
	a.openPickerLocked(&menu{title: "Permission mode", subtitle: "Shift+Tab cycles", noun: "modes", icon: "shield", items: items})
}

var modeHelp = map[agent.PermissionMode]string{
	agent.PermissionDefault:     "ask before risky actions",
	agent.PermissionAcceptEdits: "edit files without asking",
	agent.PermissionPlan:        "plan without changing anything",
	agent.PermissionBypass:      "run everything without asking",
	agent.PermissionDontAsk:     "deny whatever would ask",
	agent.PermissionAuto:        "a classifier approves each call",
}

func modeLabel(m agent.PermissionMode) string {
	switch m {
	case agent.PermissionDefault, "":
		return "ask before changes"
	case agent.PermissionAcceptEdits:
		return "accept edits"
	case agent.PermissionPlan:
		return "plan mode"
	case agent.PermissionBypass:
		return "bypass permissions"
	case agent.PermissionDontAsk:
		return "don't ask"
	case agent.PermissionAuto:
		return "auto"
	}
	return string(m)
}

func (a *app) cmdCompact(arg string) {
	sess, _, err := a.liveSession()
	if err != nil {
		a.toastLocked("error", err.Error())
		return
	}
	c, ok := sess.(agent.Compacter)
	if !ok {
		a.toastLocked("error", "Compacting is not supported")
		return
	}
	// No timeout: a queued /compact waits for the running turn, and
	// cancelling the context would interrupt the compaction.
	go func() {
		if err := c.Compact(a.ctx, arg); err != nil && !errors.Is(err, context.Canceled) {
			a.mu.Lock()
			a.toastLocked("error", "Compact: "+err.Error())
			a.mu.Unlock()
		}
	}()
}

func (a *app) cmdTitle(arg string) {
	if arg == "" {
		if _, err := a.claudeSession(); err == nil {
			a.generateTitleLocked()
			return
		}
		a.toastLocked("info", "Title: "+firstNonEmpty(a.info.Title, "(none)"))
		return
	}
	sess, gen, err := a.liveSession()
	if err != nil {
		a.toastLocked("error", err.Error())
		return
	}
	t, ok := sess.(agent.Titler)
	if !ok {
		a.toastLocked("error", "Titles are not supported")
		return
	}
	a.async("Title", func(ctx context.Context) error {
		if err := t.SetTitle(ctx, arg); err != nil {
			return err
		}
		a.mu.Lock()
		if gen == a.gen {
			a.info.Title = arg
		}
		a.mu.Unlock()
		a.changed()
		return nil
	})
}

func (a *app) cmdContext(string) { a.toggleContextLocked() }

func (a *app) cmdStatus(string) {
	var b strings.Builder
	row := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&b, "| %s | %s |\n", k, v)
		}
	}
	b.WriteString("| Session | `" + a.info.ID + "` |\n| --- | --- |\n")
	row("Title", a.info.Title)
	row("Directory", a.cwd)
	row("Model", a.model)
	row("Effort", string(a.effort))
	row("Permission mode", modeLabel(a.mode))
	row(a.backend.Product(), a.info.Version)
	if acct := a.accountLocked(); acct.known {
		row("Account", acct.label)
		row("Plan", acct.plan)
		row("Auth", acct.auth)
	}
	for _, s := range a.info.MCPServers {
		row("MCP "+s.Name, s.Status)
	}
	for _, l := range a.rateLimitsLocked() {
		row(limitName(l.Name)+" limit", fmt.Sprintf("%.0f%% used%s", l.UsedPercent, resetsIn(l.ResetsAt)))
	}
	a.addLocal(localCard{Title: "Status", Text: b.String()})
}

func (a *app) cmdCost(string) {
	text := fmt.Sprintf("**$%.4f** this conversation · context %s", a.costUSD, compact(a.contextTokens))
	if !a.backend.Provider().Capabilities().CostReporting {
		text = fmt.Sprintf("%s doesn't report cost · context %s", a.backend.Name(), compact(a.contextTokens))
	}
	if a.contextWindow > 0 {
		text += fmt.Sprintf(" of %s", compact(a.contextWindow))
	}
	a.addLocal(localCard{Title: "Cost", Text: text})
}

func (a *app) cmdMCP(arg string) {
	if cx, err := a.codexSession(); err == nil {
		if strings.TrimSpace(arg) != "" {
			a.toastLocked("info", "Manage Codex's MCP servers with codex mcp in a terminal")
			return
		}
		a.codexMCPLocked(cx)
		return
	}
	cs, err := a.claudeSession()
	if err != nil {
		a.toastLocked("error", err.Error())
		return
	}
	verb, name, _ := strings.Cut(arg, " ")
	name = strings.TrimSpace(name)
	switch verb {
	case "":
	case "login", "logout", "callback":
		a.mcpAuthLocked(verb, name)
		return
	case "reconnect", "enable", "disable":
		if name == "" {
			a.toastLocked("error", "Usage: /mcp "+verb+" <server>")
			return
		}
		a.async("MCP "+verb, func(ctx context.Context) error {
			var err error
			if verb == "reconnect" {
				err = cs.ReconnectMCPServer(ctx, name)
			} else {
				err = cs.ToggleMCPServer(ctx, name, verb == "enable")
			}
			if err != nil {
				return err
			}
			a.mu.Lock()
			a.toastLocked("success", map[string]string{"reconnect": "Reconnected ", "enable": "Enabled ", "disable": "Disabled "}[verb]+name)
			a.mu.Unlock()
			return nil
		})
		return
	default:
		a.toastLocked("error", "Usage: /mcp [reconnect|enable|disable|login|logout <server>]")
		return
	}
	gen := a.gen
	a.async("MCP", func(ctx context.Context) error {
		servers, err := cs.MCPServerStatus(ctx)
		if err != nil {
			return err
		}
		var b strings.Builder
		var choices []choice
		if len(servers) == 0 {
			b.WriteString("No MCP servers are configured.")
		}
		b.WriteString("| Server | Status | Tools | Scope |\n| --- | --- | ---: | --- |\n")
		for _, s := range servers {
			status := s.Status
			if s.Error != "" {
				status += ": " + s.Error
			}
			fmt.Fprintf(&b, "| %s | %s | %d | %s |\n", s.Name, status, len(s.Tools), s.Scope)
			n := s.Name
			choices = append(choices, choice{Label: "Reconnect " + n, Run: func() { a.cmdMCP("reconnect " + n) }})
			if s.Status == "disabled" {
				choices = append(choices, choice{Label: "Enable " + n, Run: func() { a.cmdMCP("enable " + n) }})
			} else {
				choices = append(choices, choice{Label: "Disable " + n, Run: func() { a.cmdMCP("disable " + n) }})
			}
		}
		a.mu.Lock()
		if gen == a.gen {
			a.addLocal(localCard{Title: "MCP servers", Text: b.String(), Choices: choices})
		}
		a.mu.Unlock()
		return nil
	})
}

func (a *app) cmdBtw(arg string) {
	if arg == "" {
		a.toastLocked("info", "Usage: /btw <question>")
		return
	}
	cs, err := a.claudeSession()
	if err != nil {
		a.toastLocked("error", err.Error())
		return
	}
	gen := a.gen
	a.async("Side question", func(ctx context.Context) error {
		answer, err := cs.SideQuestion(ctx, arg)
		if err != nil {
			return err
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if gen != a.gen {
			return nil
		}
		a.addLocal(localCard{Title: "Side question: " + firstLine(arg), Text: firstNonEmpty(answer, "_No answer._"), Tone: "info"})
		return nil
	})
}

func (a *app) cmdTasks(arg string) {
	if strings.TrimSpace(arg) == "clean" {
		cs, err := a.codexSession()
		if err != nil {
			a.toastLocked("info", "/tasks clean stops Codex's leftover terminals")
			return
		}
		a.async("Clean terminals", func(ctx context.Context) error {
			if err := cs.CleanBackgroundTerminals(ctx); err != nil {
				return err
			}
			a.mu.Lock()
			a.toastLocked("success", "Stopped the background terminals")
			a.mu.Unlock()
			return nil
		})
		return
	}
	// Codex also keeps processes that outlived their command; list them too.
	if cs, err := a.codexSession(); err == nil {
		gen := a.gen
		go func() {
			ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
			defer cancel()
			terms, err := cs.BackgroundTerminals(ctx)
			a.mu.Lock()
			defer a.mu.Unlock()
			if gen != a.gen {
				return
			}
			if err != nil {
				// The tasks are known here; show them anyway.
				a.logger.Warn("background terminals", "err", err)
			}
			a.openTasksLocked(terminalChoices(a, cs, terms), nil)
			a.changed()
		}()
		return
	}
	// Claude keeps each background command's output; show it in the preview.
	if cs, err := a.claudeSession(); err == nil {
		var ids []string
		for _, t := range a.sortedTasksLocked() {
			if t.Kind == agent.TaskShell && len(ids) < 8 {
				ids = append(ids, t.ID)
			}
		}
		if len(ids) > 0 {
			gen := a.gen
			go func() {
				ctx, cancel := context.WithTimeout(a.ctx, 5*time.Second)
				defer cancel()
				outputs := map[string]string{}
				for _, id := range ids {
					if raw, err := cs.TaskOutput(ctx, id); err == nil {
						var r struct {
							Output    string `json:"output"`
							Truncated bool   `json:"truncated"`
						}
						if json.Unmarshal(raw, &r) == nil && r.Output != "" {
							if r.Truncated {
								r.Output = "…\n" + r.Output
							}
							outputs[id] = r.Output
						}
					}
				}
				a.mu.Lock()
				defer a.mu.Unlock()
				if gen == a.gen {
					a.openTasksLocked(nil, outputs)
					a.changed()
				}
			}()
			return
		}
	}
	a.openTasksLocked(nil, nil)
}

// terminalChoices lists background terminals; Enter terminates one.
func terminalChoices(a *app, cs *codex.Session, terms []codex.BackgroundTerminal) []choice {
	var items []choice
	for _, t := range terms {
		term := t
		items = append(items, choice{Label: firstLine(term.Command), Detail: "running · Enter terminates it", Tone: "accent", Icon: "terminal", Mono: true,
			Facts: map[string]any{"kind": "terminal"}, Preview: "### Background terminal\n\n```sh\n" + term.Command + "\n```\n\nin `" + tildePath(term.Cwd) + "`",
			Run: func() {
				a.async("Terminate", func(ctx context.Context) error {
					if _, err := cs.TerminateBackgroundTerminal(ctx, term.ProcessID); err != nil {
						return err
					}
					a.mu.Lock()
					a.toastLocked("success", "Terminated "+firstLine(term.Command))
					a.mu.Unlock()
					return nil
				})
			}})
	}
	return items
}

// openTasksLocked shows the tasks, then extra (background terminals);
// outputs, by task, replace what the events carried.
func (a *app) openTasksLocked(extra []choice, outputs map[string]string) {
	var items []choice
	for _, t := range a.sortedTasksLocked() {
		task := t
		tone := map[agent.TaskStatus]string{agent.TaskRunning: "accent", agent.TaskCompleted: "success", agent.TaskFailed: "error", agent.TaskStopped: "muted"}[task.Status]
		var pv strings.Builder
		fmt.Fprintf(&pv, "### %s\n\n%s · %s", taskName(task), task.Kind, task.Status)
		if task.ToolUses > 0 {
			fmt.Fprintf(&pv, " · %s", plural(task.ToolUses, "tool call"))
		}
		if task.Duration > 0 {
			fmt.Fprintf(&pv, " · %s", fmtDuration(task.Duration))
		}
		if task.Description != "" {
			fmt.Fprintf(&pv, "\n\n%s", task.Description)
		}
		if out := firstNonEmpty(task.Error, outputs[task.ID], task.Output, task.Summary); out != "" {
			fmt.Fprintf(&pv, "\n\n```\n%s\n```", tail(out, 4000))
		}
		detail := task.Summary
		if task.Status == agent.TaskRunning {
			detail = firstNonEmpty(detail, "running · Enter stops it")
		}
		items = append(items, choice{Label: taskName(task), Detail: detail, Tone: tone, Icon: map[agent.TaskKind]string{agent.TaskShell: "terminal", agent.TaskSubagent: "users"}[task.Kind],
			Facts: map[string]any{"kind": string(task.Kind)}, Preview: pv.String(), Run: func() {
				if task.Status == agent.TaskRunning {
					a.stopTaskLocked(task.ID)
				}
			}})
	}
	items = append(items, extra...)
	a.openPickerLocked(&menu{title: "Background tasks", subtitle: "Enter stops a running one", noun: "tasks", icon: "activity",
		items: items, columns: []ui.PickerColumn{{ID: "kind", Format: ui.FormatDim}}})
}

func (a *app) stopTaskLocked(id string) {
	sess, _, err := a.liveSession()
	if err != nil {
		return
	}
	st, ok := sess.(agent.TaskStopper)
	if !ok {
		a.toastLocked("error", "Stopping tasks is not supported")
		return
	}
	a.async("Stop task", func(ctx context.Context) error { return st.StopTask(ctx, id) })
}

func (a *app) cmdPlan(string) {
	cs, err := a.claudeSession()
	if err != nil {
		a.toastLocked("error", err.Error())
		return
	}
	gen := a.gen
	a.async("Plan", func(ctx context.Context) error {
		p, err := cs.Plan(ctx)
		if err != nil {
			return err
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if gen != a.gen {
			return nil
		}
		if !p.Exists {
			a.toastLocked("info", "There is no plan yet")
			return nil
		}
		a.addLocal(localCard{Title: "Plan · " + p.Path, Text: p.Content, Tone: "info"})
		return nil
	})
}

func (a *app) cmdExport(string) {
	// Claude exports the conversation; Codex reads the thread with its turns.
	var read func(ctx context.Context) (json.RawMessage, error)
	if cs, err := a.claudeSession(); err == nil {
		read = cs.ExportConversation
	} else if cx, err := a.codexSession(); err == nil {
		read = func(ctx context.Context) (json.RawMessage, error) { return cx.ReadThread(ctx, true) }
	} else {
		a.toastLocked("error", err.Error())
		return
	}
	base := filepath.Join(a.cwd, strings.ToLower(a.backend.Name())+"-"+shortID(a.info.ID))
	a.async("Export", func(ctx context.Context) error {
		raw, err := read(ctx)
		if err != nil {
			return err
		}
		var out bytes.Buffer
		if json.Indent(&out, raw, "", "  ") != nil {
			out.Write(raw)
		}
		// Never overwrite: an earlier export or someone's file keeps its name.
		var f *os.File
		path := base + ".json"
		for n := 2; ; n++ {
			f, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if !errors.Is(err, fs.ErrExist) {
				break
			}
			path = fmt.Sprintf("%s-%d.json", base, n)
		}
		if err != nil {
			return err
		}
		if _, err := f.Write(out.Bytes()); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		a.mu.Lock()
		a.toastLocked("success", "Saved "+path)
		a.mu.Unlock()
		return nil
	})
}

func (a *app) sortedTasksLocked() []agent.Task {
	var ts []agent.Task
	for _, t := range a.tasks {
		if !t.Hidden {
			ts = append(ts, t)
		}
	}
	sort.Slice(ts, func(i, j int) bool {
		if (ts[i].Status == agent.TaskRunning) != (ts[j].Status == agent.TaskRunning) {
			return ts[i].Status == agent.TaskRunning
		}
		return ts[i].ID < ts[j].ID
	})
	return ts
}

func taskName(t agent.Task) string {
	if t.Subagent != nil {
		return firstNonEmpty(t.Subagent.Name, t.Subagent.Type, t.Description, "agent")
	}
	return firstNonEmpty(t.Description, string(t.Kind), t.ID)
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return firstNonEmpty(id, "session")
}

func compact(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

func relTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return t.Format("Jan 2")
}

func resetsIn(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return ", resets " + t.Local().Format("Jan 2 15:04")
}

// bypassAllowed reports whether the session may switch to bypassing
// permissions (-allow-bypass).
func bypassAllowed(cfg agent.Config) bool {
	opts, ok := cfg.ProviderOptions.(*claude.SessionOptions)
	return ok && opts.AllowBypassPermissions
}

// effortLevelsLocked lists the efforts the current model takes.
func (a *app) effortLevelsLocked() []string {
	if m, ok := a.modelLocked(); ok && len(m.Efforts) > 0 {
		var levels []string
		for _, e := range m.Efforts {
			levels = append(levels, string(e))
		}
		return levels
	}
	return []string{"low", "medium", "high", "xhigh", "max"}
}

// modeListLocked lists the permission modes the session accepts: auto only
// when the model supports it, and bypassing permissions only when the
// session started that way, since Claude Code refuses to switch to it.
func (a *app) modeListLocked() []agent.PermissionMode {
	modes := modeCycle
	if a.sess != nil {
		if m := a.sess.Capabilities().PermissionModes; len(m) > 0 {
			modes = m
		}
	}
	return slices.DeleteFunc(slices.Clone(modes), func(m agent.PermissionMode) bool {
		return m == agent.PermissionAuto && !a.autoModeLocked() ||
			m == agent.PermissionBypass && a.base.PermissionMode != agent.PermissionBypass && !bypassAllowed(a.base)
	})
}
