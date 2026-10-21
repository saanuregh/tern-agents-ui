package chat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/claude"
	"github.com/saanuregh/agent-cli-connector/codex"
	"github.com/stencil-hq/tern-sdk/go/tern"
	"github.com/stencil-hq/tern-sdk/go/tern/ui"
)

func TestComposerUTF16Edits(t *testing.T) {
	var c composer
	c.insert("a😀b")
	if got := c.cursor16(); got != 4 {
		t.Fatalf("cursor16 = %d, want 4 (the emoji is two units)", got)
	}
	// Replace the emoji (units 1..3) with "x", caret after it.
	c.edit(1, 3, "x", 2)
	if c.String() != "axb" || c.cursor != 2 {
		t.Fatalf("after edit %q cursor %d", c.String(), c.cursor)
	}
	if !c.doUndo() || c.String() != "a😀b" {
		t.Fatalf("undo gave %q", c.String())
	}
	if !c.doRedo() || c.String() != "axb" {
		t.Fatalf("redo gave %q", c.String())
	}
}

func TestComposerWordsAndLines(t *testing.T) {
	var c composer
	c.insert("one two\nthree")
	if c.cursor = c.prevWord(); c.cursor != 8 {
		t.Fatalf("prevWord = %d", c.cursor)
	}
	if !c.moveLine(-1) || c.cursor != 0 {
		t.Fatalf("moveLine up = %d", c.cursor)
	}
	if c.moveLine(-1) {
		t.Fatal("moved above the first line")
	}
	c.cursor = len(c.text)
	tok, start := c.token()
	if tok != "three" || start != 8 {
		t.Fatalf("token %q at %d", tok, start)
	}
	c.replaceToken("@three.go ")
	if c.String() != "one two\n@three.go " {
		t.Fatalf("replaceToken gave %q", c.String())
	}
}

func TestComposerHistory(t *testing.T) {
	var c composer
	c.remember("first")
	c.remember("second")
	c.set("draft")
	c.browse(-1)
	if c.String() != "second" {
		t.Fatalf("browse up = %q", c.String())
	}
	c.browse(-1)
	c.browse(1)
	c.browse(1)
	if c.String() != "draft" {
		t.Fatalf("browse back down = %q, want the stashed draft", c.String())
	}
}

func TestMessagePartsAttachImages(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "shot.png"), []byte("\x89PNG"), 0o600); err != nil {
		t.Fatal(err)
	}
	parts := messageParts("look at @shot.png and @main.go, and @missing.png", dir)
	if len(parts) != 2 || parts[1].Type != agent.PartImage || parts[1].Path != filepath.Join(dir, "shot.png") {
		t.Fatalf("parts %+v", parts)
	}
}

func TestFormFields(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","required":["name"],"properties":{
		"name":{"type":"string","title":"Name"},
		"size":{"type":"string","enum":["s","m"]},
		"count":{"type":"integer","default":2}}}`)
	f := formFields(schema)
	if len(f) != 3 || f[0].Name != "name" || !f[0].Required {
		t.Fatalf("fields %+v", f)
	}
	for _, field := range f {
		if field.Name == "count" && field.parse("7") != int64(7) {
			t.Errorf("count parsed as %#v", field.parse("7"))
		}
		if field.Name == "size" && len(field.Enum) != 2 {
			t.Errorf("size enum %v", field.Enum)
		}
	}
}

func testApp(t *testing.T) *app {
	t.Helper()
	a := newApp(context.Background(), Claude(), agent.Config{Cwd: "/w", Model: "claude-haiku-5-5", PermissionMode: agent.PermissionDefault},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(a.quit)
	return a
}

func TestCommandSuggestions(t *testing.T) {
	a := testApp(t)
	a.info.Commands = []agent.SlashCommand{{Name: "review", Description: "Review a PR"}, {Name: "model", Description: "dup"}}
	got := a.commandSuggestionsLocked("re")
	var labels []string
	for _, s := range got {
		labels = append(labels, s.Label)
	}
	want := "/resume [session id],/rewind,/reload,/review"
	if strings.Join(labels, ",") != want {
		t.Errorf("suggestions %v, want %s", labels, want)
	}
	for _, s := range a.commandSuggestionsLocked("model") {
		if s.Detail == "dup" {
			t.Error("Claude's /model shadowed the local one")
		}
	}
}

// TestViewHasUniqueKeys renders every kind of entry and request through
// the SDK's reconciler, which rejects a view with duplicate sibling keys.
func TestViewHasUniqueKeys(t *testing.T) {
	a := testApp(t)
	exit := 1
	events := []agent.Event{
		&agent.ItemCompleted{Item: agent.Item{ID: "u1", Kind: agent.ItemUserMessage, Status: agent.ItemDone, Text: "hi"}},
		&agent.ReasoningDelta{ItemID: "r1", Delta: "pondering"},
		&agent.TextDelta{ItemID: "msg_1/0", Delta: "Hello **there**"},
		&agent.ItemCompleted{Item: agent.Item{ID: "t1", Kind: agent.ItemToolCall, Status: agent.ItemFailed, Tool: &agent.ToolCall{
			Name: "Bash", Kind: agent.ToolShell, Command: "make", Output: "boom", ExitCode: &exit, IsError: true, Duration: time.Second}}},
		&agent.ItemCompleted{Item: agent.Item{ID: "t2", Kind: agent.ItemToolCall, Status: agent.ItemDone, Tool: &agent.ToolCall{
			Name: "Edit", Kind: agent.ToolEdit, Path: "/w/a.go", Input: json.RawMessage(`{"old_string":"a","new_string":"b"}`)}}},
		&agent.ItemStarted{Item: agent.Item{ID: "t3", Kind: agent.ItemToolCall, Status: agent.ItemRunning, Tool: &agent.ToolCall{
			Name: "Agent", Kind: agent.ToolSubagent, Input: json.RawMessage(`{"description":"look around"}`)}}},
		&agent.TaskUpdated{Task: agent.Task{ID: "task1", Kind: agent.TaskSubagent, Status: agent.TaskRunning, ToolCallID: "t3",
			Subagent: &agent.Subagent{Type: "Explore"}}},
		&agent.ItemCompleted{Item: agent.Item{ID: "t4", Kind: agent.ItemToolCall, Status: agent.ItemDone, SubagentID: "task1", ParentID: "t3",
			Tool: &agent.ToolCall{Name: "Read", Kind: agent.ToolRead, Path: "/w/b.go", Output: "x\ny\n"}}},
		&agent.TaskUpdated{Task: agent.Task{ID: "task2", Kind: agent.TaskShell, Status: agent.TaskRunning, Description: "npm run dev"}},
		&agent.ItemCompleted{Item: agent.Item{ID: "c1", Kind: agent.ItemCompaction, Status: agent.ItemDone}},
		&agent.PlanUpdated{Steps: []agent.PlanStep{{Text: "plan", Status: agent.StepCompleted}, {Text: "build", Status: agent.StepInProgress}}},
		&agent.RateLimitUpdated{Limits: []agent.RateLimit{{Name: "5h", UsedPercent: 91, Status: agent.RateLimitWarning}}},
		&agent.UsageUpdated{Usage: agent.Usage{ContextTokens: 50000, ContextWindow: 200000}},
		&agent.PermissionRequest{ID: "p1", Kind: agent.PermissionCommand, ToolName: "Bash", Command: "rm -rf build",
			Decisions: []agent.PermissionDecision{agent.DecisionAllow, agent.DecisionAllowSession, agent.DecisionDeny}},
		&agent.UserInputRequest{ID: "q1", Questions: []agent.Question{{ID: "color", Text: "Which?", Options: []agent.QuestionOption{{Label: "red"}, {Label: "blue"}}}}},
		&agent.ElicitationRequest{ID: "e1", Server: "jira", Schema: json.RawMessage(`{"properties":{"key":{"type":"string"},"ok":{"type":"boolean"}}}`)},
	}
	a.status = "running"
	for _, ev := range events {
		a.apply(ev)
	}
	a.queued["u1"] = true
	a.upsertLocked(a.entries["u1"].item)
	a.addLocal(localCard{Title: "Status", Text: "| a | b |", Choices: []choice{{Label: "One"}, {Label: "Two"}}})
	a.composer.insert("/he")
	a.refreshCompletionLocked(false)
	a.openMenuLocked("Pick", []choice{{Label: "x", Current: true, Preview: "# hi"}, {Label: "y", Facts: map[string]any{"n": 3}}})
	a.toastLocked("error", "oops")
	a.apply(&agent.PermissionRequest{ID: "plan1", Kind: agent.PermissionPlanApproval, Plan: "# Plan\n- a"})
	a.apply(&agent.ItemCompleted{Item: agent.Item{ID: "rd1", Kind: agent.ItemToolCall, Status: agent.ItemDone, Tool: &agent.ToolCall{Name: "Read", Kind: agent.ToolRead, Path: "/w/a.go"}}})
	a.apply(&agent.ItemCompleted{Item: agent.Item{ID: "rd2", Kind: agent.ItemToolCall, Status: agent.ItemDone, Tool: &agent.ToolCall{Name: "Read", Kind: agent.ToolRead, Path: "/w/b.go"}}})
	a.apply(&agent.ItemCompleted{Item: agent.Item{ID: "td1", Kind: agent.ItemToolCall, Status: agent.ItemDone, Tool: &agent.ToolCall{Name: "TodoWrite", Kind: agent.ToolTodo}}})
	a.apply(&agent.PlanUpdated{ItemID: "td1", Steps: []agent.PlanStep{{Text: "x", Status: agent.StepCompleted}}})

	var r tern.Reconciler
	for _, expand := range []bool{false, true} {
		a.expandAll = expand
		if _, err := r.Diff(a.view()); err != nil {
			t.Fatalf("expandAll=%v: %v", expand, err)
		}
	}
	a.apply(&agent.ItemCompleted{Item: agent.Item{ID: "u2", Kind: agent.ItemUserMessage, Status: agent.ItemDone, Text: "again"}})
	a.startRewindLocked("")
	if a.running() && a.rewind != nil {
		t.Fatal("rewind opened during a turn")
	}
	a.status = "idle"
	a.startRewindLocked("")
	if a.rewind == nil {
		t.Fatal("rewind did not open")
	}
	if _, err := r.Diff(a.view()); err != nil {
		t.Fatalf("rewind: %v", err)
	}
	a.rewind = nil
	for _, page := range []string{"session", "interface", "keys", "about"} {
		a.prefs = &prefsState{page: page}
		if _, err := r.Diff(a.view()); err != nil {
			t.Fatalf("prefs %s: %v", page, err)
		}
	}
	a.prefs = nil
	// Claude Code's context reply, as its SDK types it: rows have no id.
	var u contextUsage
	if err := json.Unmarshal([]byte(`{"totalTokens":64000,"maxTokens":200000,"categories":[
		{"name":"System prompt","tokens":3000,"kind":"used"},{"name":"Messages","tokens":61000,"kind":"used"},
		{"name":"MCP tools (deferred)","tokens":900,"kind":"deferred"},{"name":"Autocompact buffer","tokens":33000,"kind":"buffer"},
		{"name":"Free space","tokens":103000,"kind":"free"}]}`), &u); err != nil {
		t.Fatal(err)
	}
	a.ctxUsage, a.showContext = &u, true
	if _, err := r.Diff(a.view()); err != nil {
		t.Fatalf("context: %v", err)
	}
	a.showContext = false
	// Every request kind draws as the first request in turn.
	for len(a.requests) > 0 {
		if _, err := r.Diff(a.view()); err != nil {
			t.Fatalf("request %s: %v", a.requests[0].RequestID(), err)
		}
		a.dropRequestLocked(a.requests[0].RequestID())
	}
	if len(a.order) != 12 || a.entries["t4"] == nil || slices.Contains(a.order, "t4") {
		t.Errorf("order %v: want the subagent's read kept out of the main list", a.order)
	}
}

func TestQuestionAnswerFlow(t *testing.T) {
	a := testApp(t)
	r := &agent.UserInputRequest{ID: "q1", Questions: []agent.Question{
		{ID: "a", Text: "Pick", Options: []agent.QuestionOption{{Label: "x"}, {Label: "y"}}},
		{ID: "b", Text: "Why?"},
		{ID: "c", Text: "Which?", MultiSelect: true, Options: []agent.QuestionOption{{Label: "p"}, {Label: "q"}}},
	}}
	a.apply(r)
	key := func(name, text string) { a.askKey(r, tern.Key{Name: name, Text: text}) }
	key("2", "2") // picks y, moves to the free-text question
	d := a.draftFor("q1")
	if got := d.picks["a"]; len(got) != 1 || got[0] != "y" || d.q != 1 {
		t.Fatalf("picks %v q %d", got, d.q)
	}
	key("b", "b") // typing starts an Other answer
	key("e", "e")
	key("enter", "")
	if got := d.picks["b"]; len(got) != 1 || got[0] != "be" || d.q != 2 {
		t.Fatalf("free text %v q %d", got, d.q)
	}
	key("space", " ") // toggles p
	key("down", "")
	key("space", " ") // toggles q
	key("enter", "")  // moves on to the review tab
	if got := d.picks["c"]; len(got) != 2 || d.q != 3 {
		t.Fatalf("multi %v q %d", got, d.q)
	}
	key("enter", "") // submits
	if len(a.requests) != 0 {
		t.Error("request still pending after submit")
	}
}

func TestExactCommandWinsCompletion(t *testing.T) {
	a := testApp(t)
	got := a.commandSuggestionsLocked("mode")
	if len(got) == 0 || got[0].Label != "/mode [mode]" {
		t.Fatalf("first suggestion %+v, want /mode", got)
	}
	if !a.exactCommandLocked("/mode") || a.exactCommandLocked("/mod") {
		t.Error("exactCommandLocked")
	}
}

func TestLocalFiles(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"notes.md", "src/notes_test.go", ".git/notes", "node_modules/x/notes.js"} {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o700)
		_ = os.WriteFile(filepath.Join(dir, p), nil, 0o600)
	}
	got := localFiles(dir, "NOTES", 10)
	if strings.Join(got, ",") != "notes.md,src/notes_test.go" {
		t.Errorf("localFiles = %v", got)
	}
}

func TestPastedFiles(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "shot one.png"), filepath.Join(dir, "it's.pdf")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	esc := strings.ReplaceAll(a, " ", `\ `)
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{esc, []string{a}},
		{"'" + a + "' " + strings.ReplaceAll(b, "'", `\'`), []string{a, b}},
		{"file://" + strings.ReplaceAll(a, " ", "%20"), []string{a}},
		{"shot one.png", nil},          // a relative name with a space is two words
		{"please look at " + esc, nil}, // text is never taken for paths
		{filepath.Join(dir, "missing.png"), nil},
	} {
		if got := pastedFiles(tc.in, dir); strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("pastedFiles(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestAttachmentsSendWithMessage(t *testing.T) {
	a := testApp(t)
	img := filepath.Join(t.TempDir(), "s.png")
	_ = os.WriteFile(img, []byte("\x89PNG"), 0o600)
	a.attachLocked(img, img)
	if len(a.attachments) != 1 || !a.attachments[0].Image {
		t.Fatalf("attachments %+v", a.attachments)
	}
	parts := attachmentParts(a.attachments)
	if len(parts) != 1 || parts[0].Type != agent.PartImage {
		t.Fatalf("parts %+v", parts)
	}
	if id := newClientID(); len(id) != 36 || id[14] != '4' {
		t.Errorf("client id %q is not a v4 UUID", id)
	}
}

func TestLinkPaths(t *testing.T) {
	a := testApp(t)
	dir := t.TempDir()
	a.cwd = dir
	_ = os.MkdirAll(filepath.Join(dir, "src", "ui"), 0o700)
	_ = os.WriteFile(filepath.Join(dir, "README.md"), nil, 0o600)
	in := "Work in `" + dir + "` and `src/ui`, see `README.md` and `src/main.go`.\n\n" +
		"```sh\necho `README.md`\n```\nAlso " + dir + "/src here, and " + dir + "/README.md:3 too."
	out, marks := a.linkPaths(in)
	want := "Work in " + dir + " and src/ui, see README.md and `src/main.go`.\n\n" +
		"```sh\necho `README.md`\n```\nAlso " + dir + "/src here, and " + dir + "/README.md:3 too."
	if out != want {
		t.Errorf("text\n got %q\nwant %q", out, want)
	}
	var got []string
	for _, m := range marks {
		got = append(got, m.T)
	}
	if strings.Join(got, ",") != dir+",src/ui,README.md,"+dir+"/src,"+dir+"/README.md:3" {
		t.Errorf("marks %v", got)
	}
	if marks[0].S != "path" || marks[0].Href != "file://"+dir {
		t.Errorf("mark %+v", marks[0])
	}
}

// Tern hands Ctrl/Cmd+V to the program, so the composer reads the clipboard
// itself: text is pasted, an image attached.
func TestClipboardPaste(t *testing.T) {
	img, text := readClipboardImage, readClipboardText
	t.Cleanup(func() { readClipboardImage, readClipboardText = img, text })
	noImage := func(context.Context) ([]byte, error) { return nil, errors.New("the clipboard holds no image") }
	wait := func(a *app, done func() bool) {
		t.Helper()
		for range 200 {
			a.mu.Lock()
			ok := done()
			a.mu.Unlock()
			if ok {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("paste never landed")
	}

	for _, chord := range []tern.Key{{Name: "v", Ctrl: true}, {Name: "v", Meta: true}, {Name: "v", Ctrl: true, Shift: true}} {
		a := testApp(t)
		readClipboardImage = noImage
		readClipboardText = func(context.Context) (string, error) { return "hello world", nil }
		_ = a.key(tern.Key{Name: "h", Text: "h"})
		_ = a.key(chord)
		wait(a, func() bool { return a.composer.String() == "hhello world" })
	}

	a := testApp(t)
	readClipboardImage = func(context.Context) ([]byte, error) { return []byte("\x89PNG"), nil }
	_ = a.key(tern.Key{Name: "v", Ctrl: true})
	wait(a, func() bool { return len(a.attachments) == 1 && a.attachments[0].Image })
	t.Cleanup(func() { _ = os.Remove(a.attachments[0].Path) })

	b := testApp(t)
	_ = b.key(tern.Key{Name: "paste", Text: "pasted\ntext"})
	if got := b.composer.String(); got != "pasted\ntext" {
		t.Fatalf("bracketed paste gave %q", got)
	}
}

// Command suggestions carry no icon: the label's own "/" says what they are.
func TestCommandCompletionHasNoIcon(t *testing.T) {
	a := testApp(t)
	a.composer.set("/")
	a.refreshCompletionLocked(false)
	if !a.complete.open() {
		t.Fatal("no command suggestions")
	}
	ov, ok := a.completionNode().(ui.Overlay)
	if !ok {
		t.Fatalf("completion is %T", a.completionNode())
	}
	for _, c := range ov.Children[0].(ui.List).Children {
		if it := c.(ui.Item); it.Icon != "" {
			t.Fatalf("command row %v has icon %q", it.Label, it.Icon)
		}
	}
}

// namedBackend is Claude's backend under another name, to find text that
// names Claude instead of asking the backend.
type namedBackend struct{ claudeBackend }

func (namedBackend) Name() string    { return "Zed" }
func (namedBackend) Product() string { return "Zed CLI" }

// Nothing drawn names Claude when the backend is another agent.
func TestBackendWording(t *testing.T) {
	a := newApp(context.Background(), namedBackend{Claude().(claudeBackend)},
		agent.Config{Cwd: "/w", PermissionMode: agent.PermissionDefault}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(a.quit)
	check := func(what string) {
		t.Helper()
		var r tern.Reconciler
		ops, err := r.Diff(a.view())
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		raw, _ := json.Marshal(ops)
		if i := strings.Index(string(raw), "Claude"); i >= 0 {
			t.Fatalf("%s names Claude: …%s…", what, raw[max(i-80, 0):min(i+40, len(raw))])
		}
	}
	check("welcome")
	a.apply(&agent.ItemCompleted{Item: agent.Item{ID: "u1", Kind: agent.ItemUserMessage, Status: agent.ItemDone, Text: "hi"}})
	a.requests = append(a.requests, &agent.PermissionRequest{ID: "p1", Kind: agent.PermissionCommand, ToolName: "Bash", Command: "ls"})
	a.composer.set("because")
	check("permission")
	a.requests = nil
	a.composer.reset()
	for _, page := range []string{"session", "interface", "keys", "about"} {
		a.prefs = &prefsState{page: page}
		check("prefs " + page)
	}
}

func codexTestApp(t *testing.T) *app {
	t.Helper()
	a := newApp(context.Background(), Codex(), agent.Config{Cwd: "/w", PermissionMode: agent.PermissionDefault},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(a.quit)
	return a
}

// Codex lacks Claude's side questions, plan file, subagent list, rules, usage, reload, cd, thinking budget
// and prompt suggestions: none is offered.
func TestCodexFeaturesHidden(t *testing.T) {
	a := codexTestApp(t)
	for _, c := range a.commandsLocked() {
		if slices.Contains([]string{"btw", "plan", "agents", "permissions", "usage", "reload", "cd"}, c.Name) {
			t.Errorf("/%s is offered", c.Name)
		}
	}
	if a.runCommandLocked("btw", "why") {
		t.Error("/btw ran as a local command")
	}
	for _, page := range []string{"session", "interface"} {
		_, sections := a.prefsPageLocked(page)
		for _, sec := range sections {
			for _, r := range sec.Rows {
				if r.ID == "thinking" || r.ID == "suggest" {
					t.Errorf("%s offers %s", page, r.ID)
				}
			}
		}
	}
	a.apply(&agent.ItemCompleted{Item: agent.Item{ID: "u1", Kind: agent.ItemUserMessage, Status: agent.ItemDone, Text: "hi"}})
	a.status = "idle"
	a.startRewindLocked("")
	if a.rewind == nil || len(a.restoreModesLocked()) != 1 || a.rewind.restore != "conversation" {
		t.Fatalf("rewind %+v modes %v", a.rewind, a.restoreModesLocked())
	}
	if _, err := (&tern.Reconciler{}).Diff(a.view()); err != nil {
		t.Fatal(err)
	}
	a.rewind = nil
	a.toggleContextLocked()
	if a.showContext {
		t.Error("the context breakdown opened")
	}
}

// Codex's fuzzyFileSearch reply becomes paths relative to the chat folder.
func TestFuzzyFileResults(t *testing.T) {
	raw := json.RawMessage(`{"files":[
		{"file_name":"main.go","match_type":"file","path":"cmd/main.go","root":"/w","score":9},
		{"file_name":"internal","match_type":"directory","path":"internal","root":"/w","score":3},
		{"file_name":"x.go","match_type":"file","path":"/w/sub/x.go","root":"/w","score":1}]}`)
	got, err := fuzzyPaths(raw, "/w")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"cmd/main.go", "internal/", "sub/x.go"}; !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// /review's argument picks what Codex reviews.
func TestReviewArgs(t *testing.T) {
	for _, tc := range []struct {
		arg  string
		want codex.ReviewTarget
	}{
		{"", codex.ReviewUncommitted()},
		{"uncommitted", codex.ReviewUncommitted()},
		{"branch main", codex.ReviewBaseBranch("main")},
		{"commit abc123 Fix the parser", codex.ReviewCommit("abc123", "Fix the parser")},
		{"commit abc123", codex.ReviewCommit("abc123", "")},
		{"focus on error handling", codex.ReviewCustom("focus on error handling")},
	} {
		got, err := reviewTarget(tc.arg)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("reviewTarget(%q) = %v, %v; want %v", tc.arg, got, err, tc.want)
		}
	}
	for _, bad := range []string{"branch", "commit"} {
		if _, err := reviewTarget(bad); err == nil {
			t.Errorf("reviewTarget(%q) accepted a missing name", bad)
		}
	}
}

// "!cmd" runs a shell command only for an agent that offers it; for
// Claude it is an ordinary message.
func TestShellPrefix(t *testing.T) {
	if cmd, ok := shellCommand(codexTestApp(t), "!ls -la"); !ok || cmd != "ls -la" {
		t.Errorf("codex: %q %v", cmd, ok)
	}
	for _, text := range []string{"!", "! ", "hello"} {
		if _, ok := shellCommand(codexTestApp(t), text); ok {
			t.Errorf("codex took %q as a command", text)
		}
	}
	if _, ok := shellCommand(testApp(t), "!ls"); ok {
		t.Error("claude took !ls as a command")
	}
}

// Model-specific allowances show next to the main one and replace their
// own earlier report.
func TestOtherRateLimitsMerge(t *testing.T) {
	a := codexTestApp(t)
	a.apply(&agent.RateLimitUpdated{Limits: []agent.RateLimit{{Name: "primary", UsedPercent: 10}}})
	a.apply(&codex.OtherRateLimitUpdated{LimitID: "luna", LimitName: "GPT-6-Luna", Limits: []agent.RateLimit{{Name: "primary", UsedPercent: 40}}})
	a.apply(&codex.OtherRateLimitUpdated{LimitID: "luna", LimitName: "GPT-6-Luna", Limits: []agent.RateLimit{{Name: "primary", UsedPercent: 50}}})
	got := a.rateLimitsLocked()
	if len(got) != 2 || got[0].UsedPercent != 10 || got[1].UsedPercent != 50 || !strings.HasPrefix(got[1].Name, "GPT-6-Luna") {
		t.Fatalf("limits %+v", got)
	}
}

// tcodex never names Claude either.
func TestCodexWording(t *testing.T) {
	a := codexTestApp(t)
	a.apply(&agent.ItemCompleted{Item: agent.Item{ID: "u1", Kind: agent.ItemUserMessage, Status: agent.ItemDone, Text: "hi"}})
	a.requests = append(a.requests, &agent.PermissionRequest{ID: "p1", Kind: agent.PermissionFileChange, ToolName: "apply_patch"})
	for _, page := range []string{"", "session", "interface", "keys", "about"} {
		a.prefs = nil
		if page != "" {
			a.prefs = &prefsState{page: page}
		}
		var r tern.Reconciler
		ops, err := r.Diff(a.view())
		if err != nil {
			t.Fatal(err)
		}
		if raw, _ := json.Marshal(ops); strings.Contains(string(raw), "Claude") {
			t.Fatalf("page %q names Claude", page)
		}
	}
}

// Stopping a turn puts every queued message back in the composer, in
// order and ahead of the draft.
func TestRestoreQueued(t *testing.T) {
	a := codexTestApp(t)
	for _, id := range []string{"q1", "q2"} {
		a.apply(&agent.ItemCompleted{Item: agent.Item{ID: id, Kind: agent.ItemUserMessage, Status: agent.ItemDone, Text: "msg " + id}})
		a.queued[id] = true
	}
	a.composer.set("draft")
	a.restoreQueuedLocked(nil, true)
	if got := a.composer.String(); got != "msg q1\n\nmsg q2\n\ndraft" {
		t.Fatalf("composer %q", got)
	}
	if len(a.queued) != 0 || slices.Contains(a.order, "q1") {
		t.Fatalf("queued %v order %v", a.queued, a.order)
	}
}

// Codex names a turn apart from its user message; the turn's failure and
// usage still show under the message.
func TestCodexTurnFailureShown(t *testing.T) {
	a := codexTestApp(t)
	a.apply(&agent.ItemCompleted{Item: agent.Item{ID: "item-1", TurnID: "turn-1", Kind: agent.ItemUserMessage, Status: agent.ItemDone, Text: "hi"}})
	a.apply(&agent.ItemCompleted{Item: agent.Item{ID: "a1", TurnID: "turn-1", Kind: agent.ItemAssistantMessage, Status: agent.ItemDone, Text: "partial"}})
	a.apply(&agent.TurnCompleted{EventMeta: agent.EventMeta{TurnID: "turn-1"}, Result: &agent.TurnResult{TurnID: "turn-1",
		Err: &agent.TurnError{Message: "usage limit reached"}, Usage: agent.Usage{InputTokens: 10, OutputTokens: 5}}})
	var r tern.Reconciler
	ops, err := r.Diff(a.view())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(ops)
	for _, want := range []string{"usage limit reached", "this turn"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("view lacks %q", want)
		}
	}
}

// A receipt that names no cancelled message restores none.
func TestRestoreNamesOnly(t *testing.T) {
	a := testApp(t)
	a.apply(&agent.ItemCompleted{Item: agent.Item{ID: "q1", Kind: agent.ItemUserMessage, Status: agent.ItemDone, Text: "later"}})
	a.queued["q1"] = true
	a.restoreQueuedLocked(nil, false)
	if !a.queued["q1"] || a.composer.String() != "" {
		t.Fatalf("queued %v composer %q", a.queued, a.composer.String())
	}
}

// A queued message's stand-in leaves when its turn starts.
func TestStandInLeavesWhenTurnStarts(t *testing.T) {
	a := codexTestApp(t)
	a.entries["queued-1"] = &entry{item: agent.Item{ID: "queued-1", Kind: agent.ItemUserMessage, Text: "next"}, ver: 1}
	a.order = append(a.order, "queued-1")
	a.queued["queued-1"], a.queuedTurns["queued-1"] = true, nil
	a.apply(&agent.TurnStarted{EventMeta: agent.EventMeta{TurnID: "queued-1"}})
	if a.queued["queued-1"] || a.entries["queued-1"] != nil || slices.Contains(a.order, "queued-1") {
		t.Fatalf("stand-in kept: queued %v order %v", a.queued, a.order)
	}
}

// The branch comes from HEAD, also through a worktree's gitdir file.
func TestGitBranch(t *testing.T) {
	repo := t.TempDir()
	_ = os.MkdirAll(filepath.Join(repo, ".git"), 0o700)
	_ = os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("ref: refs/heads/feat/x\n"), 0o600)
	sub := filepath.Join(repo, "a", "b")
	_ = os.MkdirAll(sub, 0o700)
	if got := gitBranch(sub); got != "feat/x" {
		t.Errorf("branch %q", got)
	}
	wt := t.TempDir()
	meta := filepath.Join(repo, ".git", "worktrees", "wt")
	_ = os.MkdirAll(meta, 0o700)
	_ = os.WriteFile(filepath.Join(meta, "HEAD"), []byte("0123456789abcdef\n"), 0o600)
	_ = os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+meta+"\n"), 0o600)
	if got := gitBranch(wt); got != "0123456" {
		t.Errorf("detached worktree %q", got)
	}
	if got := gitBranch(t.TempDir()); got != "" {
		t.Errorf("outside a repo %q", got)
	}
}

// Claude Code reports a subagent's effort only to hooks, which the
// connector registers when asked.
func TestClaudeTracksSubagentEffort(t *testing.T) {
	var cfg agent.Config
	Claude().Prepare(&cfg)
	opts, ok := cfg.ProviderOptions.(*claude.SessionOptions)
	if !ok || !opts.TrackSubagentEffort {
		t.Fatalf("provider options %+v", cfg.ProviderOptions)
	}
}

// -mcp-config reads Claude Code's .mcp.json shape and refuses a server
// with nothing to run or reach.
func TestReadMCPConfig(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "mcp.json")
	_ = os.WriteFile(good, []byte(`{"mcpServers":{"fs":{"command":"npx","args":["-y","fs"]},"web":{"type":"http","url":"https://x/mcp","headers":{"A":"b"}}}}`), 0o600)
	servers, err := readMCPConfig(good)
	if err != nil {
		t.Fatal(err)
	}
	if fs := servers["fs"]; fs.Command != "npx" || len(fs.Args) != 2 {
		t.Errorf("fs %+v", fs)
	}
	if web := servers["web"]; web.Transport != agent.MCPHTTP || web.URL != "https://x/mcp" || web.Headers["A"] != "b" {
		t.Errorf("web %+v", web)
	}
	bad := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(bad, []byte(`{"mcpServers":{"x":{}}}`), 0o600)
	if _, err := readMCPConfig(bad); err == nil {
		t.Error("accepted a server with no command or url")
	}
	if got := splitList(" Bash(git:*), Read ,,"); !slices.Equal(got, []string{"Bash(git:*)", "Read"}) {
		t.Errorf("splitList %q", got)
	}
}

// Deleting from a picker takes a second press of the same key on the same
// row; other keys and rows start over.
func TestPickerConfirm(t *testing.T) {
	a := testApp(t)
	ran := 0
	a.openPickerLocked(&menu{title: "T", items: []choice{{Label: "one"}, {Label: "two"}},
		actions: []menuAction{{id: "delete", label: "Delete", key: "ctrl+d", confirm: true, run: func(int) { ran++ }}}})
	m := a.menu
	a.menuKey(tern.Key{Name: "d", Ctrl: true})
	if ran != 0 {
		t.Fatal("ran on the first press")
	}
	a.menuKey(tern.Key{Name: "down"})
	a.menuKey(tern.Key{Name: "d", Ctrl: true})
	if ran != 0 {
		t.Fatal("ran after moving to another row")
	}
	a.menuKey(tern.Key{Name: "d", Ctrl: true})
	if ran != 1 || a.menu != m {
		t.Fatalf("ran %d", ran)
	}
	// A first press long ago no longer counts.
	a.menuKey(tern.Key{Name: "d", Ctrl: true})
	m.armedAt = m.armedAt.Add(-2 * confirmWindow)
	a.menuKey(tern.Key{Name: "d", Ctrl: true})
	if ran != 1 {
		t.Fatal("ran on a stale first press")
	}
}

// Renaming from /resume borrows the composer; Esc gives the draft back.
func TestRenameThroughComposer(t *testing.T) {
	a := testApp(t)
	a.renaming = &renameTarget{id: "s1", title: "Old", draft: "half a thought"}
	a.composer.set("Old")
	_ = a.key(tern.Key{Name: "escape"})
	if a.renaming != nil || a.composer.String() != "half a thought" {
		t.Fatalf("escape left %+v %q", a.renaming, a.composer.String())
	}
}

// Codex's reasoning summaries are labelled as summaries.
func TestReasoningSummaryLabel(t *testing.T) {
	a := codexTestApp(t)
	a.apply(&agent.ReasoningDelta{ItemID: "r1", Delta: "Looking at the loader", Summary: true})
	a.apply(&agent.ItemCompleted{Item: agent.Item{ID: "r1", Kind: agent.ItemReasoning, Status: agent.ItemDone, Text: "Looking at the loader"}})
	var r tern.Reconciler
	ops, _ := r.Diff(a.view())
	if raw, _ := json.Marshal(ops); !strings.Contains(string(raw), `"Summary"`) {
		t.Error("summary not labelled")
	}
}

// Program status reports are omp's OSC 7501: one write per change, the
// message only for blocked and error, base64, and a clear on exit.
func TestProgramStatus(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "tty")
	if err != nil {
		t.Fatal(err)
	}
	tty := &ttyOut{f: f}
	tty.report("tclaude", activity{state: "working", msg: "ignored"})
	tty.report("tclaude", activity{state: "working"})
	tty.report("tclaude", activity{state: "blocked", kind: "question", msg: "Which  style?\n"})
	tty.clear()
	got, _ := os.ReadFile(f.Name())
	want := "\x1b]7501;state=working:app=tclaude\x1b\\" +
		"\x1b]7501;state=blocked:kind=question:app=tclaude:msg=" + base64.StdEncoding.EncodeToString([]byte("Which style?")) + "\x1b\\" +
		"\x1b]7501;state=clear\x1b\\"
	if string(got) != want {
		t.Fatalf("wrote %q\nwant  %q", got, want)
	}

	a := testApp(t)
	if a.activityLocked().state != "idle" {
		t.Error("new app not idle")
	}
	a.status = "running"
	if a.activityLocked().state != "working" {
		t.Error("running app not working")
	}
	a.requests = append(a.requests, &agent.PermissionRequest{ID: "p1", Kind: agent.PermissionCommand, ToolName: "Bash", Command: "rm -rf build"})
	if act := a.activityLocked(); act.state != "blocked" || act.kind != "permission" || !strings.Contains(act.msg, "rm -rf build") {
		t.Errorf("permission %+v", act)
	}
	a.requests, a.status = nil, "idle"
	a.apply(&agent.TurnCompleted{EventMeta: agent.EventMeta{TurnID: "t1"}, Result: &agent.TurnResult{TurnID: "t1", Err: &agent.TurnError{Message: "rate limited"}}})
	if act := a.activityLocked(); act.state != "error" || !strings.Contains(act.msg, "rate limited") {
		t.Errorf("failed turn %+v", act)
	}
}

// A hook's output is a folded card that says what it added, not a wall of
// text; Claude's "<event>:<matcher>" name is not repeated.
func TestHookCard(t *testing.T) {
	c := hookCard("SessionStart", "SessionStart:startup", "# Memory\n\n- one\n- two")
	if c.Title != "SessionStart hook" || c.Meta != "startup · 4 lines added to the context" || !c.Collapsed || !c.Plain {
		t.Fatalf("card %+v", c)
	}
	if c := hookCard("Stop", "Stop", "checked"); c.Meta != "1 line" {
		t.Errorf("stop hook meta %q", c.Meta)
	}
}

// countingBackend never starts an agent; it counts the attempts.
type countingBackend struct {
	claudeBackend
	starts *atomic.Int32
}

func (b countingBackend) Start(context.Context, agent.Config) (agent.Session, error) {
	b.starts.Add(1)
	return nil, errors.New("not in tests")
}

func (countingBackend) Models(context.Context, agent.Session) []agent.Model { return nil }

func TestNewConversationWaitsForFirstMessage(t *testing.T) {
	a := testApp(t)
	starts := &atomic.Int32{}
	a.backend = countingBackend{claudeBackend: a.backend.(claudeBackend), starts: starts}
	a.start(agent.Config{})
	time.Sleep(50 * time.Millisecond)
	a.mu.Lock()
	if starts.Load() != 0 || a.status != "ready" || a.deferred == nil {
		t.Fatalf("started before a message: starts %d, status %q", starts.Load(), a.status)
	}
	a.setModelLocked("claude-opus-5-5")
	a.composer.set("hello")
	a.submitLocked(agent.DeliverQueue)
	a.mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for {
		a.mu.Lock()
		retry := a.deferred != nil
		text := a.composer.String()
		a.mu.Unlock()
		if retry && text == "hello" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("failed start: deferred %v, composer %q", retry, text)
		}
		time.Sleep(10 * time.Millisecond)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if starts.Load() != 1 || a.deferred.Model != "claude-opus-5-5" || len(a.order) != 0 {
		t.Errorf("starts %d, model %q, order %v", starts.Load(), a.deferred.Model, a.order)
	}
}
