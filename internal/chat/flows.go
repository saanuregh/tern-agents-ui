package chat

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/stencil-hq/tern-sdk/go/tern"
	"github.com/stencil-hq/tern-sdk/go/tern/ui"
)

// The requests Claude makes are drawn where omp draws them: permissions as
// a card in the dock, questions in place of the composer, plan approval as
// a sheet over the transcript, MCP forms as a card.

// requestNode is the dock card for a permission or an MCP form. Questions
// and plan approval have their own places (askNode, planOverlay).
func (a *app) requestNode(r agent.Request) tern.Element {
	key := "req" + entryKey(r.RequestID())
	card := func(tone ui.Tone, title string, body ...tern.Element) tern.Element {
		return ui.Card{Common: ui.Common{Key: key, Tone: tone, Role: "agent.request"},
			Head: ui.Spans(ui.Span{T: title, S: "strong"}), Children: body}
	}
	switch r := r.(type) {
	case *agent.PermissionRequest:
		if r.Kind == agent.PermissionPlanApproval {
			return nil
		}
		var body []tern.Element
		if r.Command != "" {
			body = append(body, ui.Code{Common: ui.Common{Key: "cmd"}, Text: r.Command, Lang: "bash"})
		}
		if diff, path := requestDiff(r, a.cwd); diff != "" {
			body = append(body, ui.Diff{Common: ui.Common{Key: "diff", Max: &ui.Bounds{H: ui.Lines(16)}}, Text: diff, Path: path})
		}
		if detail := firstNonEmpty(r.Description, r.Reason); detail != "" {
			body = append(body, ui.Md{Common: ui.Common{Key: "detail"}, Text: detail})
		}
		if len(r.Paths) > 0 {
			body = append(body, ui.Text{Common: ui.Common{Key: "paths"}, Spans: []ui.Span{{T: strings.Join(r.Paths, "\n"), S: "muted"}}})
		}
		buttons := []tern.Element{shortcutButton("allow", "Allow", "1", "primary", func() { a.locked(func() { a.permitLocked(r, agent.DecisionAllow) }) })}
		if offers(r, agent.DecisionAllowSession) {
			buttons = append(buttons, shortcutButton("session", "Allow for this session", "2", "", func() { a.locked(func() { a.permitLocked(r, agent.DecisionAllowSession) }) }))
		}
		if offers(r, agent.DecisionAllowAlways) && len(r.Decisions) > 0 {
			buttons = append(buttons, shortcutButton("always", "Always allow", "a", "", func() { a.locked(func() { a.permitLocked(r, agent.DecisionAllowAlways) }) }))
		}
		buttons = append(buttons, nil,
			shortcutButton("cancel", "Deny and stop", "esc", "", func() { a.locked(func() { a.permitLocked(r, agent.DecisionCancel) }) }),
			shortcutButton("deny", "Deny", "3", "danger", func() { a.locked(func() { a.permitLocked(r, agent.DecisionDeny) }) }))
		body = append(body, actionsRow("act", buttons))
		if t := strings.TrimSpace(a.composer.String()); t != "" {
			body = append(body, ui.Text{Common: ui.Common{Key: "reason"}, Spans: []ui.Span{{T: "Enter denies and tells " + a.backend.Name() + ": ", S: "muted"}, {T: t}}})
		}
		return card(ui.ToneWarning, permissionTitle(a.backend.Name(), r, a.cwd), body...)
	case *agent.ElicitationRequest:
		return a.elicitationNode(key, r)
	}
	return nil
}

func (a *app) elicitationNode(key string, r *agent.ElicitationRequest) tern.Element {
	d := a.draftFor(r.ID)
	title := firstNonEmpty(r.Title, r.Server+" asks for input")
	body := []tern.Element{ui.Md{Common: ui.Common{Key: "msg"}, Text: firstNonEmpty(r.Message, r.Description)}}
	decline := shortcutButton("decline", "Decline", "esc", "", func() {
		a.locked(func() { a.respond(r, func(ctx context.Context) error { return r.Decline(ctx) }) })
	})
	card := func(body ...tern.Element) tern.Element {
		return ui.Card{Common: ui.Common{Key: key, Tone: ui.ToneInfo, Role: "agent.request"}, Head: ui.Spans(ui.Span{T: title, S: "strong"}), Children: body}
	}
	if r.Mode == "url" || r.URL != "" {
		open := ui.Row{Common: ui.Common{Key: "open", Role: "omp.btn", Tone: ui.ToneAccent, Href: r.URL, Actions: &ui.Actions{Click: "open"}, Title: r.URL},
			Gap: ui.GapXS, Children: ui.Nodes(ui.Text{Text: "Open in browser"})}
		done := shortcutButton("done", "Done", "enter", "", func() {
			a.locked(func() { a.respond(r, func(ctx context.Context) error { return r.Accept(ctx, nil) }) })
		})
		body = append(body, ui.Text{Common: ui.Common{Key: "url"}, Spans: []ui.Span{{T: r.URL, S: "link", Href: r.URL}}},
			actionsRow("act", []tern.Element{open, nil, decline, done}))
		return card(body...)
	}
	var pairs []ui.Pair
	for fi, f := range formFields(r.Schema) {
		field := f
		label := field.Title
		if field.Required {
			label += " *"
		}
		value := ui.Spans(ui.Span{T: "not set", S: "dim"})
		if v, ok := d.fields[field.Name]; ok {
			value = ui.T(toString(v))
		} else if field.Default != nil {
			value = ui.Spans(ui.Span{T: toString(field.Default)}, ui.Span{T: "  default", S: "dim"})
		}
		pairs = append(pairs, ui.Pair{K: ui.T(label), V: value})
		var buttons []tern.Element
		switch {
		case len(field.Enum) > 0:
			for oi, opt := range field.Enum {
				o := opt
				class := ""
				if toString(d.fields[field.Name]) == o {
					class = "primary"
				}
				buttons = append(buttons, button(fmt.Sprint("o", oi), o, class, func() { a.locked(func() { d.fields[field.Name] = o }) }))
			}
		case field.Type == "boolean":
			buttons = append(buttons,
				button("yes", "Yes", "", func() { a.locked(func() { d.fields[field.Name] = true }) }),
				button("no", "No", "", func() { a.locked(func() { d.fields[field.Name] = false }) }))
		default:
			class := ""
			if d.editing == field.Name {
				class = "primary"
			}
			buttons = append(buttons, button("edit", "Type "+strings.ToLower(field.Title), class, func() {
				a.locked(func() {
					d.editing = field.Name
					if v, ok := d.fields[field.Name]; ok {
						a.composer.set(toString(v))
					}
				})
			}))
		}
		body = append(body, actionsRow(fmt.Sprint("f", fi), buttons))
	}
	if len(pairs) > 0 {
		body = append([]tern.Element{body[0], ui.KV{Common: ui.Common{Key: "values"}, Items: pairs}}, body[1:]...)
	}
	body = append(body, actionsRow("act", []tern.Element{nil, decline,
		shortcutButton("accept", "Submit", "enter", "primary", func() { a.locked(func() { a.acceptFormLocked(r) }) })}))
	return card(body...)
}

// Questions. omp answers AskUserQuestion in the composer's place: tabs for
// several questions, a list of options with the focused one's preview, an
// "Other" row that takes typed text, and an action bar.

const otherRow = -1 // the cursor on the "Type your own answer" row

func (a *app) askRequestLocked() *agent.UserInputRequest {
	r, _ := a.firstRequestLocked().(*agent.UserInputRequest)
	return r
}

// askRows lists a question's rows: its options, then Other when free text
// is allowed.
func askRows(q agent.Question) []int {
	rows := make([]int, 0, len(q.Options)+1)
	for i := range q.Options {
		rows = append(rows, i)
	}
	if len(q.Options) == 0 || q.AllowOther {
		rows = append(rows, otherRow)
	}
	return rows
}

func (a *app) askNode(r *agent.UserInputRequest) tern.Element {
	d := a.draftFor(r.ID)
	reviewing := len(r.Questions) > 1 && d.q >= len(r.Questions)
	var kids []tern.Element
	if len(r.Questions) > 1 {
		var tabs []ui.Tab
		for i, q := range r.Questions {
			label := firstNonEmpty(q.Header, fmt.Sprintf("Question %d", i+1))
			if len(d.picks[q.ID]) > 0 {
				tabs = append(tabs, ui.Tab{ID: strconv.Itoa(i), Label: ui.Spans(ui.Span{T: "✓ ", S: "success"}, ui.Span{T: label})})
			} else {
				tabs = append(tabs, ui.Tab{ID: strconv.Itoa(i), Label: ui.T(label)})
			}
		}
		tabs = append(tabs, ui.Tab{ID: "submit", Label: ui.T("Submit")})
		active := strconv.Itoa(d.q)
		if reviewing {
			active = "submit"
		}
		kids = append(kids, ui.Row{Common: ui.Common{Key: "head", Role: "omp.ask.head"}, Align: ui.AlignCenter, Children: ui.Nodes(
			ui.Tabs{Common: ui.Common{Key: "tabs", Role: "omp.ask.questions", Title: "Next question  tab · Previous  shift+tab",
				OnAction: map[string]func(*tern.Action){"select": func(e *tern.Action) {
					a.locked(func() {
						if e.Value == "submit" {
							d.showQuestion(r, len(r.Questions))
						} else if n, err := strconv.Atoi(e.Value); err == nil && n >= 0 && n < len(r.Questions) {
							d.showQuestion(r, n)
						}
					})
				}}, Actions: &ui.Actions{Click: "select"}}, Items: tabs, Active: active})})
	}
	if reviewing {
		missing := 0
		var pairs []ui.Pair
		for _, q := range r.Questions {
			ans := strings.Join(d.picks[q.ID], ", ")
			if ans == "" {
				missing++
				ans = "—"
			}
			pairs = append(pairs, ui.Pair{K: ui.T(firstNonEmpty(q.Header, firstLine(q.Text))), V: ui.T(ans)})
		}
		notice := []ui.Span{{T: "All answered · Submit sends them"}}
		if missing > 0 {
			notice = []ui.Span{{T: fmt.Sprintf("%d unanswered", missing), S: "warning"}, {T: " · Submit still sends the rest"}}
		}
		kids = append(kids,
			ui.Text{Common: ui.Common{Key: "notice", Role: "omp.ask.notice"}, Spans: notice},
			ui.KV{Common: ui.Common{Key: "answers", Role: "omp.ask.answers"}, Items: pairs})
	} else {
		q := r.Questions[min(d.q, len(r.Questions)-1)]
		rows := askRows(q)
		d.cursor = max(0, min(d.cursor, len(rows)-1))
		kids = append(kids, ui.Md{Common: ui.Common{Key: "question", Role: "omp.ask.question"}, Text: q.Text})
		picked := d.picks[q.ID]
		var items []tern.Element
		for i, row := range rows {
			if row == otherRow {
				detail := ui.T("Type your own answer")
				if d.other != "" || d.typing {
					cur := d.other
					if d.typing {
						cur += "▏"
					}
					detail = ui.Spans(ui.Span{T: cur})
				}
				role := "omp.ask.other"
				if q.MultiSelect {
					role = "omp.ask.other.check"
				}
				it := ui.Item{Common: ui.Common{Key: fmt.Sprint("r", i), Role: role}, Label: ui.T("Other"), Detail: detail}
				if p := d.otherPick[q.ID]; p != "" && slices.Contains(picked, p) {
					it.Tone = ui.ToneAccent
				}
				items = append(items, it)
				continue
			}
			o := q.Options[row]
			role := "omp.ask.option"
			if q.MultiSelect {
				role = "omp.ask.check"
			}
			label := []ui.Span{{T: fmt.Sprintf("%d. ", i+1), S: "dim"}, {T: o.Label}}
			it := ui.Item{Common: ui.Common{Key: fmt.Sprint("r", i), Role: role}, Label: ui.Spans(label...), Detail: ui.T(o.Description)}
			if slices.Contains(picked, o.Label) {
				it.Tone = ui.ToneAccent
				if q.MultiSelect {
					it.Icon = "check"
				}
			}
			items = append(items, it)
		}
		// A click can arrive after the question changed; only rows of the
		// question still shown count.
		shown, nrows := d.q, len(rows)
		row := func(item string) (int, bool) {
			n, err := strconv.Atoi(strings.TrimPrefix(item[strings.LastIndexByte(item, '.')+1:], "r"))
			return n, err == nil && n >= 0 && n < nrows && d.q == shown && a.askRequestLocked() == r
		}
		kids = append(kids, ui.List{CommonWithoutMax: ui.CommonWithoutMax{Key: fmt.Sprint("q", d.q), Role: "omp.ask.options",
			OnActivate: func(e *tern.Activate) {
				a.locked(func() {
					if n, ok := row(e.Item); ok {
						d.cursor = n
						a.askEnterLocked(r)
					}
				})
			},
			OnSelect: func(e *tern.Select) {
				a.locked(func() {
					if n, ok := row(e.Item); ok {
						d.cursor = n
					}
				})
			}},
			Selected: fmt.Sprintf("dock.ask.q%d.r%d", d.q, d.cursor), Children: items})
		if row := rows[d.cursor]; row != otherRow && strings.TrimSpace(q.Options[row].Preview) != "" {
			kids = append(kids, ui.Md{Common: ui.Common{Key: "preview", Role: "omp.ask.preview"}, Text: q.Options[row].Preview})
		}
	}
	next := "Submit"
	if len(r.Questions) > 1 && !reviewing {
		next = "Next"
	}
	kids = append(kids, actionsRow("actions", []tern.Element{nil,
		shortcutButton("skip", "Skip", "esc", "", func() {
			a.locked(func() { a.respond(r, func(ctx context.Context) error { return r.Cancel(ctx) }) })
		}),
		shortcutButton("submit", next, "enter", "primary", func() {
			a.locked(func() {
				if a.askRequestLocked() == r {
					a.askConfirmLocked(r)
				}
			})
		}),
	}))
	return ui.Col{Common: ui.Common{Key: "ask", Role: "omp.editor.ask"}, Gap: ui.GapMD, Children: kids}
}

// askEnterLocked acts on the row under the cursor: picks it (and moves on
// for a single-choice question), or starts typing for Other. On the review
// tab it submits.
func (a *app) askEnterLocked(r *agent.UserInputRequest) {
	d := a.draftFor(r.ID)
	if d.q >= len(r.Questions) {
		a.submitAnswersLocked(r)
		return
	}
	q := r.Questions[d.q]
	rows := askRows(q)
	if d.cursor >= len(rows) {
		return
	}
	if rows[d.cursor] == otherRow {
		if !d.typing {
			d.typing = true
			return
		}
		d.typing = false
		t := strings.TrimSpace(d.other)
		d.other = t
		// The new text replaces the earlier Other answer.
		picks := slices.DeleteFunc(d.picks[q.ID], func(s string) bool { return s == d.otherPick[q.ID] || s == t })
		d.otherPick[q.ID] = t
		switch {
		case t == "":
			d.picks[q.ID] = picks
			delete(d.otherPick, q.ID)
			if !q.MultiSelect {
				return
			}
		case q.MultiSelect:
			d.picks[q.ID] = append(picks, t)
		default:
			d.picks[q.ID] = []string{t}
		}
	} else {
		label := q.Options[rows[d.cursor]].Label
		if q.MultiSelect {
			if i := slices.Index(d.picks[q.ID], label); i >= 0 {
				d.picks[q.ID] = slices.Delete(d.picks[q.ID], i, i+1)
			} else {
				d.picks[q.ID] = append(d.picks[q.ID], label)
			}
			return
		}
		d.picks[q.ID] = []string{label}
	}
	a.askAdvanceLocked(r)
}

// askAdvanceLocked moves to the next unanswered question, or submits when
// there is one question, or shows the review tab.
func (a *app) askAdvanceLocked(r *agent.UserInputRequest) {
	d := a.draftFor(r.ID)
	if len(r.Questions) == 1 {
		a.submitAnswersLocked(r)
		return
	}
	for i := d.q + 1; i < len(r.Questions); i++ {
		if len(d.picks[r.Questions[i].ID]) == 0 {
			d.showQuestion(r, i)
			return
		}
	}
	d.showQuestion(r, len(r.Questions))
}

// showQuestion moves to question q (len(questions) is the review tab),
// leaving any Other typing behind.
func (d *answerDraft) showQuestion(r *agent.UserInputRequest, q int) {
	d.q, d.cursor, d.typing, d.other = q, 0, false, ""
	if q < len(r.Questions) {
		d.other = d.otherPick[r.Questions[q].ID]
	}
}

// askConfirmLocked is Enter, and the Submit or Next button: in a multiple
// choice with picks it moves on, otherwise it acts on the row under the
// cursor.
func (a *app) askConfirmLocked(r *agent.UserInputRequest) {
	d := a.draftFor(r.ID)
	if d.q < len(r.Questions) && !d.typing {
		q := r.Questions[d.q]
		rows := askRows(q)
		if q.MultiSelect && len(d.picks[q.ID]) > 0 && (d.cursor >= len(rows) || rows[d.cursor] != otherRow) {
			a.askAdvanceLocked(r)
			return
		}
	}
	a.askEnterLocked(r)
}

// askOtherLocked starts or extends the Other answer with text.
func (a *app) askOtherLocked(r *agent.UserInputRequest, text string) {
	d := a.draftFor(r.ID)
	if d.q >= len(r.Questions) {
		return
	}
	rows := askRows(r.Questions[d.q])
	if rows[len(rows)-1] != otherRow {
		return
	}
	if !d.typing {
		d.cursor, d.typing = len(rows)-1, true
	}
	d.other += strings.ReplaceAll(text, "\n", " ")
}

// pasteIntoAskLocked pastes clipboard text into the Other answer.
func (a *app) pasteIntoAskLocked(r *agent.UserInputRequest) {
	d := a.draftFor(r.ID)
	q := d.q
	a.pasteClipboardText(func() bool { return a.askRequestLocked() == r && d.q == q }, func(text string) { a.askOtherLocked(r, text) })
}

// askKey drives the question in the composer. It reports whether it used
// the key.
func (a *app) askKey(r *agent.UserInputRequest, k tern.Key) bool {
	d := a.draftFor(r.ID)
	if d.typing {
		switch {
		case k.Is("enter"):
			a.askEnterLocked(r)
		case k.Is("escape"):
			d.typing = false
		case k.Is("backspace"):
			if rs := []rune(d.other); len(rs) > 0 {
				d.other = string(rs[:len(rs)-1])
			}
		case k.Name == "v" && (k.Ctrl || k.Meta || k.Alt):
			a.pasteIntoAskLocked(r)
		case k.Name == "paste" || (k.Text != "" && !k.Ctrl && !k.Meta):
			d.other += strings.ReplaceAll(k.Text, "\n", " ")
		default:
			return false
		}
		return true
	}
	n := len(r.Questions)
	var rows []int
	if d.q < n {
		rows = askRows(r.Questions[d.q])
	}
	switch {
	case k.Is("up"), k.Is("ctrl+p"):
		d.cursor = max(d.cursor-1, 0)
	case k.Is("down"), k.Is("ctrl+n"):
		d.cursor = min(d.cursor+1, max(len(rows)-1, 0))
	case k.Is("tab") && n > 1:
		d.showQuestion(r, (d.q+1)%(n+1))
	case k.Is("shift+tab") && n > 1:
		d.showQuestion(r, (d.q+n)%(n+1))
	case k.Is("space") && d.q < n && r.Questions[d.q].MultiSelect:
		a.askEnterLocked(r)
	case k.Is("enter"):
		a.askConfirmLocked(r)
	case k.Name == "v" && (k.Ctrl || k.Meta || k.Alt):
		a.pasteIntoAskLocked(r)
	case k.Name == "paste":
		a.askOtherLocked(r, k.Text)
	case k.Is("escape"):
		a.respond(r, func(ctx context.Context) error { return r.Cancel(ctx) })
	default:
		if v, err := strconv.Atoi(k.Text); err == nil && v >= 1 && v <= len(rows) && !k.Ctrl && !k.Alt {
			d.cursor = v - 1
			a.askEnterLocked(r)
			return true
		}
		if k.Text != "" && !k.Ctrl && !k.Meta && len(rows) > 0 && rows[len(rows)-1] == otherRow {
			// Typing anything else starts an Other answer.
			d.other = ""
			a.askOtherLocked(r, k.Text)
			return true
		}
		return false
	}
	return true
}

// Plan approval. omp shows the plan in a large sheet with its options
// below; "keep planning" hands the composer over for feedback.

func (a *app) planRequestLocked() *agent.PermissionRequest {
	if r, ok := a.firstRequestLocked().(*agent.PermissionRequest); ok && r.Kind == agent.PermissionPlanApproval && a.planFeedback != r.ID {
		return r
	}
	return nil
}

var planChoices = []struct{ label, hint string }{
	{"Yes, and auto-accept edits", "Claude edits files without asking"},
	{"Yes, and ask before each edit", "Claude asks before changing files"},
	{"No, keep planning", "Tell Claude what to change"},
}

func (a *app) planOverlay(r *agent.PermissionRequest) tern.Element {
	var items []tern.Element
	for i, c := range planChoices {
		it := ui.Item{Common: ui.Common{Key: fmt.Sprint("o", i)}, Label: ui.T(c.label), Detail: ui.T(c.hint)}
		if i == a.planCursor {
			it.Hint = []string{"enter"}
		}
		items = append(items, it)
	}
	choose := func(item string) {
		a.locked(func() {
			if n, err := strconv.Atoi(strings.TrimPrefix(item[strings.LastIndexByte(item, '.')+1:], "o")); err == nil {
				a.planCursor = n
				a.planChooseLocked(r)
			}
		})
	}
	return ui.Overlay{Common: ui.Common{Key: "plan", Role: "omp.overlay.planReview"}, Anchor: ui.AnchorCenter, Size: ui.SizeLG, Modal: true,
		Head: ui.T("Ready to code? Review the plan"), Children: ui.Nodes(
			ui.Row{Common: ui.Common{Key: "tools", Role: "omp.plan.tools"}, Justify: ui.JustifyEnd, Gap: ui.GapSM, Children: ui.Nodes(
				shortcutButton("keep", "Keep planning", "esc", "", func() { a.locked(func() { a.planCursor = 2; a.planChooseLocked(r) }) }),
			)},
			ui.Col{Common: ui.Common{Key: "body", Role: "omp.plan.body", Grow: 1}, Gap: ui.GapMD, Children: ui.Nodes(
				ui.Md{Common: ui.Common{Key: "plan", Actions: &ui.Actions{Menu: []string{"copy"}}}, Text: r.Plan})},
			ui.List{CommonWithoutMax: ui.CommonWithoutMax{Key: "options", Role: "omp.plan.options",
				OnActivate: func(e *tern.Activate) { choose(e.Item) }, OnSelect: func(e *tern.Select) { choose(e.Item) }},
				Selected: fmt.Sprintf("layer.plan.options.o%d", a.planCursor), Children: items},
			hintsRow("hints", hint{"up down", "choose"}, hint{"enter", "confirm"}, hint{"esc", "keep planning"}),
		)}
}

func (a *app) planChooseLocked(r *agent.PermissionRequest) {
	switch a.planCursor {
	case 0:
		a.approvePlanLocked(r, agent.PermissionAcceptEdits)
	case 1:
		a.approvePlanLocked(r, agent.PermissionDefault)
	default:
		// Feedback goes through the composer; Enter sends it as the refusal.
		a.planFeedback = r.ID
	}
	a.planCursor = 0
}

func (a *app) planKey(r *agent.PermissionRequest, k tern.Key) bool {
	switch {
	case k.Is("up"), k.Is("k"), k.Is("ctrl+p"):
		a.planCursor = max(a.planCursor-1, 0)
	case k.Is("down"), k.Is("j"), k.Is("ctrl+n"):
		a.planCursor = min(a.planCursor+1, len(planChoices)-1)
	case k.Is("enter"):
		a.planChooseLocked(r)
	case k.Is("escape"):
		a.planCursor = 2
		a.planChooseLocked(r)
	default:
		if v, err := strconv.Atoi(k.Text); err == nil && v >= 1 && v <= len(planChoices) {
			a.planCursor = v - 1
			a.planChooseLocked(r)
			return true
		}
		return false
	}
	return true
}

// planFeedbackLocked sends the composer's text as the reason to keep
// planning.
func (a *app) planFeedbackLocked(text string) bool {
	if a.planFeedback == "" {
		return false
	}
	for _, req := range a.requests {
		if p, ok := req.(*agent.PermissionRequest); ok && p.ID == a.planFeedback {
			a.planFeedback = ""
			resp := agent.PermissionResponse{Decision: agent.DecisionDeny, Message: text}
			a.respond(p, func(ctx context.Context) error { return p.Respond(ctx, resp) })
			return true
		}
	}
	a.planFeedback = ""
	return false
}
