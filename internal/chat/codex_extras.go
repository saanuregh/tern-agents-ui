package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/codex"
)

// Codex's own commands and controls: code review, goals, shell commands,
// the turn's diff, background terminals, extra rate limits, its account
// and MCP status.

// reviewTarget reads /review's argument: nothing or "uncommitted",
// "branch <base>", "commit <sha> [title]", or instructions of your own.
func reviewTarget(arg string) (codex.ReviewTarget, error) {
	verb, rest, _ := strings.Cut(strings.TrimSpace(arg), " ")
	rest = strings.TrimSpace(rest)
	switch verb {
	case "", "uncommitted":
		if rest == "" {
			return codex.ReviewUncommitted(), nil
		}
	case "branch":
		if rest == "" {
			return nil, errors.New("Usage: /review branch <base branch>")
		}
		return codex.ReviewBaseBranch(rest), nil
	case "commit":
		if rest == "" {
			return nil, errors.New("Usage: /review commit <sha> [title]")
		}
		sha, title, _ := strings.Cut(rest, " ")
		return codex.ReviewCommit(sha, strings.TrimSpace(title)), nil
	}
	return codex.ReviewCustom(strings.TrimSpace(arg)), nil
}

func (a *app) cmdReview(arg string) {
	target, err := reviewTarget(arg)
	if err != nil {
		a.toastLocked("info", err.Error())
		return
	}
	cs, err := a.codexSession()
	if err != nil {
		a.toastLocked("error", err.Error())
		return
	}
	if a.running() {
		a.toastLocked("info", "Review once "+a.backend.Name()+" finishes")
		return
	}
	a.async("Review", func(ctx context.Context) error {
		_, err := cs.Review(ctx, target)
		return err
	})
}

// cmdGoal shows the goal, or sets, pauses, resumes or clears it.
func (a *app) cmdGoal(arg string) {
	cs, err := a.codexSession()
	if err != nil {
		a.toastLocked("error", err.Error())
		return
	}
	gen := a.gen
	switch strings.TrimSpace(arg) {
	case "":
		a.async("Goal", func(ctx context.Context) error {
			g, err := cs.Goal(ctx)
			if err != nil {
				return err
			}
			a.mu.Lock()
			defer a.mu.Unlock()
			if gen != a.gen {
				return nil
			}
			if g == nil || g.Objective == "" {
				a.toastLocked("info", "No goal. /goal <objective> sets one")
				return nil
			}
			var facts []string
			if g.Status != "" {
				facts = append(facts, string(g.Status))
			}
			if g.TokenBudget > 0 {
				facts = append(facts, fmt.Sprintf("%s of %s tokens", compact(g.TokensUsed), compact(g.TokenBudget)))
			} else if g.TokensUsed > 0 {
				facts = append(facts, compact(g.TokensUsed)+" tokens")
			}
			if g.Iterations > 0 {
				facts = append(facts, plural(g.Iterations, "turn"))
			}
			if g.TimeUsed > 0 {
				facts = append(facts, fmtDuration(g.TimeUsed))
			}
			text := g.Objective
			if len(facts) > 0 {
				text += "\n\n_" + strings.Join(facts, " · ") + "_"
			}
			a.addLocal(localCard{Title: "Goal", Text: text, Tone: "info"})
			return nil
		})
	case "pause":
		a.async("Goal", func(ctx context.Context) error { return cs.SetGoalStatus(ctx, "paused") })
	case "resume":
		a.async("Goal", func(ctx context.Context) error { return cs.SetGoalStatus(ctx, "active") })
	case "clear":
		a.async("Goal", func(ctx context.Context) error { return cs.ClearGoal(ctx) })
	default:
		a.async("Goal", func(ctx context.Context) error { return cs.SetGoal(ctx, strings.TrimSpace(arg), 0) })
	}
}

// shellCommand reads "!cmd" as a shell command for an agent that runs
// them; anything else is a message.
func shellCommand(a *app, text string) (string, bool) {
	cmd, ok := strings.CutPrefix(text, "!")
	cmd = strings.TrimSpace(cmd)
	return cmd, ok && cmd != "" && a.backend.Has(featShell)
}

// shellLocked runs cmd in the conversation; its output arrives as items.
// A command that can't run goes back to the composer, as a message does.
func (a *app) shellLocked(cmd string) {
	restore := func() {
		if len(a.composer.text) == 0 {
			a.composer.set("!" + cmd)
		}
	}
	sess, gen, err := a.liveSession()
	cs, ok := sess.(*codex.Session)
	if err != nil || !ok {
		a.toastLocked("error", firstNonEmpty(errText(err), "Shell commands need Codex"))
		restore()
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(a.ctx, 2*time.Minute)
		defer cancel()
		if err := cs.ShellCommand(ctx, cmd, 0); err != nil && !errors.Is(err, context.Canceled) {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.toastLocked("error", "Shell: "+err.Error())
			if gen == a.gen {
				restore()
			}
		}
	}()
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// cmdDiff shows what the current or last turn changed.
func (a *app) cmdDiff(string) {
	if strings.TrimSpace(a.turnDiff) == "" {
		a.toastLocked("info", "No changes in this turn yet")
		return
	}
	a.addLocal(localCard{Title: "Changes this turn", Text: "```diff\n" + a.turnDiff + "\n```"})
}

// rateLimitsLocked lists the main allowance's windows, then each other
// allowance's, named after it.
func (a *app) rateLimitsLocked() []agent.RateLimit {
	out := append([]agent.RateLimit(nil), a.rateLimits...)
	for _, id := range a.otherLimitOrder {
		out = append(out, a.otherLimits[id]...)
	}
	return out
}

func (a *app) applyOtherLimitsLocked(ev *codex.OtherRateLimitUpdated) {
	if _, ok := a.otherLimits[ev.LimitID]; !ok {
		a.otherLimitOrder = append(a.otherLimitOrder, ev.LimitID)
	}
	var limits []agent.RateLimit
	for _, l := range ev.Limits {
		l.Name = strings.TrimSpace(firstNonEmpty(ev.LimitName, ev.LimitID) + " " + limitName(l.Name))
		limits = append(limits, l)
	}
	a.otherLimits[ev.LimitID] = limits
}

// fetchAccountLocked reads Codex's account for /status and /config.
func (a *app) fetchAccountLocked(gen int, cs *codex.Session) {
	go func() {
		ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
		defer cancel()
		raw, err := cs.Client().Account(ctx)
		if err != nil {
			return
		}
		var res struct {
			Account *struct {
				Type     string `json:"type"`
				Email    string `json:"email"`
				PlanType string `json:"planType"`
			} `json:"account"`
		}
		if json.Unmarshal(raw, &res) != nil || res.Account == nil {
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if gen == a.gen {
			a.account = accountInfo{label: firstNonEmpty(res.Account.Email, res.Account.Type), plan: res.Account.PlanType, auth: res.Account.Type, known: true}
		}
	}()
}

// codexMCPLocked lists Codex's MCP servers.
func (a *app) codexMCPLocked(cs *codex.Session) {
	gen := a.gen
	a.async("MCP", func(ctx context.Context) error {
		raws, err := cs.MCPServerStatus(ctx, codex.MCPServerStatusOptions{Detail: "toolsAndAuthOnly"})
		if err != nil {
			return err
		}
		var b strings.Builder
		if len(raws) == 0 {
			b.WriteString("No MCP servers are configured.")
		} else {
			b.WriteString("| Server | Status | Sign-in |\n| --- | --- | --- |\n")
		}
		for _, raw := range raws {
			var s struct {
				Name          string `json:"name"`
				AuthStatus    any    `json:"authStatus"`
				RuntimeStatus any    `json:"runtimeStatus"`
			}
			if json.Unmarshal(raw, &s) == nil {
				fmt.Fprintf(&b, "| %s | %s | %s |\n", s.Name, statusText(s.RuntimeStatus), statusText(s.AuthStatus))
			}
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if gen == a.gen {
			a.addLocal(localCard{Title: "MCP servers", Text: b.String()})
		}
		return nil
	})
}

// statusText shows a status that is a string or an object with a status
// or type field.
func statusText(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case map[string]any:
		for _, k := range []string{"status", "type", "state"} {
			if s, ok := v[k].(string); ok {
				return s
			}
		}
	}
	return "—"
}

func (a *app) cmdArchive(string)   { a.archiveLocked(true) }
func (a *app) cmdUnarchive(string) { a.archiveLocked(false) }

// archiveLocked moves the conversation into Codex's archive or out of it.
func (a *app) archiveLocked(archive bool) {
	cs, err := a.codexSession()
	if err != nil {
		a.toastLocked("error", err.Error())
		return
	}
	a.async("Archive", func(ctx context.Context) error {
		var err error
		if archive {
			err = cs.Archive(ctx)
		} else {
			err = cs.Unarchive(ctx)
		}
		if err != nil {
			return err
		}
		msg := "Back from the archive"
		if archive {
			msg = "Archived; /unarchive brings it back"
		}
		a.mu.Lock()
		a.toastLocked("success", msg)
		a.mu.Unlock()
		return nil
	})
}

// cmdFeedback sends what went wrong to OpenAI; Codex's logs go along only
// with --logs, since they hold the conversation.
func (a *app) cmdFeedback(arg string) {
	reason, logs := strings.TrimSpace(arg), false
	if rest, ok := strings.CutPrefix(reason, "--logs"); ok {
		reason, logs = strings.TrimSpace(rest), true
	}
	if reason == "" {
		a.toastLocked("info", "Usage: /feedback [--logs] <what went wrong>")
		return
	}
	cs, err := a.codexSession()
	if err != nil {
		a.toastLocked("error", err.Error())
		return
	}
	a.async("Feedback", func(ctx context.Context) error {
		if err := cs.UploadFeedback(ctx, "bug", reason, logs); err != nil {
			return err
		}
		a.mu.Lock()
		a.toastLocked("success", "Feedback sent")
		a.mu.Unlock()
		return nil
	})
}
