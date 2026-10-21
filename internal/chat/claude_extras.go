package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/claude"
)

// Claude Code's own controls that the shared API lacks: subagents,
// permission rules, plan usage, reloading, moving folders, MCP sign-in,
// backgrounding a command, and a health check.

// claudeAsync runs fn with the Claude session off the lock and shows its
// card if the conversation is still the same.
func (a *app) claudeAsync(what string, fn func(ctx context.Context, cs *claude.Session) (localCard, error)) {
	cs, err := a.claudeSession()
	if err != nil {
		a.toastLocked("error", err.Error())
		return
	}
	gen := a.gen
	a.async(what, func(ctx context.Context) error {
		card, err := fn(ctx, cs)
		if err != nil {
			return err
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if gen == a.gen && card.Title != "" {
			a.addLocal(card)
		}
		return nil
	})
}

// prettyJSON is raw as indented JSON in a code block, for replies whose
// shape Claude Code marks experimental.
func prettyJSON(raw json.RawMessage) string {
	var out bytes.Buffer
	if json.Indent(&out, raw, "", "  ") != nil {
		out.Reset()
		out.Write(raw)
	}
	return "```json\n" + tail(out.String(), 8000) + "\n```"
}

func (a *app) cmdAgents(string) {
	cs, err := a.claudeSession()
	if err != nil {
		a.toastLocked("error", err.Error())
		return
	}
	agents := cs.SupportedAgents()
	if len(agents) == 0 {
		a.toastLocked("info", "No subagents are defined")
		return
	}
	var b strings.Builder
	b.WriteString("| Agent | Model | What it does |\n| --- | --- | --- |\n")
	for _, ag := range agents {
		fmt.Fprintf(&b, "| `%s` | %s | %s |\n", ag.Name, firstNonEmpty(ag.Model, "inherits"), strings.ReplaceAll(firstLine(ag.Description), "|", "\\|"))
	}
	a.addLocal(localCard{Title: "Subagents", Text: b.String()})
}

func (a *app) cmdPermissions(string) {
	a.claudeAsync("Permission rules", func(ctx context.Context, cs *claude.Session) (localCard, error) {
		raw, err := cs.PermissionRules(ctx)
		if err != nil {
			return localCard{}, err
		}
		var st struct {
			Rules []struct {
				Behavior, Source, Rule, Editability string
				NotInEffect                         bool
			} `json:"rules"`
			ManagedOnly bool `json:"managedOnly"`
		}
		if json.Unmarshal(raw, &st) != nil {
			return localCard{Title: "Permission rules", Text: prettyJSON(raw)}, nil
		}
		var b strings.Builder
		if len(st.Rules) == 0 {
			b.WriteString("No permission rules.")
		} else {
			b.WriteString("| | Rule | From |\n| --- | --- | --- |\n")
		}
		for _, r := range st.Rules {
			rule := "`" + r.Rule + "`"
			if r.NotInEffect {
				rule += " _(not in effect)_"
			}
			fmt.Fprintf(&b, "| %s | %s | %s |\n", r.Behavior, rule, r.Source)
		}
		if st.ManagedOnly {
			b.WriteString("\nManaged settings allow only policy rules.")
		}
		return localCard{Title: "Permission rules", Text: b.String()}, nil
	})
}

func (a *app) cmdUsage(string) {
	a.claudeAsync("Usage", func(ctx context.Context, cs *claude.Session) (localCard, error) {
		limits, _ := cs.RateLimits(ctx)
		raw, err := cs.PlanUsage(ctx, false)
		if err != nil && len(limits) == 0 {
			return localCard{}, err
		}
		var b strings.Builder
		for _, l := range limits {
			fmt.Fprintf(&b, "- **%s** %.0f%% used%s\n", limitName(l.Name), l.UsedPercent, resetsIn(l.ResetsAt))
		}
		if len(raw) > 0 {
			b.WriteString("\n" + prettyJSON(raw))
		}
		return localCard{Title: "Usage", Text: b.String()}, nil
	})
}

// generateTitleLocked names the conversation from its first message.
func (a *app) generateTitleLocked() {
	first := ""
	for _, id := range a.order {
		if e := a.entries[id]; e != nil && e.local == nil && e.item.Kind == agent.ItemUserMessage && !e.item.Synthetic {
			first = e.item.Text
			break
		}
	}
	if first == "" {
		a.toastLocked("info", "Send a message first, or /title <title>")
		return
	}
	a.claudeAsync("Title", func(ctx context.Context, cs *claude.Session) (localCard, error) {
		title, err := cs.GenerateTitle(ctx, tail(first, 2000), true)
		if err != nil {
			return localCard{}, err
		}
		a.mu.Lock()
		a.info.Title = title
		a.toastLocked("success", "Titled: "+title)
		a.mu.Unlock()
		return localCard{}, nil
	})
}

func (a *app) cmdReload(string) {
	a.claudeAsync("Reload", func(ctx context.Context, cs *claude.Session) (localCard, error) {
		var failed []string
		if _, err := cs.ReloadSkills(ctx); err != nil {
			failed = append(failed, "skills: "+err.Error())
		}
		if _, err := cs.ReloadPlugins(ctx, nil, false); err != nil {
			failed = append(failed, "plugins: "+err.Error())
		}
		if _, err := cs.ReloadOutputStyles(ctx); err != nil {
			failed = append(failed, "output styles: "+err.Error())
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if len(failed) > 0 {
			a.toastLocked("error", "Reload: "+strings.Join(failed, "; "))
		} else {
			a.toastLocked("success", "Reloaded skills, plugins and output styles")
		}
		return localCard{}, nil
	})
}

// cmdCd moves the session to dir; files and commands then resolve there.
func (a *app) cmdCd(arg string) {
	dir := strings.TrimSpace(arg)
	if dir == "" {
		a.toastLocked("info", "Usage: /cd <folder>")
		return
	}
	if strings.HasPrefix(dir, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, dir[2:])
		}
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(a.cwd, dir)
	}
	dir = filepath.Clean(dir)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		a.toastLocked("error", "Not a folder: "+tildePath(dir))
		return
	}
	gen := a.gen
	a.claudeAsync("Change folder", func(ctx context.Context, cs *claude.Session) (localCard, error) {
		if _, err := cs.SetCwd(ctx, dir); err != nil {
			return localCard{}, err
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if gen != a.gen {
			return localCard{}, nil
		}
		// New conversations, and the spare for them, start here too.
		a.cwd, a.base.Cwd, a.branchAt, a.pathCache = dir, dir, time.Time{}, map[string]string{}
		a.toastLocked("success", "Now in "+tildePath(dir))
		return localCard{}, nil
	})
}

// mcpAuthLocked signs in to an MCP server (login), finishes a sign-in
// whose browser redirect did not come back (callback), or signs out.
func (a *app) mcpAuthLocked(verb, rest string) {
	server, extra, _ := strings.Cut(strings.TrimSpace(rest), " ")
	if server == "" || (verb == "callback" && strings.TrimSpace(extra) == "") {
		a.toastLocked("info", "Usage: /mcp login|logout <server>, or /mcp callback <server> <redirect URL>")
		return
	}
	a.claudeAsync("MCP "+verb, func(ctx context.Context, cs *claude.Session) (localCard, error) {
		var raw json.RawMessage
		var err error
		switch verb {
		case "login":
			raw, err = cs.MCPAuthenticate(ctx, server, "")
		case "logout":
			raw, err = cs.MCPClearAuth(ctx, server)
		default:
			raw, err = cs.MCPSubmitOAuthCallback(ctx, server, strings.TrimSpace(extra))
		}
		if err != nil {
			return localCard{}, err
		}
		text := "Done."
		var r struct {
			AuthURL string `json:"authUrl"`
		}
		if json.Unmarshal(raw, &r) == nil && r.AuthURL != "" {
			text = "Open [the sign-in page](" + r.AuthURL + ") to sign in to `" + server + "`. If the browser can't return here, copy the address it ends on and run `/mcp callback " + server + " <address>`."
		} else if len(bytes.TrimSpace(raw)) > 2 {
			text = prettyJSON(raw)
		}
		return localCard{Title: "MCP " + verb + " · " + server, Text: text, Tone: "info"}, nil
	})
}

// backgroundLocked moves the running command to the background, as
// Claude Code's Ctrl+B does.
func (a *app) backgroundLocked() {
	t := a.runningToolLocked()
	if t == nil || t.Kind != agent.ToolShell {
		a.toastLocked("info", "No command is running")
		return
	}
	a.claudeAsync("Background", func(ctx context.Context, cs *claude.Session) (localCard, error) {
		ok, err := cs.BackgroundTasks(ctx, "")
		if err != nil {
			return localCard{}, err
		}
		a.mu.Lock()
		if ok {
			a.toastLocked("success", "Moved to the background · /tasks shows it")
		} else {
			a.toastLocked("info", "Nothing could move to the background")
		}
		a.mu.Unlock()
		return localCard{}, nil
	})
}

// cmdDoctor checks the agent's CLI: installed, version, sign-in, and what
// this session's CLI can do.
func (a *app) cmdDoctor(string) {
	checker, ok := a.backend.Provider().(agent.Checker)
	if !ok {
		a.toastLocked("error", "Checking is not supported")
		return
	}
	cs, _ := a.claudeSession()
	gen := a.gen
	a.async("Doctor", func(ctx context.Context) error {
		st, err := checker.Check(ctx)
		if err != nil {
			return err
		}
		var b strings.Builder
		b.WriteString("| | |\n| --- | --- |\n")
		fmt.Fprintf(&b, "| Installed | %v |\n| Version | %s |\n", st.Installed, firstNonEmpty(st.Version, "—"))
		if st.Auth != nil {
			fmt.Fprintf(&b, "| Signed in | %v |\n| Method | %s |\n| Account | %s |\n| Plan | %s |\n",
				st.Auth.Authenticated, firstNonEmpty(st.Auth.Method, "—"), firstNonEmpty(st.Auth.Email, "—"), firstNonEmpty(st.Auth.Plan, "—"))
		}
		if st.Err != nil {
			fmt.Fprintf(&b, "| Problem | %s |\n", st.Err)
		}
		if cs != nil {
			if caps := cs.CLICapabilities(); len(caps) > 0 {
				fmt.Fprintf(&b, "\n**This session's CLI can:** %s\n", strings.Join(caps, ", "))
			}
			if raw, err := cs.Status(ctx); err == nil && len(raw) > 2 {
				b.WriteString("\n" + prettyJSON(raw))
			}
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if gen == a.gen {
			a.addLocal(localCard{Title: "Doctor · " + a.backend.Product(), Text: b.String()})
		}
		return nil
	})
}
