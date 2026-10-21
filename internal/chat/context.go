package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/stencil-hq/tern-sdk/go/tern"
	"github.com/stencil-hq/tern-sdk/go/tern/ui"
)

// contextUsage is Claude Code's context breakdown, as its get_context_usage
// control request reports it (the data behind its own /context). It is an
// estimate the CLI computes locally, so refreshing it costs no API call.
type contextUsage struct {
	TotalTokens          int64   `json:"totalTokens"`
	MaxTokens            int64   `json:"maxTokens"`
	Percentage           float64 `json:"percentage"`
	Model                string  `json:"model"`
	AutoCompactThreshold int64   `json:"autoCompactThreshold"`
	IsAutoCompactEnabled bool    `json:"isAutoCompactEnabled"`
	Categories           []struct {
		Name   string `json:"name"`
		Tokens int64  `json:"tokens"`
		Kind   string `json:"kind"` // used, deferred, buffer, free
	} `json:"categories"`
	MemoryFiles []struct {
		Path   string `json:"path"`
		Type   string `json:"type"`
		Tokens int64  `json:"tokens"`
	} `json:"memoryFiles"`
	MCPTools []struct {
		Name       string `json:"name"`
		ServerName string `json:"serverName"`
		Tokens     int64  `json:"tokens"`
	} `json:"mcpTools"`
	Agents []struct {
		AgentType string `json:"agentType"`
		Source    string `json:"source"`
		Tokens    int64  `json:"tokens"`
	} `json:"agents"`
	Skills struct {
		TotalSkills int64 `json:"totalSkills"`
		Tokens      int64 `json:"tokens"`
		Frontmatter []struct {
			Name   string `json:"name"`
			Source string `json:"source"`
			Tokens int64  `json:"tokens"`
		} `json:"skillFrontmatter"`
	} `json:"skills"`
	SlashCommands struct {
		TotalCommands int64 `json:"totalCommands"`
		Tokens        int64 `json:"tokens"`
	} `json:"slashCommands"`
	MessageBreakdown struct {
		ToolCallTokens         int64 `json:"toolCallTokens"`
		ToolResultTokens       int64 `json:"toolResultTokens"`
		AttachmentTokens       int64 `json:"attachmentTokens"`
		AssistantMessageTokens int64 `json:"assistantMessageTokens"`
		UserMessageTokens      int64 `json:"userMessageTokens"`
	} `json:"messageBreakdown"`
}

// usedTokens color the categories that fill the window, in the CLI's
// order, the same way in the grid, the bar and the legend. The reply names
// rows for display only, so color goes by position.
var usedTokens = []string{"accent", "info", "success", "pending", "error", "warning", "user"}

// refreshContextLocked asks Claude Code for a fresh breakdown, at most one
// request at a time.
func (a *app) refreshContextLocked() {
	cs, err := a.claudeSession()
	if err != nil || a.ctxBusy {
		return
	}
	a.ctxBusy = true
	gen := a.gen
	go func() {
		ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
		defer cancel()
		raw, err := cs.ContextUsage(ctx, false)
		var u contextUsage
		if err == nil {
			err = json.Unmarshal(raw, &u)
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		a.ctxBusy = false
		if gen != a.gen || err != nil || u.MaxTokens == 0 {
			return
		}
		a.ctxUsage, a.ctxAt = &u, time.Now()
		a.contextTokens, a.contextWindow = u.TotalTokens, u.MaxTokens
		a.changed()
	}()
}

// contextLoop keeps the numbers live: every few seconds while Claude works
// or the viewer is open.
func (a *app) contextLoop() {
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-tick.C:
		}
		a.mu.Lock()
		if a.showContext || a.running() {
			a.refreshContextLocked()
		}
		a.mu.Unlock()
	}
}

func (a *app) toggleContextLocked() {
	if !a.backend.Has(featContextBreakdown) {
		// The agent reports totals only; the ring already shows them.
		if a.contextWindow == 0 {
			a.toastLocked("info", "No context use reported yet")
		} else {
			a.toastLocked("info", "Context: "+a.contextHint())
		}
		return
	}
	a.showContext = !a.showContext
	if a.showContext {
		a.refreshContextLocked()
	}
	a.changed()
}

// contextNode is the live viewer: a grid and a bar of what fills the
// window, a legend, and folded details, above the composer.
func (a *app) contextNode() tern.Element {
	u := a.ctxUsage
	var body []tern.Element
	if u == nil {
		body = append(body, ui.Text{Common: ui.Common{Key: "wait"}, Spans: []ui.Span{{T: "Measuring the context…", S: "muted"}}})
	} else {
		body = a.contextBody(u)
	}
	return ui.Overlay{Common: ui.Common{Key: "context", Role: "agent.context"}, Anchor: ui.AtNode("dock.composer", "above"),
		Size: ui.SizeLG, Head: ui.Spans(ui.Span{T: "Context", S: "strong"}, ui.Span{T: "  live · Esc closes", S: "muted"}),
		Children: body}
}

func (a *app) contextBody(u *contextUsage) []tern.Element {
	used := float64(u.TotalTokens) / float64(u.MaxTokens)
	var parts []ui.Part
	var legend []tern.Element
	nused := 0
	for i, c := range u.Categories {
		if c.Kind == "deferred" || c.Tokens == 0 {
			continue
		}
		share := float64(c.Tokens) / float64(u.MaxTokens)
		token := "muted"
		if c.Kind == "used" {
			token = usedTokens[nused%len(usedTokens)]
			nused++
		}
		part := ui.Part{Value: share, Token: token, Label: c.Name, Hatch: c.Kind == "buffer"}
		if c.Kind == "free" {
			part.Token = "track"
		}
		parts = append(parts, part)
		dot := "●"
		if c.Kind == "free" {
			dot, token = "○", "muted"
		}
		legend = append(legend, ui.Row{Common: ui.Common{Key: fmt.Sprint("c", i), Role: "agent.context.row"}, Align: ui.AlignCenter, Gap: ui.GapMD, Children: ui.Nodes(
			ui.Text{Spans: []ui.Span{{T: dot + " ", S: token}, {T: c.Name}}, Common: ui.Common{Grow: 1}},
			ui.Text{Common: ui.Common{Role: "agent.context.tokens"}, Spans: []ui.Span{{T: compact(c.Tokens)}}},
			ui.Text{Common: ui.Common{Role: "agent.context.pct"}, Spans: []ui.Span{{T: fmt.Sprintf("%.1f%%", share*100), S: "muted"}}},
		)})
	}
	var marks []ui.MeterMark
	if u.IsAutoCompactEnabled && u.AutoCompactThreshold > 0 {
		marks = []ui.MeterMark{{At: float64(u.AutoCompactThreshold) / float64(u.MaxTokens), Tone: ui.ToneWarning, Title: "Auto-compact at " + compact(u.AutoCompactThreshold)}}
	}
	summary := []ui.Span{
		{T: compact(u.TotalTokens), S: "strong"}, {T: " of " + compact(u.MaxTokens) + " tokens", S: "muted"},
		{T: fmt.Sprintf("  ·  %.0f%% used", used*100), S: "muted"},
	}
	if u.Model != "" {
		summary = append(summary, ui.Span{T: "   " + shortModel(u.Model), S: "muted"})
	}
	body := []tern.Element{
		ui.Text{Common: ui.Common{Key: "summary", Role: "agent.context.summary"}, Spans: summary},
		ui.Row{Common: ui.Common{Key: "viz"}, Gap: ui.GapLG, Align: ui.AlignStart, Children: ui.Nodes(
			ui.Meter{Common: ui.Common{Key: "grid"}, Style: ui.MeterBlocks, Size: ui.SizeLG, Parts: parts},
			ui.Col{Common: ui.Common{Key: "legend", Grow: 1}, Gap: ui.GapXS, Children: legend},
		)},
		ui.Meter{Common: ui.Common{Key: "bar"}, Size: ui.SizeMD, Parts: parts, Marks: marks},
	}
	if len(marks) > 0 {
		left := u.AutoCompactThreshold - u.TotalTokens
		body = append(body, ui.Text{Common: ui.Common{Key: "auto"}, Spans: []ui.Span{
			{T: "Auto-compacts at " + compact(u.AutoCompactThreshold), S: "muted"},
			{T: fmt.Sprintf(" · %s to go", compact(max(left, 0))), S: "muted"},
		}})
	}
	body = append(body, a.contextDetails(u)...)
	body = append(body, actionsRow("act", []tern.Element{
		button("compact", "Compact now", "", func() { a.locked(func() { a.showContext = false; a.cmdCompact("") }) }),
		button("close", "Close", "", func() { a.locked(func() { a.showContext = false }) }),
	}))
	if !a.ctxAt.IsZero() {
		body = append(body, ui.Text{Common: ui.Common{Key: "at"}, Spans: []ui.Span{{T: "Updated " + a.ctxAt.Format("15:04:05"), S: "dim"}}})
	}
	return body
}

// contextDetails are folded sections naming what each category holds.
func (a *app) contextDetails(u *contextUsage) []tern.Element {
	type row struct {
		name, detail string
		tokens       int64
	}
	section := func(key, title string, total int64, rows []row) tern.Element {
		sort.Slice(rows, func(i, j int) bool { return rows[i].tokens > rows[j].tokens })
		var kids []tern.Element
		for i, r := range rows[:min(len(rows), 12)] {
			kids = append(kids, ui.Row{Common: ui.Common{Key: fmt.Sprint("r", i), Role: "agent.context.row"}, Children: ui.Nodes(
				ui.Text{Common: ui.Common{Grow: 1, Title: r.detail}, Spans: []ui.Span{{T: r.name}, {T: "  " + r.detail, S: "muted"}}},
				ui.Text{Common: ui.Common{Role: "agent.context.tokens"}, Spans: []ui.Span{{T: compact(r.tokens), S: "muted"}}},
			)})
		}
		if n := len(rows) - 12; n > 0 {
			kids = append(kids, ui.Text{Common: ui.Common{Key: "more"}, Spans: []ui.Span{{T: fmt.Sprintf("… %d more", n), S: "muted"}}})
		}
		return ui.Section{Common: ui.Common{Key: key}, Collapsible: true, Collapsed: true,
			Head: ui.Spans(ui.Span{T: title}, ui.Span{T: fmt.Sprintf("  %d · %s", len(rows), compact(total)), S: "muted"}), Children: kids}
	}
	var out []tern.Element
	if len(u.MemoryFiles) > 0 {
		var rows []row
		var total int64
		for _, f := range u.MemoryFiles {
			rows, total = append(rows, row{tildePath(f.Path), f.Type, f.Tokens}), total+f.Tokens
		}
		out = append(out, section("memory", "Memory files", total, rows))
	}
	if len(u.Skills.Frontmatter) > 0 {
		var rows []row
		for _, s := range u.Skills.Frontmatter {
			rows = append(rows, row{s.Name, s.Source, s.Tokens})
		}
		out = append(out, section("skills", "Skills", u.Skills.Tokens, rows))
	}
	if len(u.Agents) > 0 {
		var rows []row
		var total int64
		for _, ag := range u.Agents {
			rows, total = append(rows, row{ag.AgentType, ag.Source, ag.Tokens}), total+ag.Tokens
		}
		out = append(out, section("agents", "Custom agents", total, rows))
	}
	if len(u.MCPTools) > 0 {
		var rows []row
		var total int64
		for _, t := range u.MCPTools {
			rows, total = append(rows, row{t.Name, t.ServerName, t.Tokens}), total+t.Tokens
		}
		out = append(out, section("mcp", "MCP tools", total, rows))
	}
	mb := u.MessageBreakdown
	msgs := []row{
		{"Tool calls", "", mb.ToolCallTokens}, {"Tool results", "", mb.ToolResultTokens},
		{"Your messages", "", mb.UserMessageTokens}, {a.backend.Name() + "'s replies", "", mb.AssistantMessageTokens},
		{"Attachments", "", mb.AttachmentTokens},
	}
	var total int64
	kept := msgs[:0]
	for _, m := range msgs {
		if m.tokens > 0 {
			kept, total = append(kept, m), total+m.tokens
		}
	}
	if len(kept) > 0 {
		out = append(out, section("messages", "Messages", total, kept))
	}
	return out
}
