package chat

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/stencil-hq/tern-sdk/go/tern/ui"
)

// maxOutputBytes caps tool output drawn in the transcript; the tail is kept
// because the end of a command's output usually explains how it ended.
const maxOutputBytes = 24 << 10

// toolLook is how a tool call is drawn: Tern's tool icon name, the verb,
// and the main argument.
type toolLook struct {
	Icon       string
	Title      string
	Target     string
	TargetKind ui.TargetKind
	Meta       []string
	Output     string
	Diff       string
	DiffPath   string
	Intent     string // what the call is for, in the model's words
	Inline     bool   // a lightweight step drawn without a frame
	Listing    bool   // the output is a list of paths
}

var foundRe = regexp.MustCompile(`^Found \d+ files?$`)

func lookOf(c *agent.ToolCall, cwd string) toolLook {
	var in map[string]any
	_ = json.Unmarshal(c.Input, &in)
	str := func(k string) string {
		s, _ := in[k].(string)
		return s
	}
	rel := func(p string) string { return relPath(p, cwd) }
	l := toolLook{Title: c.Name, Output: tail(c.Output, maxOutputBytes)}
	switch c.Kind {
	case agent.ToolShell:
		l.Icon, l.Title, l.TargetKind = "bash", "Run", ui.TargetCommand
		l.Target = firstNonEmpty(c.Command, str("command"))
		l.Intent = str("description")
	case agent.ToolRead:
		l.Icon, l.Title, l.TargetKind, l.Inline = "read", "Read", ui.TargetPath, true
		l.Target = rel(firstNonEmpty(c.Path, str("file_path")))
		if n := strings.Count(c.Output, "\n"); n > 0 {
			l.Meta = []string{plural(n, "line")}
		}
		l.Output = ""
	case agent.ToolEdit:
		l.Icon, l.Title, l.TargetKind = "edit", "Edit", ui.TargetPath
		if c.Name == "Write" {
			l.Title = "Write"
		}
		path := firstNonEmpty(c.Path, str("file_path"), str("notebook_path"))
		l.Target, l.DiffPath = rel(path), path
		for _, ch := range c.Changes {
			switch {
			case ch.MovePath != "":
				l.Title, l.Target = "Rename", rel(ch.Path)+" → "+rel(ch.MovePath)
			case ch.Kind == agent.FileDeleted && len(c.Changes) == 1:
				l.Title = "Delete"
			case ch.Kind == agent.FileAdded && len(c.Changes) == 1 && l.Title == "Edit":
				l.Title = "Create"
			}
		}
		l.Diff = editDiff(c.Changes, in)
		if add, del := diffStat(l.Diff); add+del > 0 {
			l.Meta = []string{fmt.Sprintf("+%d −%d", add, del)}
		}
		if !c.IsError {
			l.Output = ""
		}
	case agent.ToolSearch:
		l.Icon, l.Title, l.TargetKind, l.Inline = "grep", "Search", ui.TargetPattern, true
		l.Listing = c.Name == "Glob" || str("output_mode") == "" || str("output_mode") == "files_with_matches"
		if c.Name == "Glob" {
			l.Icon, l.Title = "glob", "Find"
		}
		l.Target = firstNonEmpty(c.Query, str("pattern"))
		if p := str("path"); p != "" {
			l.Meta = []string{"in " + rel(p)}
		}
		if l.Listing {
			// Grep heads its list with "Found N files"; it is not a path.
			out := strings.TrimSpace(c.Output)
			if first, rest, _ := strings.Cut(out, "\n"); foundRe.MatchString(first) {
				out = strings.TrimSpace(rest)
			} else if out == "No files found" {
				out = ""
			}
			l.Output = tail(out, maxOutputBytes)
			if out != "" {
				l.Meta = append(l.Meta, plural(len(splitLines(out)), "file"))
			}
		}
	case agent.ToolWeb:
		l.Icon, l.Inline = "web", true
		if u := firstNonEmpty(c.URL, str("url")); u != "" {
			l.Title, l.Target = "Fetch", u
		} else {
			l.Title, l.TargetKind = "Search", ui.TargetQuery
			l.Target = firstNonEmpty(c.Query, str("query"))
		}
	case agent.ToolSubagent:
		l.Icon, l.Title = "task", "Agent"
		l.Target = firstNonEmpty(str("description"), firstLine(str("prompt")))
	case agent.ToolMCP:
		l.Icon = "mcp"
		l.Title = firstNonEmpty(c.DisplayName, mcpToolName(c.Name))
		l.Target = firstNonEmpty(c.ServerDisplayName, c.Server)
	case agent.ToolQuestion:
		l.Icon, l.Title = "question", "Question"
		if qs, _ := in["questions"].([]any); len(qs) > 0 {
			if q, _ := qs[0].(map[string]any); q != nil {
				l.Target, _ = q["question"].(string)
			}
		}
	case agent.ToolPlan:
		l.Icon, l.Title = "plan", "Plan"
		l.Target = strings.TrimLeft(firstLine(str("plan")), "# ")
	default:
		l.Icon = strings.ToLower(c.Name)
		l.Target = summarizeInput(in)
	}
	return l
}

// editDiff prefers the harness's unified diff and otherwise builds one
// from Claude's Edit, MultiEdit, and Write inputs.
func editDiff(changes []agent.FileChange, in map[string]any) string {
	var b strings.Builder
	for _, ch := range changes {
		b.WriteString(ch.Diff)
		if ch.Diff != "" && !strings.HasSuffix(ch.Diff, "\n") {
			b.WriteByte('\n')
		}
	}
	if b.Len() > 0 {
		return tail(b.String(), maxOutputBytes)
	}
	hunk := func(old, new string) {
		o, n := splitLines(old), splitLines(new)
		fmt.Fprintf(&b, "@@ -1,%d +1,%d @@\n", len(o), len(n))
		for _, l := range o {
			b.WriteString("-" + l + "\n")
		}
		for _, l := range n {
			b.WriteString("+" + l + "\n")
		}
	}
	if old, ok := in["old_string"].(string); ok {
		newS, _ := in["new_string"].(string)
		hunk(old, newS)
	} else if edits, ok := in["edits"].([]any); ok {
		for _, e := range edits {
			m, _ := e.(map[string]any)
			old, _ := m["old_string"].(string)
			newS, _ := m["new_string"].(string)
			hunk(old, newS)
		}
	} else if content, ok := in["content"].(string); ok {
		hunk("", content)
	}
	return tail(b.String(), maxOutputBytes)
}

func diffStat(diff string) (add, del int) {
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"):
		case strings.HasPrefix(l, "+"):
			add++
		case strings.HasPrefix(l, "-"):
			del++
		}
	}
	return add, del
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

func mcpToolName(name string) string {
	if i := strings.LastIndex(name, "__"); i >= 0 {
		return name[i+2:]
	}
	return name
}

func summarizeInput(in map[string]any) string {
	for _, k := range []string{"command", "file_path", "path", "pattern", "query", "url", "description", "prompt"} {
		if s, ok := in[k].(string); ok && s != "" {
			return firstLine(s)
		}
	}
	return ""
}

// relPath shows p relative to the chat's directory when it lies inside it.
func relPath(p, cwd string) string {
	if p == "" || cwd == "" || !filepath.IsAbs(p) {
		return p
	}
	if rel, err := filepath.Rel(cwd, p); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
		return rel
	}
	return p
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// tail keeps the last max bytes of s, cut at a line start (or at least at a
// rune boundary).
func tail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := s[len(s)-max:]
	if i := strings.IndexByte(cut, '\n'); i >= 0 && i < len(cut)-1 {
		cut = cut[i+1:]
	}
	for len(cut) > 0 && !utf8.RuneStart(cut[0]) {
		cut = cut[1:]
	}
	return "… (earlier output trimmed)\n" + cut
}

func runState(s agent.ItemStatus, isError bool) ui.RunState {
	switch {
	case s == agent.ItemRunning:
		return ui.Running
	case s == agent.ItemFailed || isError:
		return ui.Errored
	case s == agent.ItemDeclined || s == agent.ItemCancelled:
		return ui.Cancelled
	default:
		return ui.Done
	}
}

func taskState(s agent.TaskStatus) ui.AgentState {
	switch s {
	case agent.TaskRunning:
		return ui.AgentRunning
	case agent.TaskCompleted:
		return ui.AgentDone
	case agent.TaskFailed:
		return ui.AgentFailed
	case agent.TaskStopped:
		return ui.AgentAborted
	default:
		return ui.AgentPending
	}
}

func ms(d float64) *float64 { return &d }

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// limitName turns a rate-limit id such as "five_hour" into "5-hour".
func limitName(name string) string {
	r := strings.NewReplacer("five_hour", "5-hour", "seven_day", "weekly", "_", " ")
	return r.Replace(name)
}
