package chat

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/stencil-hq/tern-sdk/go/tern"
	"github.com/stencil-hq/tern-sdk/go/tern/ui"
)

// maxImageBytes is the largest image drawn as a thumbnail; Claude takes
// larger ones, but Tern caps blobs.
const maxImageBytes = 8 << 20

// attachment is a file waiting to go with the next message.
type attachment struct {
	Path  string
	Image bool
}

func newAttachment(path string) attachment {
	return attachment{Path: path, Image: slices.Contains(imageExts, strings.ToLower(filepath.Ext(path)))}
}

// pastedFiles reads a paste as file paths: what Tern pastes for dropped
// files (shell-escaped or quoted paths, file:// URLs) and for an image it
// saved from the clipboard. It returns nil unless every word is an
// existing file, so ordinary text is never taken for a path.
func pastedFiles(text, cwd string) []string {
	words := shellWords(strings.TrimSpace(text))
	if len(words) == 0 {
		return nil
	}
	var out []string
	for _, w := range words {
		if strings.HasPrefix(w, "file://") {
			u, err := url.Parse(w)
			if err != nil {
				return nil
			}
			w = u.Path
		}
		if strings.HasPrefix(w, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				w = filepath.Join(home, w[2:])
			}
		}
		if !filepath.IsAbs(w) {
			w = filepath.Join(cwd, w)
		}
		st, err := os.Stat(w)
		if err != nil || !st.Mode().IsRegular() {
			return nil
		}
		out = append(out, w)
	}
	return out
}

// shellWords splits s like a shell does for simple words: whitespace
// separates, backslash escapes, and single or double quotes group.
func shellWords(s string) []string {
	var out []string
	var cur strings.Builder
	in, quote, escaped := false, rune(0), false
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped, in = false, true
		case r == '\\' && quote != '\'':
			escaped = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, in = r, true
		case unicode.IsSpace(r):
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if in {
		out = append(out, cur.String())
	}
	return out
}

func (a *app) attachLocked(paths ...string) {
	for _, p := range paths {
		if !slices.ContainsFunc(a.attachments, func(x attachment) bool { return x.Path == p }) {
			a.attachments = append(a.attachments, newAttachment(p))
		}
	}
	a.changed()
}

// pasteClipboard pastes the system clipboard into the composer, as omp's
// Ctrl+V does: an image is saved to a temporary file and attached, anything
// else is pasted as text. Tern hands Ctrl/Cmd+V to the program instead of
// pasting itself.
func (a *app) pasteClipboard() {
	a.clipboardPaste(true, a.composerShownLocked, a.pasteTextLocked)
}

// pasteClipboardText pastes clipboard text with onText, if still is true
// when the clipboard has been read; images are ignored.
func (a *app) pasteClipboardText(still func() bool, onText func(string)) {
	a.clipboardPaste(false, still, onText)
}

// clipboardPaste reads the clipboard off the input goroutine. The read can
// take a moment, so the result is dropped when the target is gone by then.
func (a *app) clipboardPaste(images bool, still func() bool, onText func(string)) {
	go func() {
		ctx, cancel := context.WithTimeout(a.ctx, 5*time.Second)
		defer cancel()
		var data []byte
		imgErr := errors.New("the clipboard holds no text")
		if images {
			data, imgErr = readClipboardImage(ctx)
		}
		var text string
		var textErr error
		if imgErr != nil {
			text, textErr = readClipboardText(ctx)
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		defer a.changed()
		if !still() {
			return
		}
		if imgErr != nil {
			if textErr == nil && text != "" {
				onText(text)
			} else {
				a.toastLocked("info", imgErr.Error())
			}
			return
		}
		dir := filepath.Join(os.TempDir(), "tern-agents-ui-paste")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			a.toastLocked("error", "Could not save the pasted image: "+err.Error())
			return
		}
		path := filepath.Join(dir, "screenshot-"+time.Now().Format("20060102-150405.000")+".png")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			a.toastLocked("error", "Could not save the pasted image: "+err.Error())
			return
		}
		a.attachLocked(path)
	}()
}

// composerShownLocked reports whether the editor is on screen: a question,
// the rewind bar, a menu or the settings sheet takes its place or its keys.
func (a *app) composerShownLocked() bool {
	return a.rewind == nil && a.menu == nil && a.prefs == nil && a.askRequestLocked() == nil && a.planRequestLocked() == nil
}

// pasteTextLocked inserts pasted text, attaching it instead when it is
// only paths of existing files.
func (a *app) pasteTextLocked(text string) {
	if files := pastedFiles(text, a.cwd); files != nil {
		a.attachLocked(files...)
		return
	}
	a.composer.insert(text)
	a.refreshCompletionLocked(false)
}

// The clipboard readers are variables so tests can stand in for the
// platform tools.
var (
	readClipboardImage = clipboardImage
	readClipboardText  = clipboardText
)

// clipboardText reads text from the clipboard with the platform's tool.
func clipboardText(ctx context.Context) (string, error) {
	var tools [][]string
	switch runtime.GOOS {
	case "darwin":
		tools = [][]string{{"pbpaste"}}
	default:
		if os.Getenv("WAYLAND_DISPLAY") != "" {
			tools = append(tools, []string{"wl-paste", "--no-newline", "--type", "text/plain"})
		}
		tools = append(tools, []string{"xclip", "-selection", "clipboard", "-o"}, []string{"xsel", "--clipboard", "--output"})
	}
	for _, t := range tools {
		if _, err := exec.LookPath(t[0]); err != nil {
			continue
		}
		if out, err := exec.CommandContext(ctx, t[0], t[1:]...).Output(); err == nil {
			return string(out), nil
		}
	}
	return "", fmt.Errorf("could not read the clipboard")
}

// clipboardImage reads a PNG from the clipboard with the platform's tool.
func clipboardImage(ctx context.Context) ([]byte, error) {
	run := func(name string, args ...string) ([]byte, error) {
		if _, err := exec.LookPath(name); err != nil {
			return nil, err
		}
		return exec.CommandContext(ctx, name, args...).Output()
	}
	var tried []string
	switch runtime.GOOS {
	case "darwin":
		tried = append(tried, "pngpaste")
		if out, err := run("pngpaste", "-"); err == nil && len(out) > 0 {
			return out, nil
		}
		tried = append(tried, "osascript")
		f, err := os.CreateTemp("", "tern-agents-ui-*.png")
		if err == nil {
			name := f.Name()
			f.Close()
			defer os.Remove(name)
			script := fmt.Sprintf(`set f to open for access POSIX file %q with write permission
write (the clipboard as «class PNGf») to f
close access f`, name)
			if _, err := run("osascript", "-e", script); err == nil {
				if out, err := os.ReadFile(name); err == nil && len(out) > 0 {
					return out, nil
				}
			}
		}
	default:
		if os.Getenv("WAYLAND_DISPLAY") != "" {
			tried = append(tried, "wl-paste")
			if types, err := run("wl-paste", "--list-types"); err == nil {
				if !bytes.Contains(types, []byte("image/png")) {
					return nil, fmt.Errorf("the clipboard holds no image")
				}
				if out, err := run("wl-paste", "--type", "image/png"); err == nil && len(out) > 0 {
					return out, nil
				}
			}
		}
		tried = append(tried, "xclip")
		if types, err := run("xclip", "-selection", "clipboard", "-t", "TARGETS", "-o"); err == nil {
			if !bytes.Contains(types, []byte("image/png")) {
				return nil, fmt.Errorf("the clipboard holds no image")
			}
			if out, err := run("xclip", "-selection", "clipboard", "-t", "image/png", "-o"); err == nil && len(out) > 0 {
				return out, nil
			}
		}
	}
	return nil, fmt.Errorf("no clipboard image (tried %s)", strings.Join(tried, ", "))
}

// attachmentParts turns attachments into message parts: images inline,
// other files as references Claude Code reads (PDFs inline as documents).
func attachmentParts(atts []attachment) []agent.Part {
	var parts []agent.Part
	for _, at := range atts {
		if at.Image {
			parts = append(parts, agent.ImageFile(at.Path))
		} else {
			parts = append(parts, agent.FileRef(at.Path))
		}
	}
	return parts
}

// newClientID is a random UUID; Claude Code echoes it on the message item,
// which ties a sent message to its attachments.
func newClientID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// imageBlob sends a file as a blob once and returns its id.
func (a *app) imageBlob(path string) string {
	if id, ok := a.blobs[path]; ok {
		return id
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) > maxImageBytes || a.session == nil {
		a.blobs[path] = ""
		return ""
	}
	mime := "image/" + strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	if mime == "image/jpg" {
		mime = "image/jpeg"
	}
	id, err := a.session.Blob(data, mime)
	if err != nil {
		return ""
	}
	a.blobs[path] = id
	return id
}

// attachmentChips draws the composer's attachments as omp's chips: a
// numbered card per file with its thumbnail or name. Right-click or the ×
// removes one; Backspace on an empty composer removes the last.
func (a *app) attachmentChips(key string, atts []attachment) tern.Element {
	var kids []tern.Element
	for i, at := range atts {
		path := at.Path
		remove := func() {
			a.locked(func() {
				a.attachments = slices.DeleteFunc(a.attachments, func(x attachment) bool { return x.Path == path })
			})
		}
		body := []tern.Element{a.attachmentPreview(at, "12ch")}
		body = append(body, ui.Text{Common: ui.Common{Key: "x", Title: "Remove", OnClick: func(*tern.Action) { remove() }},
			Spans: []ui.Span{{T: "× remove", S: "muted"}}})
		kids = append(kids, ui.Card{Common: ui.Common{Key: fmt.Sprint("a", i), Role: "omp.composer.chip", Tone: ui.ToneAccent, Title: path,
			OnMenu: map[string]func(*tern.Action){"Remove": func(*tern.Action) { remove() }}},
			Head: ui.T(fmt.Sprintf("#%d", i+1)), Children: body})
	}
	return ui.Row{Common: ui.Common{Key: key, Role: "omp.composer.chips"}, Wrap: true, Gap: ui.GapSM, Children: kids}
}

// sentAttachmentsNode shows a sent message's attachments, as omp's
// user-message images.
func (a *app) sentAttachmentsNode(atts []attachment) tern.Element {
	var kids []tern.Element
	for i, at := range atts {
		kids = append(kids, ui.Col{Common: ui.Common{Key: fmt.Sprint("a", i), Title: at.Path}, Children: ui.Nodes(a.attachmentPreview(at, "24ch"))})
	}
	return ui.Row{Common: ui.Common{Key: "images", Role: "omp.user.images"}, Wrap: true, Gap: ui.GapSM, Align: ui.AlignStart, Children: kids}
}

// attachmentPreview is an image thumbnail, or a file icon and name.
func (a *app) attachmentPreview(at attachment, width string) tern.Element {
	if at.Image {
		if id := a.imageBlob(at.Path); id != "" {
			w, _ := strconv.ParseFloat(strings.TrimSuffix(width, "ch"), 64)
			return ui.Image{Common: ui.Common{Key: "img", Title: at.Path, Actions: &ui.Actions{Click: "zoom"},
				Max: &ui.Bounds{W: ui.Ch(w), H: ui.Lines(4)}}, Blob: id, Alt: filepath.Base(at.Path)}
		}
	}
	return ui.Row{Common: ui.Common{Key: "file"}, Gap: ui.GapXS, Align: ui.AlignCenter, Children: ui.Nodes(
		ui.Icon{Name: "file"}, ui.Text{Text: filepath.Base(at.Path)})}
}
