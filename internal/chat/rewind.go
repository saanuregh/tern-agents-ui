package chat

import (
	"context"
	"fmt"
	"slices"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/claude"
	"github.com/stencil-hq/tern-sdk/go/tern"
	"github.com/stencil-hq/tern-sdk/go/tern/ui"
)

// rewindState is the in-place rewind, as omp does it: the transcript marks
// the chosen message and dims what would be dropped, and the composer
// becomes a bar. Claude Code can also put files back as they were at that
// message (file checkpoints), so the bar offers conversation, code or both.
type rewindState struct {
	targets []string // user message ids, oldest first
	sel     int
	restore string // "conversation", "code" or "both"
	files   *claude.RewindFilesResult
	filesAt string // the message files was computed for
}

var restoreModes = []struct{ id, label string }{
	{"both", "Code and conversation"},
	{"conversation", "Conversation only"},
	{"code", "Code only"},
}

// restoreModesLocked are the choices the agent supports.
func (a *app) restoreModesLocked() []struct{ id, label string } {
	if a.backend.Has(featFileRewind) {
		return restoreModes
	}
	return restoreModes[1:2]
}

// startRewindLocked opens the rewind at message id, or at the newest one.
func (a *app) startRewindLocked(id string) {
	if a.running() {
		a.toastLocked("info", "Rewind once "+a.backend.Name()+" finishes, or press Esc to stop it")
		return
	}
	var targets []string
	for _, oid := range a.order {
		if e := a.entries[oid]; e != nil && e.local == nil && e.item.Kind == agent.ItemUserMessage && !e.item.Synthetic && !a.queued[oid] {
			targets = append(targets, oid)
		}
	}
	if len(targets) == 0 {
		a.toastLocked("info", "Nothing to rewind to yet")
		return
	}
	sel := len(targets) - 1
	if i := slices.Index(targets, id); i >= 0 {
		sel = i
	}
	a.rewind = &rewindState{targets: targets, sel: sel, restore: "conversation"}
	a.rewindDryRunLocked()
	a.revealRewind = true
	a.changed()
}

func (a *app) rewindTarget() string {
	if a.rewind == nil {
		return ""
	}
	return a.rewind.targets[a.rewind.sel]
}

// rewindDryRunLocked asks which files the chosen message would restore.
func (a *app) rewindDryRunLocked() {
	cs, err := a.claudeSession()
	rw := a.rewind
	if rw == nil || !a.backend.Has(featFileRewind) {
		return
	}
	id := rw.targets[rw.sel]
	if rw.filesAt == id {
		return
	}
	if err != nil {
		rw.filesAt, rw.files = id, &claude.RewindFilesResult{Error: err.Error()}
		return
	}
	rw.filesAt, rw.files = id, nil
	go func() {
		ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
		defer cancel()
		res, err := cs.RewindFiles(ctx, id, true)
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.rewind != rw || rw.filesAt != id {
			return
		}
		if err != nil {
			res = &claude.RewindFilesResult{Error: err.Error()}
		}
		rw.files = res
		if res.CanRewind && len(res.FilesChanged) > 0 && rw.restore == "conversation" {
			rw.restore = "both"
		}
		a.changed()
	}()
}

func (a *app) rewindKey(k tern.Key) {
	rw := a.rewind
	switch {
	case k.Is("escape"), k.Is("ctrl+c"):
		a.rewind = nil
	case k.Is("up"), k.Is("k"), k.Is("left"):
		rw.sel = max(rw.sel-1, 0)
		a.rewindDryRunLocked()
		a.revealRewind = true
	case k.Is("down"), k.Is("j"), k.Is("right"):
		rw.sel = min(rw.sel+1, len(rw.targets)-1)
		a.rewindDryRunLocked()
		a.revealRewind = true
	case k.Is("tab"):
		modes := a.restoreModesLocked()
		i := slices.IndexFunc(modes, func(m struct{ id, label string }) bool { return m.id == rw.restore })
		rw.restore = modes[(i+1)%len(modes)].id
	case k.Is("enter"):
		a.doRewindLocked()
	}
}

// doRewindLocked restores what the bar says: files through Claude Code's
// checkpoints, then the conversation (the message returns to the composer).
func (a *app) doRewindLocked() {
	rw := a.rewind
	if rw == nil {
		return
	}
	files := rw.restore != "conversation" && rw.files != nil && rw.files.CanRewind && len(rw.files.FilesChanged) > 0
	conversation := rw.restore != "code"
	// Keep the bar open rather than silently restore less than it says.
	switch {
	case rw.restore != "conversation" && rw.files == nil:
		a.toastLocked("info", "Still checking which files would be restored")
		return
	case rw.restore == "code" && !files:
		a.toastLocked("info", "No file changes to restore from this message")
		return
	}
	a.rewind = nil
	e := a.entries[rw.targets[rw.sel]]
	if e == nil {
		return
	}
	it := e.item
	if files {
		cs, err := a.claudeSession()
		if err != nil {
			a.toastLocked("error", err.Error())
			return
		}
		a.async("Restore files", func(ctx context.Context) error {
			res, err := cs.RewindFiles(ctx, it.ID, false)
			if err == nil && res.Error != "" {
				err = fmt.Errorf("%s", res.Error)
			}
			if err != nil {
				return err
			}
			a.mu.Lock()
			defer a.mu.Unlock()
			a.toastLocked("success", fmt.Sprintf("Restored %s (+%d −%d)", plural(len(res.FilesChanged), "file"), res.Insertions, res.Deletions))
			if conversation {
				a.rewindLocked(it)
			}
			return nil
		})
		return
	}
	if conversation {
		a.rewindLocked(it)
	}
}

// rewindBar replaces the composer while choosing.
func (a *app) rewindBar() tern.Element {
	rw := a.rewind
	var modes []tern.Element
	for _, m := range a.restoreModesLocked() {
		mode := m
		class := ""
		if rw.restore == mode.id {
			class = "primary"
		}
		modes = append(modes, button("mode-"+mode.id, mode.label, class, func() { a.locked(func() { rw.restore = mode.id }) }))
	}
	var files []ui.Span
	switch f := rw.files; {
	case !a.backend.Has(featFileRewind):
		files = []ui.Span{{T: a.backend.Name() + " rewinds the conversation; files stay as they are", S: "dim"}}
	case f == nil:
		files = []ui.Span{{T: "Checking files…", S: "dim"}}
	case f.Error != "" || !f.CanRewind:
		msg := "Files can't be restored from here"
		if f.Error != "" {
			msg += ": " + f.Error
		}
		files = []ui.Span{{T: msg, S: "dim"}}
	case len(f.FilesChanged) == 0:
		files = []ui.Span{{T: "No file changes since this message", S: "dim"}}
	default:
		files = []ui.Span{{T: plural(len(f.FilesChanged), "file") + " would be restored ", S: "muted"},
			{T: fmt.Sprintf("+%d", f.Insertions), S: "ins"}, {T: " "}, {T: fmt.Sprintf("−%d", f.Deletions), S: "del"}}
	}
	return ui.Col{Common: ui.Common{Key: "composer", Role: "omp.editor"}, Gap: ui.GapSM, Children: ui.Nodes(
		ui.Row{Common: ui.Common{Key: "bar", Role: "omp.rewind.bar"}, Gap: ui.GapMD, Align: ui.AlignCenter, Wrap: true, Children: ui.Nodes(
			ui.Text{Common: ui.Common{Key: "title"}, Spans: []ui.Span{{T: "↺ ", S: "accent"}, {T: "Rewind", S: "strong"}}},
			ui.Text{Common: ui.Common{Key: "pos"}, Spans: []ui.Span{{T: fmt.Sprintf("%d/%d", rw.sel+1, len(rw.targets)), S: "dim"}}},
			a.rewindHints(),
			actionsRow("actions", []tern.Element{nil,
				shortcutButton("cancel", "Cancel", "esc", "", func() { a.locked(func() { a.rewind = nil }) }),
				shortcutButton("go", "Rewind here", "enter", "primary", func() { a.locked(a.doRewindLocked) }),
			}),
		)},
		ui.Row{Common: ui.Common{Key: "restore"}, Gap: ui.GapSM, Align: ui.AlignCenter, Wrap: true, Children: append(modes,
			ui.Text{Common: ui.Common{Key: "files"}, Spans: files})},
	)}
}

// rewindHints names the bar's keys; Tab only when there is a choice.
func (a *app) rewindHints() tern.Element {
	if len(a.restoreModesLocked()) < 2 {
		return hintsRow("hints", hint{"up down", "message"})
	}
	return hintsRow("hints", hint{"up down", "message"}, hint{"tab", "what to restore"})
}

// rewindHere is the line above the chosen message.
func (a *app) rewindHere() tern.Element {
	rw := a.rewind
	dropped := len(rw.targets) - rw.sel - 1
	spans := []ui.Span{{T: "↺ ", S: "accent"}, {T: "Rewind to here", S: "accent strong"},
		{T: " · the message returns to the composer", S: "dim"}}
	if dropped > 0 {
		spans = append(spans, ui.Span{T: fmt.Sprintf(" · %s below dropped", plural(dropped, "turn")), S: "dim"})
	} else {
		spans = append(spans, ui.Span{T: " · only this turn is dropped", S: "dim"})
	}
	return ui.Text{Common: ui.Common{Key: "rewind-here", Role: "omp.rewind.here"}, Spans: spans, Wrap: "none"}
}
