package chat

import (
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sync"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/claude"
)

// claudeEntrypoint names this program in the sessions Claude Code stores.
// The connector would record "sdk-ts", and Claude Code's own /resume and
// --continue hide sdk-ts, sdk-py and sdk-cli sessions (it also turns "cli"
// into "sdk-cli" when driven over stream-json), so any other name keeps
// these conversations listed in the regular claude CLI.
const claudeEntrypoint = "tclaude"

type claudeBackend struct {
	provider *claude.Provider
	spare    *spare // a parked process for the next new session
}

// spare holds at most one parked Claude Code process.
type spare struct {
	mu      sync.Mutex
	sp      *claude.Spare
	cwd     string // the folder sp was started in
	warming bool   // a Prewarm is under way
	closed  bool   // the program is quitting: park nothing more
}

// Claude is the backend for Claude Code.
func Claude() backend {
	return claudeBackend{provider: claude.New(claude.Options{ClientApp: "tclaude/" + version}), spare: &spare{}}
}

// Start claims the parked process for a new conversation and starts
// normally when there is none, the conversation is resumed or forked, the
// folder changed, or the claim is refused (the refused spare is closed).
func (b claudeBackend) Start(ctx context.Context, cfg agent.Config) (agent.Session, error) {
	if cfg.Resume == "" && !cfg.Fork {
		if sp, cwd := b.spare.take(); sp != nil {
			if cwd == cfg.Cwd {
				// The spare already has the settings fixed at its start;
				// passing them again would only draw "ignored" notices.
				claim := cfg
				claim.SystemPrompt, claim.MCPServers, claim.Ephemeral, claim.AllowedTools = "", nil, false, nil
				if s, err := sp.Claim(ctx, claim); err == nil {
					return s, nil
				}
			}
			_ = sp.Close()
		}
	}
	return b.provider.Start(ctx, cfg)
}

// Warm parks a process with cfg's fixed settings (alpha in Claude Code),
// unless one is parked or starting; it costs memory, not tokens. Settings
// a claim always refuses park nothing.
func (b claudeBackend) Warm(ctx context.Context, cfg agent.Config) {
	if len(cfg.DisallowedTools) > 0 || len(cfg.UnsetEnv) > 0 || cfg.PermissionMode == agent.PermissionReadOnly {
		return
	}
	s := b.spare
	s.mu.Lock()
	if s.closed || s.warming || (s.sp != nil && !exited(s.sp)) {
		s.mu.Unlock()
		return
	}
	s.warming = true
	s.mu.Unlock()

	cfg.Resume, cfg.Fork, cfg.ForkAtTurn = "", false, ""
	sp, err := b.provider.Prewarm(ctx, cfg)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.warming = false
	if err != nil {
		return
	}
	if s.closed || ctx.Err() != nil || s.sp != nil {
		go sp.Close()
		return
	}
	s.sp, s.cwd = sp, cfg.Cwd
}

// Close discards the parked process and parks no more.
func (b claudeBackend) Close() {
	b.spare.mu.Lock()
	b.spare.closed = true
	sp := b.spare.sp
	b.spare.sp = nil
	b.spare.mu.Unlock()
	if sp != nil {
		_ = sp.Close()
	}
}

func (s *spare) take() (*claude.Spare, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, cwd := s.sp, s.cwd
	s.sp = nil
	return sp, cwd
}

// exited reports whether a parked process has ended.
func exited(sp *claude.Spare) bool {
	select {
	case <-sp.Done():
		return true
	default:
		return false
	}
}

func (claudeBackend) Name() string               { return "Claude" }
func (claudeBackend) Product() string            { return "Claude Code" }
func (claudeBackend) Command() string            { return "tclaude" }
func (b claudeBackend) Provider() agent.Provider { return b.provider }

// DefaultModel keeps a new chat cheap: Haiku with its standard context
// window. -model or /model picks another.
func (claudeBackend) DefaultModel() string { return "claude-haiku-5-5" }

// Defaults reads defaultMode and effortLevel from Claude Code's user,
// project and local settings, later files winning. Managed settings and
// environment variables can still override them once the session starts.
func (claudeBackend) Defaults(cwd string) (agent.PermissionMode, agent.Effort) {
	var mode agent.PermissionMode
	var effort agent.Effort
	files := []string{filepath.Join(cwd, ".claude", "settings.json"), filepath.Join(cwd, ".claude", "settings.local.json")}
	if home, err := os.UserHomeDir(); err == nil {
		files = append([]string{filepath.Join(home, ".claude", "settings.json")}, files...)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var s struct {
			Permissions struct {
				DefaultMode agent.PermissionMode `json:"defaultMode"`
			} `json:"permissions"`
			EffortLevel agent.Effort `json:"effortLevel"`
		}
		if json.Unmarshal(data, &s) != nil {
			continue
		}
		mode = cmp.Or(s.Permissions.DefaultMode, mode)
		effort = cmp.Or(s.EffortLevel, effort)
	}
	return mode, effort
}

func (claudeBackend) Prepare(cfg *agent.Config) {
	if cfg.Env == nil {
		cfg.Env = map[string]string{}
	}
	cfg.Env["CLAUDE_CODE_ENTRYPOINT"] = claudeEntrypoint
	cfg.ProviderOptions = &claude.SessionOptions{
		PromptSuggestions:       true, // ghost text for the next prompt
		EnableFileCheckpointing: true, // rewind can restore files
		TailShellOutput:         true, // Bash output streams while it runs
		AgentProgressSummaries:  true, // subagents report what they are doing
		TrackSubagentEffort:     true, // Claude Code tells only hooks the effort a subagent runs at
		IncludeHookEvents:       true, // hooks' progress and what they print
	}
}

// Flags are Claude Code's own: the agent to run as, a fallback model,
// turn and spending limits, and opting in to bypassing permissions.
func (claudeBackend) Flags(fs *flag.FlagSet) func(cfg *agent.Config) {
	agentName := fs.String("agent", "", "run as this subagent (see /agents)")
	fallback := fs.String("fallback-model", "", "model to use when the chosen one is overloaded")
	maxTurns := fs.Int("max-turns", 0, "stop a turn after this many model steps (0: no limit)")
	maxBudget := fs.Float64("max-budget", 0, "stop once the conversation has cost this many dollars (0: no limit)")
	bypass := fs.Bool("allow-bypass", false, "let the permission mode switch to bypassPermissions")
	// Codex refuses tool lists, so these are Claude's alone.
	allowed := fs.String("allowed-tools", "", "comma-separated tools allowed without asking, such as Bash(git:*),Read")
	disallowed := fs.String("disallowed-tools", "", "comma-separated tools Claude may not use")
	return func(cfg *agent.Config) {
		cfg.AllowedTools, cfg.DisallowedTools = splitList(*allowed), splitList(*disallowed)
		opts := cfg.ProviderOptions.(*claude.SessionOptions)
		opts.Agent, opts.FallbackModel, opts.MaxTurns, opts.MaxBudgetUSD = *agentName, *fallback, *maxTurns, *maxBudget
		opts.AllowBypassPermissions = *bypass || cfg.PermissionMode == agent.PermissionBypass
	}
}

func (claudeBackend) Efforts() []agent.Effort {
	return []agent.Effort{agent.EffortLow, agent.EffortMedium, agent.EffortHigh, agent.EffortXHigh, agent.EffortMax}
}

func (claudeBackend) Has(f feature) bool {
	switch f {
	case featContextBreakdown, featSideQuestion, featFileRewind, featPromptSuggestions,
		featThinkingBudget, featPlanFile, featExport, featAgents, featPermissionRules, featPlanUsage,
		featReload, featCd, featMCPAuth, featBackground:
		return true
	}
	return false
}

func (b claudeBackend) SessionExists(ctx context.Context, id, cwd string) error {
	_, err := b.provider.SessionInfo(ctx, id, cwd)
	return err
}

func (claudeBackend) FileSuggestions(ctx context.Context, s agent.Session, query, _ string) ([]string, error) {
	cs, ok := s.(*claude.Session)
	if !ok {
		return nil, agent.ErrUnsupported
	}
	return cs.FileSuggestions(ctx, query)
}

func (b claudeBackend) Models(ctx context.Context, s agent.Session) []agent.Model {
	cs, ok := s.(*claude.Session)
	if !ok {
		// No session yet: the provider asks the CLI.
		models, _ := b.provider.ListModels(ctx)
		return models
	}
	var out []agent.Model
	for _, m := range cs.SupportedModels() {
		var efforts []agent.Effort
		for _, e := range m.SupportedEffortLevels {
			efforts = append(efforts, agent.Effort(e))
		}
		out = append(out, agent.Model{ID: m.Value, DisplayName: m.DisplayName, Description: m.Description,
			ResolvedID: m.ResolvedModel, Efforts: efforts})
	}
	return out
}
