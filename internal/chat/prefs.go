package chat

import (
	"context"
	"fmt"
	"slices"
	"strings"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/stencil-hq/tern-sdk/go/tern"
	"github.com/stencil-hq/tern-sdk/go/tern/ui"
)

// prefsState is the open /config sheet: Tern's native settings page,
// docked at the pane's side so the conversation stays visible beside it.
type prefsState struct {
	page    string
	focus   string // the row with the keyboard focus
	editing string // the choice row whose menu is open
	option  string // the option under the keyboard while editing
}

// thinkingBudgets are the limits /config offers; 0 is the model's default.
var thinkingBudgets = []struct {
	value  string
	tokens int
	label  string
}{{"default", 0, "Default"}, {"4k", 4096, "4k"}, {"16k", 16384, "16k"}, {"32k", 32768, "32k"}}

func (a *app) cmdConfig(string) {
	a.prefs = &prefsState{page: "session"}
	a.changed()
}

// prefsPagesLocked are the sheet's pages, grouped under the agent.
func (a *app) prefsPagesLocked() []ui.PrefsPage {
	name := a.backend.Name()
	return []ui.PrefsPage{
		{ID: "session", Label: "Session", Group: name, Icon: ui.IconName("app-" + strings.ToLower(name))},
		{ID: "interface", Label: "Interface", Group: name, Icon: "sliders"},
		{ID: "keys", Label: "Keyboard", Group: "Help", Icon: "keyboard"},
		{ID: "about", Label: "About", Group: "Help", Icon: "info"},
	}
}

// prefsPageLocked is a page's lead and sections.
func (a *app) prefsPageLocked(page string) (string, []ui.PrefsSection) {
	switch page {
	case "session":
		return "Applies to this conversation now. New conversations start from your " + a.backend.Name() + " settings.", a.sessionPrefs()
	case "interface":
		return "How " + a.backend.Command() + " shows the conversation.", a.interfacePrefs()
	case "keys":
		return "", keyPrefs(a.backend.Name())
	case "about":
		return "", a.aboutPrefs()
	}
	return "", nil
}

// prefsActions run a PrefAction; each closes the sheet first.
func (a *app) prefsActLocked(act string) {
	run := map[string]func(){
		"model":   func() { a.cmdModel("") },
		"compact": func() { a.cmdCompact("") },
		"context": a.toggleContextLocked,
		"mcp":     func() { a.cmdMCP("") },
	}[act]
	if run != nil {
		a.prefs = nil
		run()
	}
}

func (a *app) prefsNode() tern.Element {
	p := a.prefs
	lead, sections := a.prefsPageLocked(p.page)
	var editing *ui.PrefsEditing
	if p.editing != "" {
		editing = &ui.PrefsEditing{Row: p.editing, Option: p.option}
	}
	// Events can arrive after the sheet closed; each handler checks.
	open := func(fn func(p *prefsState)) {
		a.locked(func() {
			if a.prefs != nil {
				fn(a.prefs)
			}
		})
	}
	actions := map[string]func(*tern.Action){
		"page": func(e *tern.Action) {
			open(func(p *prefsState) { p.page, p.editing, p.focus = e.Value, "", "" })
		},
		"close": func(e *tern.Action) {
			open(func(p *prefsState) {
				if e.Value != "" {
					p.editing = ""
				} else {
					a.prefs = nil
				}
			})
		},
	}
	for _, act := range []string{"model", "compact", "context", "mcp"} {
		actions[act] = func(*tern.Action) { open(func(*prefsState) { a.prefsActLocked(act) }) }
	}
	return ui.Prefs{
		CommonWithoutTitle: ui.CommonWithoutTitle{Key: "prefs",
			OnChange: func(e *tern.Change) { open(func(*prefsState) { a.prefsChangeLocked(e.Item, e.Value) }) },
			OnActivate: func(e *tern.Activate) {
				open(func(p *prefsState) { p.editing, p.focus, p.option = e.Item, e.Item, a.prefsChoiceValue(e.Item) })
			},
			OnAction: actions},
		Title: a.backend.Name(), Pages: a.prefsPagesLocked(), Page: p.page, Lead: lead, Sections: sections, Focus: p.focus, Editing: editing,
	}
}

func (a *app) sessionPrefs() []ui.PrefsSection {
	var effort []ui.PrefOption
	effort = append(effort, ui.PrefOption{Value: "", Label: "Default"})
	for _, l := range a.effortLevelsLocked() {
		effort = append(effort, ui.PrefOption{Value: l, Label: l})
	}
	var modes []ui.PrefOption
	for _, m := range a.modeListLocked() {
		modes = append(modes, ui.PrefOption{Value: string(m), Label: modeLabel(m), Detail: modeHelp[m]})
	}
	var budgets []ui.PrefOption
	for _, b := range thinkingBudgets {
		budgets = append(budgets, ui.PrefOption{Value: b.value, Label: b.label})
	}
	modelRows := []ui.PrefsRow{
		{ID: "model", Label: "Model", Hint: firstNonEmpty(a.model, "from your "+a.backend.Name()+" settings"), Control: ui.PrefAction{Act: "model", Label: "Change…"}},
		{ID: "effort", Label: "Reasoning effort", Hint: "How hard the model thinks before answering",
			Control: ui.PrefChoice{Value: string(a.effort), Options: effort}},
	}
	if a.backend.Has(featThinkingBudget) {
		modelRows = append(modelRows, ui.PrefsRow{ID: "thinking", Label: "Thinking budget", Hint: "The most tokens " + a.backend.Name() + " spends thinking per reply",
			Control: ui.PrefChoice{Value: a.thinkingBudget, Options: budgets}})
	}
	return []ui.PrefsSection{
		{ID: "model", Title: "Model", Rows: modelRows},
		{ID: "permissions", Title: "Permissions", Rows: []ui.PrefsRow{
			{ID: "mode", Label: "Permission mode", Hint: "Shift+Tab cycles it from the composer",
				Control: ui.PrefChoice{Value: string(a.mode), Options: modes, Style: ui.ChoiceStyle("menu")}},
		}},
		{ID: "context", Title: "Context", Rows: []ui.PrefsRow{
			{ID: "context", Label: "Context window", Hint: a.contextHint(), Control: ui.PrefAction{Act: "context", Label: "Show…"}},
			{ID: "compact", Label: "Compact", Hint: "Summarize the conversation to free context", Control: ui.PrefAction{Act: "compact", Label: "Compact now"}},
			{ID: "mcp", Label: "MCP servers", Hint: fmt.Sprintf("%d configured", len(a.info.MCPServers)), Control: ui.PrefAction{Act: "mcp", Label: "Manage…"}},
		}},
	}
}

func (a *app) contextHint() string {
	if a.contextWindow > 0 {
		return fmt.Sprintf("%s of %s tokens used", compact(a.contextTokens), compact(a.contextWindow))
	}
	return "What fills the context, live"
}

func (a *app) interfacePrefs() []ui.PrefsSection {
	rows := []ui.PrefsRow{
		{ID: "expand", Label: "Expand tool calls", Hint: "Show every tool's output and " + a.backend.Name() + "'s thinking unfolded (Ctrl+O)",
			Control: ui.PrefSwitch{On: a.expandAll}, Changed: a.expandAll, DefaultLabel: "off"},
		{ID: "plan", Label: "Full todo list", Hint: "Show the whole list above the composer instead of the pill (Ctrl+T)",
			Control: ui.PrefSwitch{On: a.showPlan}, Changed: a.showPlan, DefaultLabel: "off"},
	}
	if a.backend.Has(featPromptSuggestions) {
		rows = append(rows, ui.PrefsRow{ID: "suggest", Label: "Prompt suggestions", Hint: a.backend.Name() + " predicts your next message as ghost text; Tab accepts it",
			Control: ui.PrefSwitch{On: !a.suggestPaused}, Changed: a.suggestPaused, DefaultLabel: "on"})
	}
	return []ui.PrefsSection{{ID: "display", Title: "Display", Rows: rows}}
}

func keyPrefs(name string) []ui.PrefsSection {
	row := func(id, label, hint string, chords ...string) ui.PrefsRow {
		var keys [][]string
		for _, c := range chords {
			keys = append(keys, strings.Split(c, "+"))
		}
		return ui.PrefsRow{ID: id, Label: label, Hint: hint, Control: ui.PrefKeys{Keys: keys}}
	}
	return []ui.PrefsSection{
		{ID: "compose", Title: "Composer", Rows: []ui.PrefsRow{
			row("send", "Send", "While "+name+" works the message is queued", "enter"),
			row("steer", "Steer the running turn", "", "ctrl+enter"),
			row("newline", "New line", "", "shift+enter", "ctrl+j"),
			row("complete", "Complete a command or file", "Also accepts a suggestion", "tab"),
			row("history", "Earlier messages", "On the first or last line", "up", "down"),
			row("paste", "Attach a screenshot", "Drop files on the pane to attach them", "ctrl+v"),
			row("undo", "Undo, redo", "", "ctrl+z", "ctrl+y"),
		}},
		{ID: "control", Title: "Conversation", Rows: []ui.PrefsRow{
			row("interrupt", "Interrupt", "", "esc"),
			row("rewind", "Rewind", "On an empty composer", "esc", "esc"),
			row("mode", "Cycle permission mode", "", "shift+tab"),
			row("expand", "Expand tool calls", "", "ctrl+o"),
			row("todo", "Full todo list", "", "ctrl+t"),
			row("quit", "Quit", "Press twice", "ctrl+c"),
		}},
		{ID: "requests", Title: "Requests", Rows: []ui.PrefsRow{
			row("allow", "Allow, for the session, deny", "Or type why and press Enter to deny", "1", "2", "3"),
			row("answer", "Pick an answer", "Space toggles in a multiple choice", "1", "2", "3"),
		}},
	}
}

func (a *app) aboutPrefs() []ui.PrefsSection {
	info := func(id, label, value string) ui.PrefsRow {
		return ui.PrefsRow{ID: id, Label: label, Hint: firstNonEmpty(value, "—"), Control: ui.PrefAction{Act: "noop", Label: "·"}}
	}
	rows := []ui.PrefsRow{
		info("session", "Session", a.info.ID),
		info("title", "Title", a.info.Title),
		info("cwd", "Directory", a.cwd),
		info("version", a.backend.Product(), a.info.Version),
	}
	if acct := a.accountLocked(); acct.known {
		rows = append(rows, info("account", "Account", acct.label), info("plan", "Plan", acct.plan))
	}
	for _, l := range a.rateLimitsLocked() {
		rows = append(rows, info("limit-"+l.Name, limitName(l.Name)+" limit", fmt.Sprintf("%.0f%% used%s", l.UsedPercent, resetsIn(l.ResetsAt))))
	}
	return []ui.PrefsSection{{ID: "about", Title: "This conversation", Rows: rows}}
}

func (a *app) prefsChangeLocked(row string, value any) {
	a.prefs.editing = ""
	str, _ := value.(string)
	on, _ := value.(bool)
	switch row {
	case "effort":
		a.setEffortLocked(agent.Effort(str))
	case "mode":
		if str != "" {
			a.setModeLocked(agent.PermissionMode(str))
		}
	case "thinking":
		a.setThinkingLocked(str)
	case "expand":
		a.expandAll = on
	case "plan":
		a.showPlan = on
	case "suggest":
		a.suggestPaused = !on
		if !on {
			a.suggestion = ""
		}
		if cs, err := a.claudeSession(); err == nil {
			a.async("Prompt suggestions", func(ctx context.Context) error { return cs.SetPromptSuggestionsPaused(ctx, !on) })
		}
	}
}

func (a *app) setThinkingLocked(value string) {
	cs, err := a.claudeSession()
	if err != nil {
		a.toastLocked("error", err.Error())
		return
	}
	var tokens *int
	for _, b := range thinkingBudgets {
		if b.value == value && b.tokens > 0 {
			t := b.tokens
			tokens = &t
		}
	}
	a.thinkingBudget = value
	a.async("Thinking budget", func(ctx context.Context) error { return cs.SetMaxThinkingTokens(ctx, tokens, "") })
}

// prefsRowsLocked lists the current page's rows in order.
func (a *app) prefsRowsLocked() []ui.PrefsRow {
	_, sections := a.prefsPageLocked(a.prefs.page)
	var rows []ui.PrefsRow
	for _, sec := range sections {
		rows = append(rows, sec.Rows...)
	}
	return rows
}

// prefsChoiceValue is the current value of a choice row.
func (a *app) prefsChoiceValue(row string) string {
	for _, r := range a.prefsRowsLocked() {
		if c, ok := r.Control.(ui.PrefChoice); ok && r.ID == row {
			return c.Value
		}
	}
	return ""
}

// prefsKey drives the sheet from the keyboard: ↑↓ move between rows,
// ←→ and Tab between pages, Enter toggles, opens a choice or runs an
// action; in an open choice ↑↓ pick and Enter applies.
func (a *app) prefsKey(k tern.Key) {
	p := a.prefs
	rows := a.prefsRowsLocked()
	if p.editing != "" {
		var opts []ui.PrefOption
		for _, r := range rows {
			if c, ok := r.Control.(ui.PrefChoice); ok && r.ID == p.editing {
				opts = c.Options
			}
		}
		i := slices.IndexFunc(opts, func(o ui.PrefOption) bool { return o.Value == p.option })
		switch {
		case k.Is("escape"), k.Is("ctrl+c"):
			p.editing = ""
		case (k.Is("up") || k.Is("left")) && len(opts) > 0:
			p.option = opts[max(i-1, 0)].Value
		case (k.Is("down") || k.Is("right")) && len(opts) > 0:
			p.option = opts[min(i+1, len(opts)-1)].Value
		case k.Is("enter"), k.Is("space"):
			a.prefsChangeLocked(p.editing, p.option)
		}
		return
	}
	at := slices.IndexFunc(rows, func(r ui.PrefsRow) bool { return r.ID == p.focus })
	switch {
	case k.Is("escape"), k.Is("ctrl+c"):
		a.prefs = nil
	case k.Is("tab"), k.Is("right"):
		p.page, p.focus = nextPage(p.page, 1), ""
	case k.Is("shift+tab"), k.Is("left"):
		p.page, p.focus = nextPage(p.page, -1), ""
	case k.Is("down") && len(rows) > 0:
		p.focus = rows[min(at+1, len(rows)-1)].ID
	case k.Is("up") && len(rows) > 0:
		p.focus = rows[max(at-1, 0)].ID
	case (k.Is("enter") || k.Is("space")) && at >= 0:
		switch c := rows[at].Control.(type) {
		case ui.PrefSwitch:
			a.prefsChangeLocked(rows[at].ID, !c.On)
		case ui.PrefChoice:
			p.editing, p.option = rows[at].ID, c.Value
		case ui.PrefAction:
			a.prefsActLocked(c.Act)
		}
	}
}

func nextPage(page string, dir int) string {
	pages := []string{"session", "interface", "keys", "about"}
	for i, p := range pages {
		if p == page {
			return pages[(i+dir+len(pages))%len(pages)]
		}
	}
	return pages[0]
}
