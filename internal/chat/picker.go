package chat

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"

	"github.com/stencil-hq/tern-sdk/go/tern"
	"github.com/stencil-hq/tern-sdk/go/tern/ui"
)

// menu is a choice drawn as Tern's native picker sheet: a search line you
// type into, rows with match highlights, facts and a current check, and a
// preview pane. Keys are ours: arrows move, Enter picks, Esc closes.
type menu struct {
	title    string
	subtitle string
	noun     string
	icon     string
	items    []choice
	columns  []ui.PickerColumn
	sel      int // index into visible()
	filter   string
	preview  bool
	actions  []menuAction // more than choosing, on the selected row
	armed    string       // the confirming action and row, pressed once
	armedAt  time.Time    // when; the second press must follow soon
}

// confirmWindow is how long a destructive action waits for its second press.
const confirmWindow = 5 * time.Second

// menuAction acts on the selected row; with confirm it needs a second
// press, for what can't be undone.
type menuAction struct {
	id, label, key string
	confirm        bool
	run            func(item int)
}

// actLocked runs action act on the selected row.
func (a *app) actLocked(m *menu, act menuAction) {
	vis := m.visible()
	if m.sel >= len(vis) {
		return
	}
	i := vis[m.sel]
	if armed := fmt.Sprint(act.id, "#", i); act.confirm && (m.armed != armed || time.Since(m.armedAt) > confirmWindow) {
		m.armed, m.armedAt = armed, time.Now()
		a.toastLocked("warning", fmt.Sprintf("Press %s again to %s “%s”", act.key, strings.ToLower(act.label), m.items[i].Label))
		return
	}
	m.armed = ""
	act.run(i)
}

func (m *menu) visible() []int {
	var idx []int
	f := strings.ToLower(strings.TrimSpace(m.filter))
	for i, c := range m.items {
		if f == "" || strings.Contains(strings.ToLower(c.Label+" "+c.Detail), f) {
			idx = append(idx, i)
		}
	}
	return idx
}

func (a *app) openMenuLocked(title string, items []choice) {
	a.openPickerLocked(&menu{title: title, items: items})
}

func (a *app) openPickerLocked(m *menu) {
	if len(m.items) == 0 {
		a.toastLocked("info", m.title+": nothing to choose")
		return
	}
	for _, c := range m.items {
		m.preview = m.preview || c.Preview != ""
	}
	for i, c := range m.items {
		if c.Current {
			m.sel = i
			break
		}
	}
	a.menu = m
	a.changed()
}

func (a *app) pickLocked(i int) {
	m := a.menu
	if m == nil || i < 0 || i >= len(m.items) {
		return
	}
	a.menu = nil
	if run := m.items[i].Run; run != nil {
		run()
	}
}

func (a *app) menuKey(k tern.Key) {
	m := a.menu
	vis := m.visible()
	switch {
	case k.Is("escape"), k.Is("ctrl+c"):
		a.menu = nil
	case k.Is("up"), k.Is("ctrl+p"):
		m.sel = max(m.sel-1, 0)
	case k.Is("down"), k.Is("ctrl+n"):
		m.sel = min(m.sel+1, max(len(vis)-1, 0))
	case k.Is("page_up"):
		m.sel = max(m.sel-10, 0)
	case k.Is("page_down"):
		m.sel = min(m.sel+10, max(len(vis)-1, 0))
	case k.Is("home"):
		m.sel = 0
	case k.Is("end"):
		m.sel = max(len(vis)-1, 0)
	case k.Is("enter"):
		if m.sel < len(vis) {
			a.pickLocked(vis[m.sel])
		}
	case k.Is("backspace"):
		if r := []rune(m.filter); len(r) > 0 {
			m.filter, m.sel = string(r[:len(r)-1]), 0
		}
	case k.Is("ctrl+u"):
		m.filter, m.sel = "", 0
	case k.Name == "v" && (k.Ctrl || k.Meta || k.Alt):
		a.pasteClipboardText(func() bool { return a.menu == m }, func(text string) {
			m.filter, m.sel = m.filter+strings.ReplaceAll(text, "\n", " "), 0
		})
	case k.Name == "paste" || (k.Text != "" && !k.Ctrl && !k.Meta):
		m.filter += strings.ReplaceAll(k.Text, "\n", " ")
		m.sel = 0
	default:
		for _, act := range m.actions {
			if k.Is(act.key) {
				a.actLocked(m, act)
			}
		}
	}
}

// foldedMatch finds sub, already lower-cased, in s ignoring case, and
// returns the match as UTF-16 offsets into s. It folds rune by rune, so
// offsets stay right where lower-casing changes a rune's byte length.
func foldedMatch(s, sub string) (from, to int, ok bool) {
	rs, want := []rune(s), []rune(sub)
	if len(want) == 0 {
		return 0, 0, false
	}
	for i := 0; i+len(want) <= len(rs); i++ {
		match := true
		for j, w := range want {
			if unicode.ToLower(rs[i+j]) != w {
				match = false
				break
			}
		}
		if match {
			from = len(utf16.Encode(rs[:i]))
			return from, from + len(utf16.Encode(rs[i:i+len(want)])), true
		}
	}
	return 0, 0, false
}

// pickerNode draws the open menu.
func (a *app) pickerNode() tern.Element {
	m := a.menu
	vis := m.visible()
	m.sel = max(0, min(m.sel, len(vis)-1))
	items := make([]ui.PickerItem, 0, len(m.items))
	var order []ui.OrderEntry
	var current []string
	hits := map[string][][2]int{}
	f := strings.ToLower(strings.TrimSpace(m.filter))
	for _, i := range vis {
		c := m.items[i]
		id := fmt.Sprint("o", i)
		it := ui.PickerItem{ID: id, Label: ui.T(c.Label), Icon: ui.IconName(c.Icon), Facts: c.Facts, Mono: c.Mono, Dot: ui.Tone(c.Tone)}
		if c.Detail != "" {
			it.Detail = ui.Spans(ui.Span{T: c.Detail, S: "muted"})
		}
		for _, b := range c.Badges {
			it.Badges = append(it.Badges, ui.Chip{Text: b})
		}
		if f != "" {
			if from, to, ok := foldedMatch(c.Label, f); ok {
				hits[id] = [][2]int{{from, to}}
			}
		}
		if c.Current {
			current = append(current, id)
		}
		items = append(items, it)
		order = append(order, ui.OrderEntry{ID: id})
	}
	sel := ""
	var preview []tern.Element
	if len(vis) > 0 {
		sel = fmt.Sprint("o", vis[m.sel])
		if p := m.items[vis[m.sel]].Preview; p != "" {
			preview = ui.Nodes(ui.Md{Common: ui.Common{Key: "preview"}, Text: p})
		}
	}
	query := m.filter
	cursor := len(utf16.Encode([]rune(query)))
	size, pane := ui.SizeMD, ui.PickerPane("none")
	if m.preview {
		size, pane = ui.SizeLG, ui.PickerPane("side")
	}
	idOf := func(item string) int {
		var n int
		if _, err := fmt.Sscanf(item[strings.LastIndexByte(item, '.')+1:], "o%d", &n); err != nil {
			return -1
		}
		return n
	}
	p := ui.Picker{
		PickerCommon: ui.PickerCommon{Key: "menu",
			OnSelect: func(e *tern.Select) {
				a.locked(func() {
					if a.menu == nil {
						return
					}
					for vi, i := range a.menu.visible() {
						if i == idOf(e.Item) {
							a.menu.sel = vi
						}
					}
				})
			},
			OnActivate: func(e *tern.Activate) { a.locked(func() { a.pickLocked(idOf(e.Item)) }) },
			OnAction: map[string]func(*tern.Action){
				"close": func(*tern.Action) { a.locked(func() { a.menu = nil }) },
				"clear": func(*tern.Action) {
					a.locked(func() {
						if a.menu != nil {
							a.menu.filter, a.menu.sel = "", 0
						}
					})
				},
				"choose": func(*tern.Action) {
					a.locked(func() {
						if a.menu != nil {
							if v := a.menu.visible(); a.menu.sel < len(v) {
								a.pickLocked(v[a.menu.sel])
							}
						}
					})
				},
			}},
		Size: size, Preview: pane, Title: ui.T(m.title), Subtitle: ui.T(m.subtitle), Icon: ui.IconName(firstNonEmpty(m.icon, "list")),
		Query: &query, Cursor: &cursor, Noun: firstNonEmpty(m.noun, "items"), Total: ptr(len(m.items)),
		Items: items, Order: order, Selected: sel, Current: current, Hits: hits, Columns: m.columns,
		Actions:  a.pickerActions(m),
		Children: preview,
	}
	for _, act := range m.actions {
		p.OnAction[act.id] = func(*tern.Action) {
			a.locked(func() {
				if a.menu == m {
					a.actLocked(m, act)
				}
			})
		}
	}
	return p
}

// pickerActions are the sheet's buttons: the menu's own, then close and
// choose.
func (a *app) pickerActions(m *menu) []ui.PickerAction {
	var out []ui.PickerAction
	for _, act := range m.actions {
		out = append(out, ui.PickerAction{ID: act.id, Label: act.label, Keys: []string{act.key}})
	}
	return append(out, ui.PickerAction{ID: "close", Label: "Close", Keys: []string{"esc"}},
		ui.PickerAction{ID: "choose", Label: "Choose", Keys: []string{"enter"}, Primary: true})
}

func ptr[T any](v T) *T { return &v }
