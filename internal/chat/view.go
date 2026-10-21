package chat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/stencil-hq/tern-sdk/go/tern"
	"github.com/stencil-hq/tern-sdk/go/tern/ui"
)

// view builds the whole surface from the state. Caller holds mu.
func (a *app) view() tern.View {
	return tern.View{Main: a.mainRegion(), Dock: a.dockRegion(), Layer: a.layerRegion()}
}

// mainRegion lays the transcript out the way Tern's chat skin expects:
// each user message a card, and everything Claude does until the next one
// grouped into one assistant turn, closed by its error and usage footer.
func (a *app) mainRegion() tern.Nodes {
	var nodes tern.Nodes
	var turn []tern.Element
	var reads []string       // consecutive finished reads, drawn as one row
	var mark ui.Mark         // while rewinding: pick from the chosen message, drop after it
	var later []tern.Element // command cards run during a turn, drawn after it
	turnKey, userID := "", ""
	flushReads := func() {
		switch len(reads) {
		case 0:
		case 1:
			turn = append(turn, a.entries[reads[0]].node)
		default:
			turn = append(turn, a.readGroupNode(reads))
		}
		reads = nil
	}
	flush := func() {
		flushReads()
		if userID != "" {
			if f, ok := a.turnFails[a.turnOf(userID)]; ok {
				turn = append(turn, a.failureNode(f, userID))
			}
			if u, ok := a.turnUsage[a.turnOf(userID)]; ok && len(turn) > 0 {
				turn = append(turn, usageFooter(u))
			}
			if turnKey == "" && len(turn) > 0 {
				turnKey = "turn" + entryKey(userID)
			}
		}
		if len(turn) > 0 {
			nodes = append(nodes, ui.Col{Common: ui.Common{Key: turnKey, Role: "omp.assistant", Mark: mark}, Children: turn})
		}
		nodes = append(nodes, later...)
		turn, turnKey, later = nil, "", nil
	}
	for i, id := range a.order {
		if i == a.resumedAt {
			flush()
			nodes = append(nodes, ui.Rule{Common: ui.Common{Key: "resumed", Role: "agent.resumed"}, Label: ui.T("Resumed")})
		}
		e := a.entries[id]
		if e == nil {
			continue
		}
		if e.node == nil || e.built != e.ver || e.expanded != a.expandAll {
			e.node, e.built, e.expanded = a.entryNode(id, e), e.ver, a.expandAll
		}
		if e.local == nil && e.item.Kind == agent.ItemUserMessage && a.queued[id] {
			continue // shown in the queue strip until it runs
		}
		if e.local == nil && e.item.Kind == agent.ItemUserMessage {
			flush()
			userID = id
			if a.rewind != nil {
				switch target := a.rewindTarget(); {
				case id == target:
					nodes = append(nodes, a.rewindHere())
					mark = ui.MarkPick
				case mark == ui.MarkPick:
					mark = ui.MarkDrop
				}
			}
			if e.node != nil {
				nodes = append(nodes, withMark(e.node, mark))
			}
			continue
		}
		if e.node == nil {
			continue
		}
		if e.local != nil {
			if userID == "" {
				nodes = append(nodes, withMark(e.node, mark))
			} else {
				later = append(later, withMark(e.node, mark))
			}
			continue
		}
		if turnKey == "" {
			turnKey = "turn" + entryKey(id)
		}
		if isGroupableRead(e.item) {
			reads = append(reads, id)
			continue
		}
		flushReads()
		turn = append(turn, e.node)
	}
	flush()
	if len(nodes) == 0 {
		nodes = append(nodes, a.welcome())
	}
	return nodes
}

// usageFooter is the quiet line under a finished turn, as omp draws it.
func usageFooter(u turnUsage) tern.Element {
	var parts, long []string
	if u.duration > 0 {
		d := fmtDuration(u.duration)
		parts, long = append(parts, d), append(long, d)
	}
	n := u.input + u.output
	parts = append(parts, compact(n)+" tok")
	long = append(long, fmt.Sprintf("%d tokens (%d in · %d out)", n, u.input, u.output))
	if u.cost > 0 {
		c := fmt.Sprintf("$%.2f", u.cost)
		if u.cost < 0.005 {
			c = "<$0.01"
		}
		parts, long = append(parts, c), append(long, c)
	}
	text := strings.Join(parts, " · ") + " this turn"
	if u.stopped != "" {
		text += " · " + u.stopped
	}
	return ui.Row{Common: ui.Common{Key: "turn-usage", Role: "omp.turn.usage", Title: "This turn: " + strings.Join(long, " · ")},
		Gap: ui.GapXS, Align: ui.AlignCenter, Children: ui.Nodes(ui.Text{Text: text})}
}

func fmtDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
}

// failureNode closes a turn that failed or was interrupted.
func (a *app) failureNode(f turnFailure, userID string) tern.Element {
	if f.aborted {
		return ui.Text{Common: ui.Common{Key: "error", Role: "omp.assistant.abort"}, Spans: []ui.Span{{T: "Interrupted", S: "error"}}}
	}
	head := []tern.Element{ui.Text{Spans: []ui.Span{{T: "Request failed", S: "error strong"}}}}
	if f.code != "" {
		head = append(head, ui.Badge{Common: ui.Common{Role: "omp.error.code", Tone: ui.ToneError}, Text: f.code})
	}
	action := func(key, label, title string, fn func()) tern.Element {
		return ui.Row{Common: ui.Common{Key: key, Role: "omp.error.action", Title: title, OnClick: func(*tern.Action) { a.locked(fn) }},
			Gap: ui.GapXS, Children: ui.Nodes(ui.Text{Text: label})}
	}
	retry := func() {
		if e := a.entries[userID]; e != nil {
			a.sendLocked(e.item.Text, agent.DeliverQueue)
		}
	}
	return ui.Card{Common: ui.Common{Key: "error", Role: "omp.error", Tone: ui.ToneError}, Children: ui.Nodes(
		ui.Row{Common: ui.Common{Key: "head"}, Gap: ui.GapSM, Children: head},
		ui.Text{Common: ui.Common{Key: "message", Role: "omp.error.message", Actions: &ui.Actions{Menu: []string{"copy"}}},
			Spans: []ui.Span{{T: firstNonEmpty(f.message, "Unknown error") + resetsIn(f.resetsAt), S: "mono"}}, Wrap: "word"},
		ui.Row{Common: ui.Common{Key: "actions", Role: "omp.error.actions"}, Gap: ui.GapSM, Children: ui.Nodes(
			action("retry", "Retry", "Send the message again", retry),
			action("switch-model", "Switch model", "Pick another model", func() { a.cmdModel("") }),
		)},
	)}
}

// workingNode is the live line above the composer while Claude works, as
// omp's loader draws it: spinner or retry countdown, elapsed, label.
func (a *app) workingNode() tern.Element {
	if !a.running() || a.firstRequestLocked() != nil {
		return nil
	}
	label := "Thinking…"
	if a.hookRunning != "" {
		label = "Running hook " + a.hookRunning
	} else if t := a.runningToolLocked(); t != nil {
		look := lookOf(t, a.cwd)
		label = firstNonEmpty(look.Intent, strings.TrimSpace(look.Title+" "+look.Target))
		if r := []rune(label); len(r) > 60 {
			label = string(r[:59]) + "…"
		}
	} else if a.streamingLocked() {
		label = "Writing…"
	}
	var lead tern.Element = ui.Spinner{Common: ui.Common{Key: "spinner", Tone: ui.ToneAccent}, Style: ui.SpinnerStarburst}
	if r := a.retry; r != nil {
		label = fmt.Sprintf("Retrying %d/%d", r.Attempt, r.MaxAttempts)
		if r.Message != "" {
			label += " · " + firstLine(r.Message)
		}
		left := 0.0
		if r.Delay > 0 {
			left = max(0, 1-float64(time.Since(a.retryAt))/float64(r.Delay))
		}
		lead = ui.Meter{Common: ui.Common{Key: "countdown"}, Value: &left, Style: ui.MeterRing, Size: ui.SizeSM}
	}
	if a.status == "compacting" {
		label = "Compacting the conversation…"
	}
	children := []tern.Element{
		lead,
		ui.Elapsed{Common: ui.Common{Key: "elapsed"}, Age: float64(time.Since(a.turnStart).Milliseconds()), Format: ui.ElapsedShort},
		ui.Text{Common: ui.Common{Key: "sep", Role: "omp.working.sep"}, Text: "·"},
		ui.Shimmer{Common: ui.Common{Key: "label", Role: "omp.working.label"}, Text: label},
		ui.Row{Common: ui.Common{Key: "fill", Grow: 1}},
	}
	if a.status == "compacting" {
		children = append(children, ui.Progress{Common: ui.Common{Key: "progress"}})
	}
	return ui.Row{Common: ui.Common{Key: "working", Role: "omp.working"}, Align: ui.AlignCenter, Gap: ui.GapSM, Children: children}
}

// runningToolLocked is the newest main-conversation tool call still running.
func (a *app) runningToolLocked() *agent.ToolCall {
	for i := len(a.order) - 1; i >= 0; i-- {
		e := a.entries[a.order[i]]
		// Queued and synthetic messages sit in the order but start no turn.
		if e == nil || e.local != nil || a.queued[a.order[i]] || e.item.Synthetic {
			continue
		}
		if e.item.Kind == agent.ItemUserMessage {
			return nil
		}
		if e.item.Kind == agent.ItemToolCall && e.item.Status == agent.ItemRunning && e.item.Tool != nil {
			return e.item.Tool
		}
	}
	return nil
}

// streamingLocked reports whether the newest entry is still arriving, so
// the separate working indicator would be redundant.
func (a *app) streamingLocked() bool {
	var e *entry
	for i := len(a.order) - 1; i >= 0 && e == nil; i-- {
		if !a.queued[a.order[i]] {
			e = a.entries[a.order[i]]
		}
	}
	return e != nil && e.local == nil && e.item.Status == agent.ItemRunning &&
		(e.item.Kind == agent.ItemAssistantMessage || e.item.Kind == agent.ItemToolCall || e.item.Kind == agent.ItemReasoning)
}

func (a *app) welcome() tern.Element {
	tips := []struct{ key, text string }{
		{"/", "commands"}, {"@", "files and images"}, {"⇧Tab", "permission mode"}, {"Esc Esc", "rewind"},
	}
	var tipSpans []ui.Span
	for i, t := range tips {
		if i > 0 {
			tipSpans = append(tipSpans, ui.Span{T: "   "})
		}
		tipSpans = append(tipSpans, ui.Span{T: t.key, S: "key"}, ui.Span{T: " " + t.text, S: "muted"})
	}
	var starters []tern.Element
	for i, p := range []string{"Explain this codebase", "Find and fix a bug", "Write tests for recent changes", "Review my uncommitted changes"} {
		prompt := p
		starters = append(starters, button(fmt.Sprint("s", i), prompt, "starter", func() {
			a.locked(func() { a.composer.set(prompt); a.submitLocked(agent.DeliverQueue) })
		}))
	}
	return ui.Card{Common: ui.Common{Key: "welcome", Role: "omp.welcome"}, Children: ui.Nodes(
		ui.Row{Common: ui.Common{Key: "lockup", Role: "omp.welcome.lockup"}, Align: ui.AlignCenter, Gap: ui.GapSM, Children: ui.Nodes(
			ui.Icon{Common: ui.Common{Role: "omp.welcome.logo", Tone: ui.ToneAccent}, Name: ui.IconName("app-" + strings.ToLower(a.backend.Name()))},
			ui.Text{Common: ui.Common{Role: "omp.welcome.wordmark"}, Spans: []ui.Span{{T: a.backend.Product(), S: "strong"}}},
			ui.Text{Common: ui.Common{Role: "omp.welcome.version"}, Spans: []ui.Span{{T: firstNonEmpty(a.info.Version, "tern"), S: "muted"}}},
		)},
		ui.Text{Common: ui.Common{Key: "where", Role: "agent.where"}, Spans: []ui.Span{{T: shortModel(a.model), S: "accent"}, {T: "  ·  " + tildePath(a.cwd), S: "muted"}}},
		ui.Row{Common: ui.Common{Key: "starters", Role: "agent.starters"}, Gap: ui.GapSM, Wrap: true, Children: starters},
		ui.Text{Common: ui.Common{Key: "tips", Role: "omp.welcome.tip"}, Spans: tipSpans},
	)}
}

func tildePath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

func (a *app) entryNode(id string, e *entry) tern.Element {
	key := entryKey(id)
	if e.local != nil {
		return a.localNode(key, e.local)
	}
	it := e.item
	switch it.Kind {
	case agent.ItemUserMessage:
		if it.Synthetic {
			return nil
		}
		return a.userNode(key, it)
	case agent.ItemAssistantMessage:
		if strings.TrimSpace(it.Text) == "" {
			return nil
		}
		md := ui.Md{Common: ui.Common{Key: key, Actions: &ui.Actions{Menu: []string{"copy"}}},
			Text: it.Text, Stream: it.Status == agent.ItemRunning}
		if !md.Stream {
			md.Text, md.Marks = a.linkPaths(it.Text)
		}
		return md
	case agent.ItemReasoning:
		if strings.TrimSpace(it.Text) == "" {
			return nil
		}
		// A summary of the reasoning (Codex) says so; raw thinking is a thought.
		done, live := "Thought", "Thinking…"
		if a.summaries[it.ID] {
			done, live = "Summary", "Summarizing…"
		}
		role, head := "omp.thinking", ui.Spans(ui.Span{T: done, S: "muted"})
		if it.Status == agent.ItemRunning {
			role, head = "omp.thinking.live", ui.Spans(ui.Span{T: live, S: "muted", Fx: ui.FxShimmer})
		} else if span := a.thinking[it.ID]; !span.start.IsZero() && span.end.After(span.start) && !a.summaries[it.ID] {
			head = ui.Spans(ui.Span{T: "Thought for " + fmtDuration(span.end.Sub(span.start).Round(time.Second)), S: "muted"})
		}
		return ui.Section{Common: ui.Common{Key: key, Role: role},
			Head: head, Collapsible: true, Collapsed: !a.expandAll,
			Children: ui.Nodes(ui.Md{Text: it.Text})}
	case agent.ItemToolCall:
		if it.Tool == nil {
			return nil
		}
		if it.Tool.Kind == agent.ToolTodo {
			return a.todoNode(key, it)
		}
		return a.toolNode(key, it, 0)
	case agent.ItemPlan:
		return ui.Card{Common: ui.Common{Key: key, Tone: ui.ToneInfo}, Head: ui.Spans(ui.Span{T: "Plan", S: "strong"}),
			Children: ui.Nodes(ui.Md{Text: it.Text})}
	case agent.ItemCompaction:
		// Claude puts the trigger in the item's text; the summary comes
		// with the compaction event.
		head := "Compacted"
		if t := strings.TrimSpace(it.Text); t == "auto" || t == "manual" {
			head += " (" + t + ")"
		}
		summary := ""
		if c := a.compaction; c != nil {
			if c.TokensBefore > 0 && c.TokensAfter > 0 {
				head += fmt.Sprintf(" · %s → %s tokens", compact(c.TokensBefore), compact(c.TokensAfter))
			}
			summary = c.Summary
		}
		kids := []tern.Element{ui.Text{Common: ui.Common{Key: "caption", Role: "omp.compaction.caption"},
			Spans: []ui.Span{{T: "Earlier messages were summarized to free context.", S: "muted"}}}}
		if strings.TrimSpace(summary) != "" {
			kids = append(kids, ui.Md{Common: ui.Common{Key: "summary"}, Text: summary})
		}
		return ui.Section{Common: ui.Common{Key: key, Role: "omp.compaction"}, Head: ui.Spans(ui.Span{T: head, S: "muted"}),
			Collapsible: true, Collapsed: true, Children: kids}
	case agent.ItemError:
		return ui.Card{Common: ui.Common{Key: key, Tone: ui.ToneError, Role: "omp.error"}, Head: ui.Spans(ui.Span{T: "Error", S: "strong"}),
			Children: ui.Nodes(ui.Text{Spans: []ui.Span{{T: it.Text, S: "error"}}})}
	}
	return nil
}

// entryKey makes a node key from an item id: stable, and free of the dots
// that separate node ids.
func entryKey(id string) string {
	return "e" + strings.NewReplacer(".", "_", "/", "_").Replace(id)
}

// userNode is a sent message in omp's card shape: hover tools, then
// attached images, then the text (right-click copies it).
func (a *app) userNode(key string, it agent.Item) tern.Element {
	tool := func(key, icon, title string, fn func()) tern.Element {
		return ui.Icon{Common: ui.Common{Key: key, Role: "omp.user.tool", Title: title, Aria: title,
			OnClick: func(*tern.Action) { a.locked(fn) }}, Name: ui.IconName(icon)}
	}
	tools := []tern.Element{
		tool("rewind", "rewind", "Rewind to here: choose what to restore", func() { a.startRewindLocked(it.ID) }),
		tool("fork", "git-branch", "Fork: continue from here in a copy", func() { a.forkLocked(firstNonEmpty(it.TurnID, it.ID)) }),
	}
	if !it.Time.IsZero() {
		tools = append(tools, ui.Text{Common: ui.Common{Key: "dot"}, Text: "·"},
			ui.Text{Common: ui.Common{Key: "time", Role: "omp.user.time", Title: it.Time.Local().Format("Jan 2 15:04:05")},
				Spans: []ui.Span{{T: it.Time.Local().Format("15:04"), S: "dim mono"}}})
	}
	children := []tern.Element{ui.Row{Common: ui.Common{Key: "tools", Role: "omp.user.tools"}, Gap: ui.GapXS, Align: ui.AlignCenter, Children: tools}}
	if a.queued[it.ID] {
		children = append(children, ui.Row{Common: ui.Common{Key: "badges", Role: "omp.user.badges"}, Justify: ui.JustifyEnd, Children: ui.Nodes(
			ui.Badge{Common: ui.Common{Key: "queued", Tone: ui.ToneAccent, Title: "Click to cancel",
				OnClick: func(*tern.Action) { a.locked(func() { a.cancelQueuedLocked(it.ID, nil) }) }}, Text: "queued · cancel"})})
	}
	if atts := a.sentAttachments[firstNonEmpty(it.ClientID, it.ID)]; len(atts) > 0 {
		children = append(children, a.sentAttachmentsNode(atts))
	}
	if strings.TrimSpace(it.Text) != "" {
		children = append(children, ui.Md{Common: ui.Common{Key: "text", Actions: &ui.Actions{Menu: []string{"copy"}}}, Text: it.Text})
	}
	return ui.Card{Common: ui.Common{Key: key, Role: "omp.user"}, Children: children}
}

// cancelQueuedLocked takes a queued message off the queue before it runs,
// then calls then (if any) with mu held. The CLI announces no event for a
// cancelled message, so the queue is updated here.
func (a *app) cancelQueuedLocked(turnID string, then func()) {
	gen := a.gen
	if cx, err := a.codexSession(); err == nil {
		// Codex holds queued input itself: dropping it only cancels.
		a.async("Cancel", func(ctx context.Context) error {
			if err := cx.InterruptTurn(ctx, turnID); err != nil {
				return err
			}
			a.mu.Lock()
			defer a.mu.Unlock()
			defer a.changed()
			if gen == a.gen && a.queued[turnID] {
				delete(a.queued, turnID)
				delete(a.queuedTurns, turnID)
				a.order = slices.DeleteFunc(a.order, func(id string) bool { return id == turnID })
				delete(a.entries, turnID)
				if then != nil {
					then()
				}
			}
			return nil
		})
		return
	}
	cs, err := a.claudeSession()
	if err != nil {
		a.toastLocked("error", err.Error())
		return
	}
	a.async("Cancel", func(ctx context.Context) error {
		cancelled, err := cs.CancelQueuedMessage(ctx, turnID)
		if err != nil {
			return err
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		defer a.changed()
		if gen != a.gen {
			return nil
		}
		if !cancelled {
			a.toastLocked("info", "That message has already started")
			return nil
		}
		delete(a.queued, turnID)
		a.order = slices.DeleteFunc(a.order, func(id string) bool { return id == turnID })
		delete(a.entries, turnID)
		if then != nil {
			then()
		}
		return nil
	})
}

// locked runs fn with mu held and redraws; node handlers use it.
func (a *app) locked(fn func()) {
	a.mu.Lock()
	fn()
	a.mu.Unlock()
	a.changed()
}

func (a *app) toolNode(key string, it agent.Item, depth int) tern.Element {
	c := it.Tool
	if c.Kind == agent.ToolTodo {
		return a.todoNode(key, it)
	}
	look := lookOf(c, a.cwd)
	status := runState(it.Status, c.IsError)

	if c.Kind == agent.ToolSubagent {
		if task, ok := a.taskFor(it.ID); ok {
			return a.agentNode(key, it, task, depth)
		}
	}

	if c.Kind == agent.ToolShell {
		return a.shellNode(key, it, look, status)
	}
	var body []tern.Element
	if look.Diff != "" {
		body = append(body, ui.Diff{Common: ui.Common{Key: "diff"}, Text: look.Diff, Path: look.DiffPath})
	}
	files := a.fileRows(look)
	if files != nil {
		body = append(body, files)
		look.Output = ""
	}
	if look.Output != "" {
		if c.Kind == agent.ToolShell {
			body = append(body, ui.Ansi{Common: ui.Common{Key: "out"}, Text: look.Output})
		} else {
			tone := "muted"
			if c.IsError {
				tone = "error"
			}
			body = append(body, ui.Text{Common: ui.Common{Key: "out"}, Spans: []ui.Span{{T: look.Output, S: tone}}})
		}
	}
	for i, img := range c.Images {
		if n := a.imageNode(fmt.Sprintf("img%d", i), img); n != nil {
			body = append(body, n)
		}
	}
	collapsed := status != ui.Running && status != ui.Errored && look.Diff == "" && !a.expandAll
	preview := ui.PreviewTail(4)
	if look.Diff != "" {
		preview = ui.PreviewLines(14)
	}
	frame := ui.FrameCard
	if look.Inline && status != ui.Errored {
		frame = ui.FrameInline
	}
	t := ui.Tool{
		CommonWithoutTitle: ui.CommonWithoutTitle{Key: key, Role: "omp.tool." + look.Icon},
		Frame:              frame,
		Intent:             firstNonEmpty(look.Intent, a.toolSummary[it.ID]),
		Name:               look.Icon,
		Title:              ui.T(look.Title),
		TargetKind:         look.TargetKind,
		Status:             status,
		Collapsible:        len(body) > 0,
		Collapsed:          &collapsed,
		Preview:            preview,
	}
	if look.Target != "" {
		t.Target = ui.T(look.Target)
	}
	for _, m := range look.Meta {
		t.Meta = append(t.Meta, ui.T(m))
	}
	if c.ExitCode != nil && *c.ExitCode != 0 {
		t.Exit = *c.ExitCode
	}
	if status == ui.Running {
		t.Age = a.ageOf(it.ID)
	} else if c.Duration > 0 {
		t.Took = ms(float64(c.Duration.Milliseconds()))
	}
	if len(body) > 0 {
		t.Tools = []ui.PickerAction{{ID: "copy", Label: "Copy"}}
	}
	t.Children = body
	return t
}

func (a *app) imageNode(key string, img agent.Image) tern.Element {
	switch {
	case len(img.Data) > 0:
		// Sent once: a rebuilt node reuses the blob.
		sum := sha256.Sum256(img.Data)
		key := "sha256:" + hex.EncodeToString(sum[:])
		id, ok := a.blobs[key]
		if !ok {
			var err error
			if id, err = a.session.Blob(img.Data, firstNonEmpty(img.MediaType, "image/png")); err != nil {
				return nil
			}
			a.blobs[key] = id
		}
		return ui.Image{Common: ui.Common{Key: key, Actions: &ui.Actions{Click: "zoom"}}, Blob: id}
	case img.Path != "":
		if id := a.imageBlob(img.Path); id != "" {
			return ui.Image{Common: ui.Common{Key: key, Actions: &ui.Actions{Click: "zoom"}}, Blob: id}
		}
	}
	return nil
}

func (a *app) taskFor(toolCallID string) (agent.Task, bool) {
	for _, t := range a.tasks {
		if t.ToolCallID == toolCallID {
			return t, true
		}
	}
	return agent.Task{}, false
}

// agentNode draws a subagent: its row, then the tool calls and final
// answer it produced, folded.
func (a *app) agentNode(key string, it agent.Item, task agent.Task, depth int) tern.Element {
	var kids []tern.Element
	for _, id := range a.subOrder[task.ID] {
		sub := a.entries[id]
		if sub == nil || sub.item.Kind != agent.ItemToolCall || sub.item.Tool == nil {
			continue
		}
		kids = append(kids, a.toolNode(entryKey(id), sub.item, depth+1))
	}
	if task.Error != "" && task.Status == agent.TaskFailed {
		kids = append(kids, ui.Text{Common: ui.Common{Key: "error"}, Spans: []ui.Span{{T: task.Error, S: "error"}}})
	}
	if out := firstNonEmpty(task.Output, it.Tool.Output); out != "" && task.Status != agent.TaskRunning {
		kids = append(kids, ui.Md{Common: ui.Common{Key: "answer"}, Text: tail(out, maxOutputBytes)})
	}
	stats := &ui.AgentStats{Tools: task.ToolUses}
	if task.Usage != nil {
		stats.Cost = task.Usage.CostUSD
		if task.Usage.TotalTokens > 0 {
			stats.Tokens = ms(float64(task.Usage.TotalTokens))
		}
	}
	if task.Status == agent.TaskRunning {
		stats.Age = ms(0)
	} else if task.Duration > 0 {
		stats.Took = ms(float64(task.Duration.Milliseconds()))
	}
	n := ui.Agent{
		Common:      ui.Common{Key: key},
		Name:        taskName(task),
		Task:        ui.T(firstNonEmpty(task.Description, lookOf(it.Tool, a.cwd).Target)),
		Status:      taskState(task.Status),
		Stats:       stats,
		Depth:       min(depth, 6),
		Collapsible: len(kids) > 0,
		Collapsed:   new(!a.expandAll),
		Children:    kids,
	}
	if task.Subagent != nil {
		n.Agent = task.Subagent.Type
		n.Model = modelEffort(task.Subagent)
	}
	if task.Status == agent.TaskRunning && task.LastTool != "" {
		n.Tool = &ui.AgentTool{Name: strings.ToLower(task.LastTool), Intent: task.Summary}
	}
	if task.Status == agent.TaskRunning {
		id := task.ID
		n.OnMenu = map[string]func(*tern.Action){"Stop": func(*tern.Action) { a.locked(func() { a.stopTaskLocked(id) }) }}
	}
	return n
}

func (a *app) localNode(key string, c *localCard) tern.Element {
	body := tern.Element(ui.Md{Common: ui.Common{Key: "text", Actions: &ui.Actions{Menu: []string{"copy"}}}, Text: c.Text})
	if c.Plain {
		body = ui.Text{Common: ui.Common{Key: "text", Actions: &ui.Actions{Menu: []string{"copy"}}}, Spans: []ui.Span{{T: c.Text, S: "mono muted"}}, Wrap: "word"}
	}
	children := []tern.Element{body}
	if len(c.Choices) > 0 {
		var buttons []tern.Element
		for i, ch := range c.Choices {
			run := ch.Run
			buttons = append(buttons, button(fmt.Sprint("c", i), ch.Label, "", func() { a.locked(run) }))
		}
		children = append(children, actionsRow("choices", buttons))
	}
	head := []ui.Span{{T: c.Title, S: "strong"}}
	if c.Meta != "" {
		head = append(head, ui.Span{T: " · " + c.Meta, S: "muted"})
	}
	return ui.Card{Common: ui.Common{Key: key, Tone: ui.Tone(c.Tone), Role: "agent.local"},
		Head: ui.Spans(head...), Collapsible: true, Collapsed: c.Collapsed && !a.expandAll, Children: children}
}

// button is omp's native button (role omp.btn): a label and an optional
// keycap, accent for the primary action, red for a destructive one.
func button(key, label, class string, fn func()) tern.Element {
	return shortcutButton(key, label, "", class, fn)
}

func shortcutButton(key, label, keys, class string, fn func()) tern.Element {
	var tone ui.Tone
	switch class {
	case "primary":
		tone = ui.ToneAccent
	case "danger":
		tone = ui.ToneError
	}
	kids := []tern.Element{ui.Text{Common: ui.Common{Key: "l"}, Text: label}}
	title := label
	if keys != "" {
		kids = append(kids, ui.Kbd{Common: ui.Common{Key: "k"}, Keys: strings.Split(keys, "+")})
		title += "  " + keys
	}
	return ui.Row{Common: ui.Common{Key: key, Role: "omp.btn", Tone: tone, Title: title, OnClick: func(*tern.Action) { fn() }},
		Gap: ui.GapXS, Align: ui.AlignCenter, Children: kids}
}

// actionsRow lays buttons out as omp's action bar; a nil entry pushes the
// rest to the right.
func actionsRow(key string, buttons []tern.Element) tern.Element {
	kids := make([]tern.Element, 0, len(buttons))
	for i, b := range buttons {
		if b == nil {
			b = ui.Spacer{Common: ui.Common{Key: fmt.Sprint("gap", i), Grow: 1}}
		}
		kids = append(kids, b)
	}
	return ui.Row{Common: ui.Common{Key: key, Role: "omp.actions"}, Gap: ui.GapSM, Align: ui.AlignCenter, Wrap: true, Children: kids}
}

// hint is one keyboard hint: keycaps and what they do.
type hint struct{ keys, label string }

// hintsRow draws key hints as omp's overlays do.
func hintsRow(key string, hints ...hint) tern.Element {
	var kids []tern.Element
	for i, h := range hints {
		var row []tern.Element
		for j, k := range strings.Split(h.keys, " ") {
			row = append(row, ui.Kbd{Common: ui.Common{Key: fmt.Sprint("k", j)}, Keys: strings.Split(k, "+")})
		}
		row = append(row, ui.Text{Common: ui.Common{Key: "l"}, Spans: []ui.Span{{T: h.label, S: "muted"}}})
		kids = append(kids, ui.Row{Common: ui.Common{Key: fmt.Sprint("h", i)}, Gap: ui.GapXS, Align: ui.AlignCenter, Children: row})
	}
	return ui.Row{Common: ui.Common{Key: key, Role: "omp.overlay.hints"}, Gap: ui.GapMD, Wrap: true, Children: kids}
}

func (a *app) dockRegion() tern.Nodes {
	var dock tern.Nodes
	if p := a.jobsPill(); p != nil {
		dock = append(dock, p)
	}
	if plan := a.planNode(); plan != nil {
		dock = append(dock, plan)
	}
	for _, t := range a.sortedTasksLocked() {
		if t.Status != agent.TaskRunning {
			continue
		}
		if _, shown := a.entries[t.ToolCallID]; shown && t.Kind == agent.TaskSubagent {
			continue // drawn in the transcript
		}
		id := t.ID
		row := ui.Agent{Common: ui.Common{Grow: 1, Title: firstNonEmpty(t.Description, t.Summary)}, Name: taskName(t), Agent: string(t.Kind),
			Status: taskState(t.Status), Stats: &ui.AgentStats{Tools: t.ToolUses, Age: ms(0)}}
		// The name is often the description; don't say it twice.
		if task := firstNonEmpty(t.Summary, t.Description); task != row.Name {
			row.Task = ui.T(task)
		}
		if t.Subagent != nil {
			row.Model = modelEffort(t.Subagent)
		}
		dock = append(dock, ui.Row{Common: ui.Common{Key: "task" + entryKey(id), Role: "agent.task"}, Align: ui.AlignCenter, Gap: ui.GapSM, Children: ui.Nodes(
			row, button("stop", "Stop", "", func() { a.locked(func() { a.stopTaskLocked(id) }) }),
		)})
	}
	if r := a.firstRequestLocked(); r != nil {
		if n := a.requestNode(r); n != nil {
			dock = append(dock, n)
		}
		if n := len(a.requests) - 1; n > 0 {
			dock = append(dock, ui.Text{Common: ui.Common{Key: "more", Role: "agent.more"}, Spans: []ui.Span{{T: fmt.Sprintf("%d more waiting", n), S: "muted"}}})
		}
	}
	if q := a.queueNode(); q != nil {
		dock = append(dock, q)
	}
	if w := a.workingNode(); w != nil {
		dock = append(dock, w)
	}
	if a.rewind != nil {
		dock = append(dock, a.rewindBar())
	} else {
		dock = append(dock, a.composerNode())
	}
	return dock
}

func (a *app) planNode() tern.Element {
	if len(a.plan) == 0 {
		return nil
	}
	open := false
	var items []ui.Todo
	for i, s := range a.plan {
		state := ui.TodoPending
		switch s.Status {
		case agent.StepCompleted:
			state = ui.TodoDone
		case agent.StepInProgress:
			state = ui.TodoActive
			open = true
		default:
			open = true
		}
		items = append(items, ui.Todo{ID: firstNonEmpty(s.ID, strconv.Itoa(i)), Text: ui.T(s.Text), Status: state})
	}
	if !open && !a.showPlan {
		return nil
	}
	mode := ui.ChecklistHUD
	if a.showPlan {
		mode = ui.ChecklistFull
	}
	return ui.Checklist{Common: ui.Common{Key: "plan", OnActivate: func(*tern.Activate) { a.locked(func() { a.showPlan = !a.showPlan }) }},
		Mode: mode, Phases: []ui.Phase{{ID: "todo", Items: items}}}
}

// completionNode is the suggestion popover under the composer's caret, in
// the shape Tern's chat skin draws for its own agent's autocomplete.
func (a *app) completionNode() tern.Element {
	// A command's label starts with "/", so only files get an icon.
	icon := ui.IconName("")
	if a.complete.kind == "file" {
		icon = "file"
	}
	var items []tern.Element
	for i, s := range a.complete.items {
		label, hint := s.Label, ""
		if a.complete.kind == "command" {
			if name, args, ok := strings.Cut(s.Label, " "); ok {
				label, hint = name, args
			}
		}
		items = append(items, ui.Item{Common: ui.Common{Key: fmt.Sprint("s", i)}, Icon: icon,
			Label: ui.T(label), Detail: ui.T(s.Detail), Value: ui.T(hint)})
	}
	pick := func(item string) {
		a.locked(func() {
			if i, err := strconv.Atoi(strings.TrimPrefix(item[strings.LastIndexByte(item, '.')+1:], "s")); err == nil && i < len(a.complete.items) {
				a.complete.sel = i
				a.acceptCompletionLocked()
			}
		})
	}
	return ui.Overlay{Common: ui.Common{Key: "complete", Role: "omp.autocomplete"}, Anchor: ui.AtCaret("dock.composer.input"), Size: ui.SizeSM,
		Children: ui.Nodes(ui.List{CommonWithoutMax: ui.CommonWithoutMax{Key: "list",
			OnSelect: func(e *tern.Select) { pick(e.Item) }, OnActivate: func(e *tern.Activate) { pick(e.Item) }},
			Selected: fmt.Sprintf("layer.complete.list.s%d", a.complete.sel), Max: ui.MaxLines(10), Children: items})}
}

// composerNode is the floating composer: the editor over a bar of
// session controls, in the shape Tern's chat skin draws as one card.
func (a *app) composerNode() tern.Element {
	c := &a.composer
	cursor := c.cursor16()
	ghost := ""
	if a.complete.open() {
		s := a.complete.items[a.complete.sel].Value
		if token, _ := c.token(); strings.HasPrefix(s, token) && c.cursor == len(c.text) {
			ghost = strings.TrimSuffix(s[len(token):], " ")
		}
	}
	if len(c.text) == 0 && a.suggestion != "" && !a.running() && a.firstRequestLocked() == nil {
		ghost = a.suggestion
	}
	placeholder := "Ask " + a.backend.Name() + " anything — / for commands, @ for files"
	switch r := a.firstRequestLocked().(type) {
	case *agent.UserInputRequest:
		placeholder = "Type an answer, or pick an option above"
	case *agent.ElicitationRequest:
		if d := a.answers[r.ID]; d != nil && d.editing != "" {
			placeholder = "Value for " + d.editing + ", then Enter"
		}
	case *agent.PermissionRequest:
		placeholder = "1 allow · 2 for the session · 3 deny · or type why and press Enter to deny"
		if r.Kind == agent.PermissionPlanApproval && a.planFeedback == r.ID {
			placeholder = "What should " + a.backend.Name() + " change in the plan? Enter sends · Esc shows the plan again"
		}
	default:
		switch {
		case a.renaming != nil:
			placeholder = "New title for “" + a.renaming.title + "” · Enter saves · Esc cancels"
		case a.goal != nil && a.goal.Objective != "":
			// As omp does: the goal the agent works toward, quoted.
			placeholder = "“" + firstLine(a.goal.Objective) + "”"
		case a.running():
			placeholder = "Queue a follow-up · Ctrl+Enter steers the current turn"
		}
	}
	editor := ui.Editor{
		Common: ui.Common{Key: "input",
			OnEdit: a.edit, OnUndo: func(*tern.Undo) { a.undo() },
			OnSend:  func(s *tern.Send) { a.sendText(s.Text) },
			OnFocus: func(f *tern.Focus) { _ = a.surface.Focus(f.ID) }},
		Field:    ui.Field{Text: c.String(), Cursor: &cursor, Ghost: ghost, Placeholder: ui.T(placeholder), Sendable: a.sess != nil},
		MaxLines: 12,
	}
	if r := a.askRequestLocked(); r != nil {
		return ui.Col{Common: ui.Common{Key: "composer", Role: "omp.editor"}, Children: ui.Nodes(a.askNode(r), a.composerBar())}
	}
	kids := []tern.Element{}
	if len(a.attachments) > 0 {
		kids = append(kids, a.attachmentChips("atts", a.attachments))
	}
	kids = append(kids, editor, a.composerBar())
	if m := a.contextMeter(); m != nil {
		kids = append([]tern.Element{m}, kids...)
	}
	return ui.Col{Common: ui.Common{Key: "composer", Role: "omp.editor"}, Children: kids}
}

// contextMeter is the bar along the composer's top edge: how much of the
// context window the conversation fills, and the window's size.
func (a *app) contextMeter() tern.Element {
	if a.contextTokens <= 0 || a.contextWindow <= 0 {
		return nil
	}
	used := min(float64(a.contextTokens)/float64(a.contextWindow), 1)
	return ui.Meter{Common: ui.Common{Key: "context", Role: "omp.composer.context",
		Title:   fmt.Sprintf("%s of %s tokens of context · click for details", compact(a.contextTokens), compact(a.contextWindow)),
		OnClick: func(*tern.Action) { a.locked(a.toggleContextLocked) }},
		Value: &used, Thresholds: &ui.Thresholds{Warn: ms(0.75), Bad: ms(0.9)},
		Label: ui.T(fmt.Sprintf("%.0f%%", used*100)), Total: ui.T(compact(a.contextWindow))}
}

// composerBar holds the model and effort, then session facts, then Send or
// Stop.
func (a *app) composerBar() tern.Element {
	chip := func(key, role, icon, text, title string, click func()) tern.Element {
		return ui.Row{Common: ui.Common{Key: key, Role: role, Title: title, OnClick: func(*tern.Action) { a.locked(click) }},
			Gap: ui.GapXS, Align: ui.AlignCenter, Children: ui.Nodes(
				ui.Icon{Name: ui.IconName(icon)},
				ui.Text{Text: text},
			)}
	}
	model := chip("model", "omp.composer.model", "model", shortModel(a.model), "Model · click or /model to change", func() { a.cmdModel("") }).(ui.Row)
	model.Children = append(model.Children, ui.Icon{Name: "chev"}) // the caret: it opens a menu
	bar := []tern.Element{model}
	if a.effort != "" {
		bar = append(bar, chip("effort", "omp.composer.effort", "brain", string(a.effort), "Reasoning effort · click or /effort", func() { a.cmdEffort("") }))
	}
	bar = append(bar, a.extras())
	for i, l := range a.rateLimitsLocked() {
		if l.Status == agent.RateLimitOK {
			continue
		}
		tone := ui.ToneWarning
		if l.Status == agent.RateLimitExceeded {
			tone = ui.ToneError
		}
		bar = append(bar, ui.Text{Common: ui.Common{Key: fmt.Sprint("rate", i), Role: "omp.composer.rate",
			Tone: tone, Title: limitName(l.Name) + " limit" + resetsIn(l.ResetsAt)}, Spans: []ui.Span{{T: fmt.Sprintf("%s %.0f%%", limitName(l.Name), l.UsedPercent), S: string(tone)}}})
	}
	if a.costUSD > 0 {
		bar = append(bar, ui.Text{Common: ui.Common{Key: "usage", Role: "omp.composer.usage", Title: "Cost of this conversation"},
			Spans: []ui.Span{{T: fmt.Sprintf("$%.2f", a.costUSD), S: "muted"}}})
	}
	if a.running() {
		bar = append(bar, ui.Row{Common: ui.Common{Key: "stop", Role: "omp.composer.stop", Title: "Interrupt (esc)",
			OnClick: func(*tern.Action) { a.locked(a.interruptLocked) }}, Gap: ui.GapXS, Align: ui.AlignCenter, Children: ui.Nodes(
			ui.Text{Text: "Stop"},
		)})
	} else {
		bar = append(bar, ui.Row{Common: ui.Common{Key: "send", Role: "omp.composer.send", Title: "Send",
			OnClick: func(*tern.Action) { a.locked(func() { a.submitLocked(agent.DeliverQueue) }) }}, Gap: ui.GapXS, Align: ui.AlignCenter, Children: ui.Nodes(
			ui.Kbd{Keys: []string{"enter"}},
		)})
	}
	return ui.Row{Common: ui.Common{Key: "bar", Role: "omp.composer.bar"}, Align: ui.AlignCenter, Children: bar}
}

// extras is the bar's status strip: connection state, permission mode,
// folder, queue and goal. It drops segments when the pane is narrow.
func (a *app) extras() tern.Element {
	var segs []tern.Element
	seg := func(s ui.Seg) { segs = append(segs, s) }
	switch a.status {
	case "starting":
		seg(ui.Seg{Common: ui.Common{Key: "state", Tone: ui.TonePending}, Text: "starting", Priority: 10,
			Children: ui.Nodes(ui.Spinner{Style: ui.SpinnerDots})})
	case "error":
		seg(ui.Seg{Common: ui.Common{Key: "state", Tone: ui.ToneError, Title: a.err, Min: &ui.Bounds{W: ui.Ch(16)}}, Icon: "x-circle", Text: a.err, Priority: 10})
	case "closed":
		seg(ui.Seg{Common: ui.Common{Key: "state", Tone: ui.ToneWarning, Title: "/continue resumes it, /new starts over",
			OnClick: func(*tern.Action) { a.locked(func() { a.cmdContinue("") }) }}, Icon: "warn", Text: "session ended", Priority: 10})
	}
	var broken []string
	for _, m := range a.info.MCPServers {
		if m.Status == "failed" || m.Status == "needs-auth" {
			broken = append(broken, m.Name+" "+m.Status)
		}
	}
	if len(broken) > 0 {
		seg(ui.Seg{Common: ui.Common{Key: "mcp", Tone: ui.ToneWarning, Title: strings.Join(broken, ", ") + " · click for MCP servers",
			OnClick: func(*tern.Action) { a.locked(func() { a.cmdMCP("") }) }}, Icon: "plug", Text: plural(len(broken), "MCP issue"), Priority: 4})
	}
	modeTone := ui.Tone("")
	if a.mode != agent.PermissionDefault && a.mode != "" {
		modeTone = ui.ToneWarning
	}
	seg(ui.Seg{Common: ui.Common{Key: "mode", Tone: modeTone, Title: "Permission mode · Shift+Tab or click to cycle",
		OnClick: func(*tern.Action) { a.locked(a.cycleModeLocked) }}, Icon: "shield", Text: modeLabel(a.mode), Priority: 7})
	if n := len(a.queued); n > 0 {
		seg(ui.Seg{Common: ui.Common{Key: "queued"}, Icon: "inbox", Text: fmt.Sprintf("%d queued", n), Priority: 5})
	}
	if b := a.branchLocked(); b != "" {
		seg(ui.Seg{Common: ui.Common{Key: "branch", Title: "Git branch"}, Icon: "branch", Text: b, Priority: 3})
	}

	return ui.Status{Common: ui.Common{Key: "extras", Role: "omp.composer.extras", Grow: 1}, Transparent: true, Children: segs}
}

// shellNode draws a command as omp does: "Bash" and what it is for, the
// command as a highlighted script clamped to a few lines, its output
// clamped too, and a footer with how it ended.
func (a *app) shellNode(key string, it agent.Item, look toolLook, status ui.RunState) tern.Element {
	c := it.Tool
	command := strings.TrimSpace(look.Target)
	title := "Shell"
	if c.Name == "Bash" {
		title = "Bash"
	}
	t := ui.Tool{
		CommonWithoutTitle: ui.CommonWithoutTitle{Key: key, Role: "omp.tool.bash"},
		Name:               "bash",
		Title:              ui.T(title),
		Intent:             firstNonEmpty(a.toolSummary[it.ID], command),
		Status:             status,
		Frame:              ui.FrameInline,
		Collapsible:        true,
		Collapsed:          new(false),
	}
	// The purpose heads the card when the model gave one; otherwise the
	// command does, and the script below only repeats a long one.
	script := command
	if look.Intent != "" {
		t.Target, t.TargetKind = ui.T(look.Intent), ui.TargetText
	} else {
		t.Target, t.TargetKind = ui.T(firstLine(command)), ui.TargetCommand
		if !strings.Contains(command, "\n") && len(command) <= 80 {
			script = ""
		}
	}
	var body []tern.Element
	if script != "" {
		body = append(body, a.clampedScript(it.ID, script)...)
	}
	if out := strings.TrimRight(look.Output, "\n"); out != "" {
		body = append(body, ui.Ansi{Common: ui.Common{Key: "out", Role: "omp.tool.bash.output"}, Text: out,
			Preview: ui.PreviewLines(10), Follow: status == ui.Running})
	}
	if in := strings.TrimRight(a.stdin[it.ID], "\n"); in != "" {
		// What the agent typed into the command while it ran.
		body = append(body, ui.Text{Common: ui.Common{Key: "stdin"}, Spans: []ui.Span{{T: "› ", S: "accent"}, {T: tail(in, 2000), S: "mono muted"}}})
	}
	for i, img := range c.Images {
		if n := a.imageNode(fmt.Sprintf("img%d", i), img); n != nil {
			body = append(body, n)
		}
	}
	if status != ui.Running {
		body = append(body, shellFooter(c, status))
	} else {
		t.Age = a.ageOf(it.ID)
	}
	if c.ExitCode != nil && *c.ExitCode != 0 {
		t.Exit = *c.ExitCode
	}
	t.Tools = []ui.PickerAction{{ID: "copy", Label: "Copy"}}
	t.Children = body
	return t
}

// scriptLines is how much of a long command shows until it is opened.
const scriptLines = 5

// clampedScript is a command as highlighted code: the first lines, then a
// "N more lines" toggle that opens the rest in place (Ctrl+O opens all).
func (a *app) clampedScript(id, script string) []tern.Element {
	lines := strings.Split(script, "\n")
	open := a.expandAll || a.openScripts[id]
	shown := script
	if !open && len(lines) > scriptLines+1 {
		shown = strings.Join(lines[:scriptLines], "\n")
	}
	out := []tern.Element{ui.Code{Common: ui.Common{Key: "command", Role: "omp.tool.bash.command"}, Text: shown, Lang: "bash", Wrap: true}}
	if len(lines) > scriptLines+1 && !a.expandAll {
		label := fmt.Sprintf("%d more lines", len(lines)-scriptLines)
		icon := ui.IconName("chev")
		if open {
			label, icon = "Show less", "chev-up"
		}
		toggle := func(*tern.Action) {
			a.locked(func() {
				a.openScripts[id] = !a.openScripts[id]
				if e := a.entries[id]; e != nil {
					e.ver++
				}
			})
		}
		out = append(out, ui.Row{Common: ui.Common{Key: "more", Role: "agent.more-lines", OnClick: toggle}, Gap: ui.GapXS, Align: ui.AlignCenter,
			Children: ui.Nodes(ui.Text{Spans: []ui.Span{{T: label, S: "muted"}}}, ui.Icon{Common: ui.Common{Tone: ui.ToneMuted}, Name: icon})})
	}
	return out
}

// ageOf is how long a running tool has run, in ms, by its own progress
// reports when there are any.
func (a *app) ageOf(id string) *float64 {
	if start, ok := a.toolStart[id]; ok {
		return ms(float64(time.Since(start).Milliseconds()))
	}
	return ms(0)
}

// shellFooter says how a command ended and how long it took.
func shellFooter(c *agent.ToolCall, status ui.RunState) tern.Element {
	icon, word, tone := ui.IconName("check"), "Done", ui.ToneSuccess
	switch {
	case status == ui.Errored && c.ExitCode != nil:
		icon, word, tone = "x", fmt.Sprintf("Exit %d", *c.ExitCode), ui.ToneError
	case status == ui.Errored:
		icon, word, tone = "x", "Failed", ui.ToneError
	case status == ui.Cancelled:
		icon, word, tone = "slash-circle", "Stopped", ui.ToneWarning
	}
	kids := ui.Nodes(ui.Icon{Common: ui.Common{Tone: tone}, Name: icon}, ui.Text{Common: ui.Common{Tone: tone}, Text: word})
	if c.Duration > 0 {
		kids = append(kids, ui.Text{Common: ui.Common{Key: "took"}, Spans: []ui.Span{{T: fmtDuration(c.Duration), S: "dim mono"}}})
	}
	return ui.Row{Common: ui.Common{Key: "status", Role: "omp.run.status"}, Gap: ui.GapSM, Align: ui.AlignCenter, Children: kids}
}

// jobsPill counts the work running in the background, as omp's dock does;
// it opens /tasks.
func (a *app) jobsPill() tern.Element {
	n := 0
	for _, t := range a.tasks {
		if t.Status == agent.TaskRunning && !t.Hidden {
			n++
		}
	}
	n = max(n, len(a.background))
	if n == 0 {
		return nil
	}
	return ui.Row{Common: ui.Common{Key: "jobs", Role: "agent.jobs", Title: plural(n, "background job") + " · click for details",
		OnClick: func(*tern.Action) { a.locked(func() { a.cmdTasks("") }) }}, Gap: ui.GapXS, Align: ui.AlignCenter,
		Children: ui.Nodes(ui.Icon{Common: ui.Common{Tone: ui.ToneWarning}, Name: "activity"}, ui.Text{Spans: []ui.Span{{T: strconv.Itoa(n), S: "warning"}}})}
}

// modelEffort is a subagent's model chip: its model and, when reported,
// its reasoning effort.
func modelEffort(s *agent.Subagent) string {
	return joinNonEmpty(" · ", strings.TrimPrefix(s.Model, "claude-"), string(s.Effort))
}

func shortModel(m string) string {
	return firstNonEmpty(strings.TrimPrefix(m, "claude-"), "default model")
}

func (a *app) layerRegion() tern.Nodes {
	var layer tern.Nodes
	if a.complete.open() && a.menu == nil {
		layer = append(layer, a.completionNode())
	}
	if r := a.planRequestLocked(); r != nil && a.menu == nil {
		layer = append(layer, a.planOverlay(r))
	}
	if a.showContext && a.menu == nil {
		layer = append(layer, a.contextNode())
	}
	if a.prefs != nil {
		layer = append(layer, a.prefsNode())
	}
	if a.menu != nil {
		layer = append(layer, a.pickerNode())
	}
	for _, t := range a.toasts {
		tone := ui.ToneInfo
		switch t.level {
		case "error":
			tone = ui.ToneError
		case "warning":
			tone = ui.ToneWarning
		case "success":
			tone = ui.ToneSuccess
		}
		layer = append(layer, ui.Toast{Common: ui.Common{Key: fmt.Sprint("t", t.id), Tone: tone}, Text: t.text})
	}
	if len(layer) == 0 {
		return nil
	}
	return layer
}

func permissionTitle(name string, r *agent.PermissionRequest, cwd string) string {
	if r.Title != "" && r.Title != r.ToolName {
		return r.Title
	}
	switch r.Kind {
	case agent.PermissionCommand:
		return "Allow " + name + " to run this command?"
	case agent.PermissionFileChange:
		return "Allow " + name + " to change files?"
	case agent.PermissionGrant:
		return "Allow " + name + " extra access?"
	}
	if diff, path := requestDiff(r, cwd); diff != "" {
		return "Allow " + name + " to edit " + firstNonEmpty(path, "files") + "?"
	}
	return "Allow " + name + " to use " + r.ToolName + "?"
}

// requestDiff is the change an edit permission would make.
func requestDiff(r *agent.PermissionRequest, cwd string) (string, string) {
	if len(r.Changes) > 0 {
		return editDiff(r.Changes, nil), relPath(r.Changes[0].Path, cwd)
	}
	switch r.ToolName {
	case "Edit", "MultiEdit", "Write", "NotebookEdit":
		l := lookOf(&agent.ToolCall{Name: r.ToolName, Kind: agent.ToolEdit, Input: r.Input}, cwd)
		return l.Diff, l.Target
	}
	return "", ""
}

// fileRows draws a search's file list as linked paths, as omp's tool.files
// does; nil when the output isn't a list of existing files.
func (a *app) fileRows(look toolLook) tern.Element {
	if !look.Listing || strings.TrimSpace(look.Output) == "" {
		return nil
	}
	lines := splitLines(strings.TrimSpace(look.Output))
	if len(lines) > 200 {
		return nil
	}
	var rows []tern.Element
	for i, l := range lines {
		l = strings.TrimSpace(l)
		abs, ok := a.existingPath(l)
		if !ok {
			return nil
		}
		if i < 20 {
			rows = append(rows, ui.Text{Common: ui.Common{Key: fmt.Sprint("f", i), Href: "file://" + abs},
				Spans: []ui.Span{{T: relPath(abs, a.cwd), S: "path", Href: "file://" + abs}}})
		}
	}
	if n := len(lines) - len(rows); n > 0 {
		rows = append(rows, ui.Text{Common: ui.Common{Key: "more"}, Spans: []ui.Span{{T: fmt.Sprintf("… %d more", n), S: "muted"}}})
	}
	return ui.Col{Common: ui.Common{Key: "files", Role: "omp.tool.files"}, Gap: ui.GapNone, Children: rows}
}

// queueNode lists messages waiting for the running turn, as omp's queue
// strip: Edit pulls one back into the composer, the x drops it.
func (a *app) queueNode() tern.Element {
	var ids []string
	for _, id := range a.order {
		if a.queued[id] {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	var rows []tern.Element
	for i, id := range ids {
		e := a.entries[id]
		if e == nil {
			continue
		}
		it := e.item
		kids := []tern.Element{
			ui.Icon{Common: ui.Common{Key: "icon"}, Name: "corner-down-right"},
			ui.Text{Common: ui.Common{Key: "text", Grow: 1}, Text: strings.Join(strings.Fields(it.Text), " "), Truncate: "end", Wrap: "none"},
		}
		if i == 0 && len(ids) > 1 {
			kids = append(kids, ui.Badge{Common: ui.Common{Key: "count", Role: "omp.queue.count"}, Text: fmt.Sprint(len(ids))})
		}
		kids = append(kids,
			ui.Row{Common: ui.Common{Key: "edit", Role: "omp.queue.edit", Title: "Edit: cancel it and put it back in the composer",
				OnClick: func(*tern.Action) {
					a.locked(func() {
						a.cancelQueuedLocked(it.ID, func() {
							if len(a.composer.text) == 0 {
								a.composer.set(it.Text)
							}
						})
					})
				}}, Gap: ui.GapXS, Align: ui.AlignCenter, Children: ui.Nodes(ui.Text{Text: "Edit"})},
			ui.Row{Common: ui.Common{Key: "drop", Role: "omp.queue.edit", Title: "Remove from the queue",
				OnClick: func(*tern.Action) { a.locked(func() { a.cancelQueuedLocked(it.ID, nil) }) }}, Children: ui.Nodes(ui.Text{Text: "×"})},
		)
		rows = append(rows, ui.Row{Common: ui.Common{Key: "q" + entryKey(id), Role: "omp.queue.item", Title: "Runs after the current turn"},
			Align: ui.AlignCenter, Gap: ui.GapSM, Children: kids})
	}
	return ui.Col{Common: ui.Common{Key: "queue", Role: "omp.queue"}, Gap: ui.GapXS, Children: rows}
}

func isGroupableRead(it agent.Item) bool {
	return it.Kind == agent.ItemToolCall && it.Tool != nil && it.Tool.Kind == agent.ToolRead &&
		it.Status == agent.ItemDone && !it.Tool.IsError
}

// readGroupNode folds consecutive reads into one inline step, as omp does:
// "Read 3 files", each file a linked row with its size.
func (a *app) readGroupNode(ids []string) tern.Element {
	// Rebuilt only when a member changes, like single entries.
	var sig strings.Builder
	for _, id := range ids {
		fmt.Fprintf(&sig, "%s:%d,", id, a.entries[id].ver)
	}
	fmt.Fprint(&sig, a.expandAll)
	if c, ok := a.readGroups[ids[0]]; ok && c.sig == sig.String() {
		return c.node
	}
	node := a.buildReadGroup(ids)
	a.readGroups[ids[0]] = cachedNode{sig: sig.String(), node: node}
	return node
}

// cachedNode is a built node and what it was built from.
type cachedNode struct {
	sig  string
	node tern.Element
}

func (a *app) buildReadGroup(ids []string) tern.Element {
	var rows []tern.Element
	var took time.Duration
	for i, id := range ids {
		it := a.entries[id].item
		look := lookOf(it.Tool, a.cwd)
		took += it.Tool.Duration
		spans := []ui.Span{{T: look.Target, S: "path"}}
		if abs, ok := a.existingPath(look.Target); ok {
			spans[0].Href = "file://" + abs
		}
		if len(look.Meta) > 0 {
			spans = append(spans, ui.Span{T: "  " + strings.Join(look.Meta, " · "), S: "muted"})
		}
		rows = append(rows, ui.Text{Common: ui.Common{Key: fmt.Sprint("f", i)}, Spans: spans})
	}
	collapsed := !a.expandAll
	t := ui.Tool{CommonWithoutTitle: ui.CommonWithoutTitle{Key: "reads" + entryKey(ids[0]), Role: "omp.tool.read"},
		Name: "read", Title: ui.T("Read"), Target: ui.T(plural(len(ids), "file")), TargetKind: ui.TargetText,
		Status: ui.Done, Frame: ui.FrameInline, Collapsible: true, Collapsed: &collapsed, Children: rows}
	if took > 0 {
		t.Took = ms(float64(took.Milliseconds()))
	}
	return t
}

// todoNode shows a todo-list update inline: "Todo 3/7" over the checklist
// it set.
func (a *app) todoNode(key string, it agent.Item) tern.Element {
	steps := a.planAt[it.ID]
	if len(steps) == 0 {
		return nil
	}
	done := 0
	for _, s := range steps {
		if s.Status == agent.StepCompleted {
			done++
		}
	}
	collapsed := !a.expandAll && it.Status != agent.ItemRunning
	return ui.Tool{CommonWithoutTitle: ui.CommonWithoutTitle{Key: key, Role: "omp.tool.todo"},
		Name: "todo", Title: ui.T("Todo"), Target: ui.T(fmt.Sprintf("%d/%d", done, len(steps))), TargetKind: ui.TargetText,
		Status: runState(it.Status, it.Tool.IsError), Frame: ui.FrameInline, Collapsible: true, Collapsed: &collapsed,
		Children: ui.Nodes(ui.Checklist{Common: ui.Common{Key: "list"}, Mode: ui.ChecklistFull, Phases: []ui.Phase{{ID: "todo", Items: todos(steps)}}})}
}

func todos(steps []agent.PlanStep) []ui.Todo {
	var items []ui.Todo
	for i, s := range steps {
		state := ui.TodoPending
		switch s.Status {
		case agent.StepCompleted:
			state = ui.TodoDone
		case agent.StepInProgress:
			state = ui.TodoActive
		}
		items = append(items, ui.Todo{ID: firstNonEmpty(s.ID, strconv.Itoa(i)), Text: ui.T(s.Text), Status: state})
	}
	return items
}

// withMark draws a transcript node marked for the rewind.
func withMark(el tern.Element, m ui.Mark) tern.Element {
	if m == "" {
		return el
	}
	switch n := el.(type) {
	case ui.Card:
		n.Mark = m
		return n
	case ui.Col:
		n.Mark = m
		return n
	}
	return el
}
