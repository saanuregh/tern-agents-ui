package chat

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf16"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/claude"
	"github.com/saanuregh/agent-cli-connector/codex"
	"github.com/stencil-hq/tern-sdk/go/tern"
)

// modeCycle is what Shift+Tab steps through, as in Claude Code.
var modeCycle = []agent.PermissionMode{agent.PermissionDefault, agent.PermissionAcceptEdits, agent.PermissionPlan}

var imageExts = []string{".png", ".jpg", ".jpeg", ".gif", ".webp"}

// key handles one key press. It returns tern.ErrStop to quit.
func (a *app) key(k tern.Key) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	defer a.changed()
	if a.menu != nil {
		a.menuKey(k)
		return nil
	}
	if a.rewind != nil {
		a.rewindKey(k)
		return nil
	}
	if a.prefs != nil && a.menu == nil {
		a.prefsKey(k)
		return nil
	}
	// The plan sheet is modal: what it doesn't use must not reach the
	// composer behind it. Ctrl+C still interrupts or quits.
	if r := a.planRequestLocked(); r != nil && !k.Is("ctrl+c") {
		a.planKey(r, k)
		return nil
	}
	if r := a.askRequestLocked(); r != nil && a.menu == nil {
		if k.Is("ctrl+c") && a.running() {
			a.interruptLocked()
			return nil
		}
		a.askKey(r, k)
		return nil
	}
	if r := a.renaming; r != nil && k.Is("escape") {
		a.renaming = nil
		a.composer.set(r.draft)
		return nil
	}
	if a.planFeedback != "" && k.Is("escape") && len(a.composer.text) == 0 {
		a.planFeedback = ""
		return nil
	}
	if a.showContext && k.Is("escape") {
		a.showContext = false
		return nil
	}
	c := &a.composer
	empty := len(c.text) == 0
	switch {
	case k.Is("ctrl+c"):
		switch {
		case !empty:
			c.reset()
		case time.Since(a.quitting) < 2*time.Second:
			// Twice quits, even when an interrupt didn't take.
			return tern.ErrStop
		case a.running():
			a.interruptLocked()
			a.quitting = time.Now()
		default:
			a.quitting = time.Now()
			a.toastLocked("info", "Press Ctrl+C again to quit")
		}
		a.complete.close()
		return nil
	case k.Is("ctrl+d") && empty:
		return tern.ErrStop
	}

	if a.complete.open() {
		switch {
		case k.Is("up"), k.Is("ctrl+p"):
			a.complete.sel = (a.complete.sel - 1 + len(a.complete.items)) % len(a.complete.items)
			return nil
		case k.Is("down"), k.Is("ctrl+n"):
			a.complete.sel = (a.complete.sel + 1) % len(a.complete.items)
			return nil
		case k.Is("tab"):
			a.acceptCompletionLocked()
			return nil
		case k.Is("enter") && a.complete.kind == "command" && a.exactCommandLocked(a.complete.query):
			a.complete.close()
			a.submitLocked(agent.DeliverQueue)
			return nil
		case k.Is("enter"):
			value := a.complete.items[a.complete.sel].Value
			kind := a.complete.kind
			if a.acceptCompletionLocked() {
				// A command that takes no arguments runs at once.
				if kind == "command" && !strings.HasSuffix(value, " ") {
					a.submitLocked(agent.DeliverQueue)
				}
				return nil
			}
		case k.Is("escape"):
			a.complete.close()
			return nil
		}
	}

	if empty && a.requestKeyLocked(k) {
		return nil
	}

	switch {
	case k.Name == "paste" && strings.TrimSpace(k.Text) == "":
		// What a terminal pastes for an image on the clipboard.
		a.pasteClipboard()
	case k.Name == "paste":
		a.pasteTextLocked(k.Text)
	case k.Is("ctrl+b") && a.backend.Has(featBackground) && a.running():
		a.backgroundLocked()
	case k.Name == "v" && (k.Ctrl || k.Meta || k.Alt):
		a.pasteClipboard()
	case k.Is("enter"):
		a.submitLocked(agent.DeliverQueue)
	case k.Is("ctrl+enter"):
		a.submitLocked(agent.DeliverSteer)
	case k.Is("shift+enter"), k.Is("alt+enter"), k.Is("ctrl+j"):
		c.insert("\n")
	case k.Is("shift+tab"):
		a.cycleModeLocked()
	case (k.Is("tab") || k.Is("right")) && empty && a.suggestion != "" && !a.running():
		c.set(a.suggestion)
		a.suggestion = ""
	case k.Is("tab"):
		a.refreshCompletionLocked(true)
		return nil
	case k.Is("escape"):
		switch {
		case a.running():
			a.interruptLocked()
		case empty && time.Since(a.lastEsc) < time.Second:
			a.startRewindLocked("")
		default:
			a.lastEsc = time.Now()
		}
		return nil
	case k.Is("ctrl+o"):
		a.expandAll = !a.expandAll
		return nil
	case k.Is("ctrl+t"):
		a.showPlan = !a.showPlan
		return nil
	case k.Is("ctrl+z"):
		c.doUndo()
	case k.Is("ctrl+y"), k.Is("ctrl+shift+z"):
		c.doRedo()
	case k.Is("backspace") && empty && len(a.attachments) > 0:
		a.attachments = a.attachments[:len(a.attachments)-1]
	case k.Is("backspace"):
		c.deleteRange(c.prevChar(), c.cursor)
	case k.Is("alt+backspace"), k.Is("ctrl+backspace"), k.Is("ctrl+w"):
		c.deleteRange(c.prevWord(), c.cursor)
	case k.Is("delete"), k.Is("ctrl+d"):
		c.deleteRange(c.cursor, c.nextChar())
	case k.Is("alt+delete"), k.Is("ctrl+delete"), k.Is("alt+d"):
		c.deleteRange(c.cursor, c.nextWord())
	case k.Is("ctrl+u"):
		c.deleteRange(c.lineStart(), c.cursor)
	case k.Is("ctrl+k"):
		end := c.lineEnd()
		if end == c.cursor {
			end = c.nextChar()
		}
		c.deleteRange(c.cursor, end)
	case k.Is("left"), k.Is("ctrl+b"):
		c.cursor = c.prevChar()
	case k.Is("right"), k.Is("ctrl+f"):
		c.cursor = c.nextChar()
	case k.Is("alt+left"), k.Is("ctrl+left"), k.Is("alt+b"):
		c.cursor = c.prevWord()
	case k.Is("alt+right"), k.Is("ctrl+right"), k.Is("alt+f"):
		c.cursor = c.nextWord()
	case k.Is("home"), k.Is("ctrl+a"):
		c.cursor = c.lineStart()
	case k.Is("end"), k.Is("ctrl+e"):
		c.cursor = c.lineEnd()
	case k.Is("ctrl+home"):
		c.cursor = 0
	case k.Is("ctrl+end"):
		c.cursor = len(c.text)
	case k.Is("up"):
		if !c.moveLine(-1) {
			c.browse(-1)
		}
	case k.Is("down"):
		if !c.moveLine(1) {
			c.browse(1)
		}
	case k.Text != "" && !k.Ctrl && !k.Meta:
		c.insert(k.Text)
	default:
		return nil
	}
	a.refreshCompletionLocked(false)
	return nil
}

// edit applies a native edit event from Tern's selection handling.
func (a *app) edit(e *tern.Edit) {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Tern computed the edit on the text it last saw; if the program has
	// changed it since (a paste, a restored draft), the range is stale.
	// The redraw shows Tern the current text instead.
	if e.Len != len(utf16.Encode(a.composer.text)) {
		a.changed()
		return
	}
	a.composer.edit(e.From, e.To, e.Text, e.Cursor)
	a.refreshCompletionLocked(false)
	a.changed()
}

func (a *app) undo() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.composer.doUndo()
	a.changed()
}

// sendText handles text Tern submits atomically into the composer.
func (a *app) sendText(text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.composer.set(text)
	// Text Tern sends is never a shell command: Codex runs those unsandboxed.
	a.fromTern = true
	a.submitLocked(agent.DeliverQueue)
	a.fromTern = false
	a.changed()
}

// submitLocked sends the draft: to a pending question or form field first,
// then to a slash command, else to Claude.
func (a *app) submitLocked(delivery agent.Delivery) {
	text := strings.TrimSpace(a.composer.String())
	if text == "" && len(a.attachments) == 0 {
		return
	}
	if r := a.renaming; r != nil {
		a.renameLocked(text)
		a.composer.set(r.draft)
		return
	}
	if text != "" && a.planFeedbackLocked(text) {
		a.composer.reset()
		return
	}
	if text != "" && a.answerFromComposerLocked(text) {
		a.composer.reset()
		return
	}
	if text != "" {
		a.composer.remember(text)
	}
	a.composer.reset()
	a.complete.close()
	if cmd, ok := shellCommand(a, text); ok && !a.fromTern && len(a.attachments) == 0 {
		a.shellLocked(cmd)
		return
	}
	switch {
	case strings.HasPrefix(text, "//") && len(text) > 2 && text[2] != ' ' && text[2] != '/':
		// //name runs Claude Code's command where a local one has the name.
		text = text[1:]
	case strings.HasPrefix(text, "/"):
		name, arg, _ := strings.Cut(text[1:], " ")
		if a.runCommandLocked(strings.ToLower(name), strings.TrimSpace(arg)) {
			return
		}
	}
	a.sendLocked(text, delivery)
}

func (a *app) sendLocked(text string, delivery agent.Delivery) {
	if a.sess == nil && a.deferred != nil {
		// The message shows while the agent starts.
		const standIn = "first-message"
		a.entries[standIn] = &entry{item: agent.Item{ID: standIn, Kind: agent.ItemUserMessage, Status: agent.ItemDone, Text: text}, ver: 1}
		a.order = append(a.order, standIn)
		a.launchLocked(func(started bool) {
			delete(a.entries, standIn)
			a.order = slices.DeleteFunc(a.order, func(o string) bool { return o == standIn })
			if !started {
				a.composer.set(text)
				return
			}
			a.sendLocked(text, delivery)
		})
		return
	}
	sess, gen, err := a.liveSession()
	if err != nil {
		a.toastLocked("error", err.Error())
		a.composer.set(text)
		return
	}
	queue := a.running() && delivery == agent.DeliverQueue
	atts := a.attachments
	a.attachments = nil
	parts := attachmentParts(atts)
	if text != "" {
		parts = append(messageParts(text, a.cwd), parts...)
	}
	in := agent.Input{Parts: parts, Delivery: delivery, ClientID: newClientID()}
	a.suggestion = ""
	if len(atts) > 0 {
		a.sentAttachments[in.ClientID] = atts
	}
	go func() {
		turn, err := sess.Send(a.ctx, in)
		a.mu.Lock()
		defer a.mu.Unlock()
		defer a.changed()
		if gen != a.gen {
			return
		}
		if err != nil {
			a.toastLocked("error", "Send failed: "+err.Error())
			if len(a.composer.text) == 0 {
				a.composer.set(text)
			}
			if len(a.attachments) == 0 {
				a.attachments = atts
			}
			return
		}
		a.sent = true
		// The turn may have started (or ended) before Send returned.
		if id := turn.ID(); queue && !a.turnSeen[id] {
			a.queued[id] = true
			if it, ok := a.transcript.Item(id); ok {
				a.upsertLocked(it)
			} else {
				// Codex announces a queued message only when it runs, under
				// a new id; until then the queue shows this stand-in.
				a.entries[id] = &entry{item: agent.Item{ID: id, Kind: agent.ItemUserMessage, Status: agent.ItemDone, Text: text}, ver: 1}
				a.order = append(a.order, id)
				a.queuedTurns[id] = turn
			}
		}
	}()
}

// messageParts turns @mentions of image files into image parts; other
// @paths stay in the text, which Claude Code resolves itself.
func messageParts(text, cwd string) []agent.Part {
	parts := []agent.Part{agent.TextPart(text)}
	for _, word := range strings.Fields(text) {
		if !strings.HasPrefix(word, "@") || len(word) < 2 {
			continue
		}
		path := strings.TrimRight(word[1:], ".,;:!?)")
		if !slices.Contains(imageExts, strings.ToLower(filepath.Ext(path))) {
			continue
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() {
			parts = append(parts, agent.ImageFile(path))
		}
	}
	return parts
}

// interruptLocked stops the running turn. As in Claude Code, queued
// messages are not sent after it: they come back to the composer.
func (a *app) interruptLocked() {
	sess, gen, err := a.liveSession()
	if err != nil {
		return
	}
	if cx, ok := sess.(*codex.Session); ok && len(a.queued) > 0 {
		// Codex holds queued input itself; clear it, then stop the turn.
		cx.ClearQueue()
		a.restoreQueuedLocked(nil, true)
		a.async("Interrupt", func(ctx context.Context) error { return sess.Interrupt(ctx) })
		return
	}
	cs, ok := sess.(*claude.Session)
	if !ok || len(a.queued) == 0 {
		a.async("Interrupt", func(ctx context.Context) error { return sess.Interrupt(ctx) })
		return
	}
	a.async("Interrupt", func(ctx context.Context) error {
		receipt, err := cs.InterruptWithOptions(ctx, true)
		if err != nil {
			return err
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		defer a.changed()
		if gen != a.gen {
			return nil
		}
		a.restoreQueuedLocked(receipt.Cancelled, false)
		return nil
	})
}

// restoreQueuedLocked takes cancelled queued messages (ids, or every
// queued one with all) off the queue and back into the composer, ahead of
// any draft.
func (a *app) restoreQueuedLocked(ids []string, all bool) {
	gone := func(id string) bool { return a.queued[id] && (all || slices.Contains(ids, id)) }
	var texts []string
	for _, id := range a.order {
		if e := a.entries[id]; e != nil && gone(id) {
			texts = append(texts, e.item.Text)
		}
	}
	a.order = slices.DeleteFunc(a.order, gone)
	for id := range a.queued {
		if all || slices.Contains(ids, id) {
			delete(a.queued, id)
			delete(a.entries, id)
			delete(a.queuedTurns, id)
		}
	}
	if len(texts) > 0 {
		if draft := a.composer.String(); draft != "" {
			texts = append(texts, draft)
		}
		a.composer.set(strings.Join(texts, "\n\n"))
	}
}

func (a *app) cycleModeLocked() {
	modes := modeCycle
	if a.autoModeLocked() {
		modes = append(slices.Clone(modeCycle), agent.PermissionAuto)
	}
	i := slices.Index(modes, a.mode)
	a.setModeLocked(modes[(i+1)%len(modes)])
}

// autoModeLocked reports whether auto mode (a classifier approves each
// call) is offered: the session accepts it and the model, when Claude says,
// supports it.
func (a *app) autoModeLocked() bool {
	if a.sess != nil && !slices.Contains(a.sess.Capabilities().PermissionModes, agent.PermissionAuto) {
		return false
	}
	cs, err := a.claudeSession()
	if err != nil {
		return true
	}
	for _, m := range cs.SupportedModels() {
		if m.Value == a.model || m.ResolvedModel == a.model {
			return m.SupportsAutoMode
		}
	}
	return true
}

func (a *app) setModeLocked(mode agent.PermissionMode) {
	prev := a.mode
	a.mode = mode
	sess, gen, err := a.liveSession()
	if err != nil {
		a.pending.mode = mode
		return
	}
	a.async("Permission mode", func(ctx context.Context) error {
		if err := sess.SetPermissionMode(ctx, mode); err != nil {
			a.mu.Lock()
			if gen == a.gen {
				a.mode = prev
			}
			a.mu.Unlock()
			a.changed()
			return err
		}
		return nil
	})
}

// refreshCompletionLocked opens, updates or closes the suggestion list for
// the word under the cursor. explicit (Tab) also opens it for an empty "@".
func (a *app) refreshCompletionLocked(explicit bool) {
	token, start := a.composer.token()
	switch {
	case start == 0 && strings.HasPrefix(token, "/") && !strings.ContainsRune(a.composer.String(), ' '):
		a.complete = completion{kind: "command", query: token, items: a.commandSuggestionsLocked(token[1:])}
		if len(a.complete.items) == 1 && explicit {
			a.acceptCompletionLocked()
		}
	case strings.HasPrefix(token, "@") && (len(token) > 1 || explicit):
		if a.complete.kind != "file" || a.complete.query != token {
			// Until the new results come, keep only earlier ones that still match.
			q := strings.ToLower(token[1:])
			if a.complete.kind != "file" {
				a.complete.items = nil
			}
			a.complete.items = slices.DeleteFunc(a.complete.items, func(it suggestion) bool { return !strings.Contains(strings.ToLower(it.Label), q) })
			a.complete.kind, a.complete.query, a.complete.sel = "file", token, 0
			a.fileSuggestionsLocked(token)
		}
	default:
		a.complete.close()
	}
}

func (a *app) fileSuggestionsLocked(token string) {
	sess, gen, cwd := a.sess, a.gen, a.cwd
	go func() {
		ctx, cancel := context.WithTimeout(a.ctx, 5*time.Second)
		defer cancel()
		var paths []string
		if sess != nil {
			var err error
			if paths, err = a.backend.FileSuggestions(ctx, sess, token[1:], cwd); err != nil {
				a.logger.Warn("file suggestions", "err", err)
			}
		}
		if len(paths) == 0 {
			// The agent builds its file index lazily; search the tree meanwhile.
			paths = localFiles(cwd, token[1:], 12)
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if gen != a.gen || a.complete.kind != "file" || a.complete.query != token {
			return
		}
		items := make([]suggestion, 0, min(len(paths), 12))
		for _, p := range paths[:min(len(paths), 12)] {
			items = append(items, suggestion{Value: "@" + p + " ", Label: p})
		}
		a.complete.items, a.complete.sel = items, 0
		a.changed()
	}()
}

// exactCommandLocked reports whether token names a command exactly.
func (a *app) exactCommandLocked(token string) bool {
	name := strings.TrimPrefix(token, "/")
	for _, c := range a.commandsLocked() {
		if c.Name == name {
			return true
		}
	}
	for _, c := range a.info.Commands {
		if c.Name == name {
			return true
		}
	}
	return false
}

// acceptCompletionLocked puts the selected suggestion in place of the
// token. It reports false when there is nothing to accept, so Enter sends.
func (a *app) acceptCompletionLocked() bool {
	if !a.complete.open() {
		return false
	}
	s := a.complete.items[a.complete.sel]
	if strings.TrimSpace(s.Value) == strings.TrimSpace(a.complete.query) {
		a.complete.close()
		return false
	}
	a.composer.replaceToken(s.Value)
	a.complete.close()
	return true
}

// skipDirs are never searched for @file suggestions.
var skipDirs = map[string]bool{".git": true, "node_modules": true, "target": true, "dist": true, ".venv": true, "vendor": true}

// localFiles finds up to limit paths under root whose relative path
// contains query (case-insensitive), visiting at most 5000 entries.
func localFiles(root, query string, limit int) []string {
	query = strings.ToLower(query)
	var out []string
	seen := 0
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || len(out) >= limit || seen > 5000 {
			return filepath.SkipDir
		}
		seen++
		if d.IsDir() {
			if p != root && (skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err == nil && strings.Contains(strings.ToLower(rel), query) {
			out = append(out, rel)
		}
		return nil
	})
	return out
}
