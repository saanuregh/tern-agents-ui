package chat

import (
	"encoding/json"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
)

// loadDemoLocked fills the screen with a sample conversation, without
// starting Claude, to look at the UI (-demo welcome|complete|chat|running|permission|question).
func (a *app) loadDemoLocked(scene string) {
	a.status = "idle"
	a.model, a.effort, a.mode = "claude-haiku-5-5", agent.EffortMedium, agent.PermissionAuto
	a.info.Version = "2.1.306"
	now := time.Now()
	if scene == "welcome" {
		return
	}
	if scene == "context" {
		defer func() {
			var u contextUsage
			_ = json.Unmarshal([]byte(demoContext), &u)
			a.ctxUsage, a.ctxAt, a.showContext = &u, now, true
			a.contextTokens, a.contextWindow = u.TotalTokens, u.MaxTokens
		}()
	}
	if scene == "paths" {
		a.apply(&agent.ItemCompleted{Item: agent.Item{ID: "p1", Kind: agent.ItemAssistantMessage, Status: agent.ItemDone, Text: "dir code `" + a.cwd + "`\n\n" +
			"dir slash `" + a.cwd + "/`\n\nfile code `" + a.cwd + "/README.md`\n\nrel code `README.md`\n\nrel dir `src/ui`\n\n" +
			"prose " + a.cwd + "/README.md and src/main.go:12 and ./notes.md\n\ntilde `~/.claude/settings.json`"}})
		return
	}
	if scene == "complete" {
		a.composer.insert("/")
		a.refreshCompletionLocked(false)
		return
	}
	exit0 := 0
	events := []agent.Event{
		&agent.ItemCompleted{Item: agent.Item{ID: "u1", Kind: agent.ItemUserMessage, Status: agent.ItemDone, Time: now,
			Text: "The config loader ignores `TERN_THEME`. Can you find out why and fix it?"}},
		&agent.ItemCompleted{Item: agent.Item{ID: "r1", Kind: agent.ItemReasoning, Status: agent.ItemDone,
			Text: "The loader probably reads the environment before the file and then overwrites it. Let me look at `config.go`."}},
		&agent.ItemCompleted{Item: agent.Item{ID: "t1", Kind: agent.ItemToolCall, Status: agent.ItemDone, Tool: &agent.ToolCall{
			Name: "Grep", Kind: agent.ToolSearch, Query: "TERN_THEME", Output: "README.md\nmain.go", Duration: 220 * time.Millisecond}}},
		&agent.ItemCompleted{Item: agent.Item{ID: "t2", Kind: agent.ItemToolCall, Status: agent.ItemDone, Tool: &agent.ToolCall{
			Name: "Read", Kind: agent.ToolRead, Path: a.cwd + "/config.go", Output: "package config\n\nfunc Load() {}\n", Duration: 90 * time.Millisecond}}},
		&agent.ItemCompleted{Item: agent.Item{ID: "t2b", Kind: agent.ItemToolCall, Status: agent.ItemDone, Tool: &agent.ToolCall{
			Name: "Read", Kind: agent.ToolRead, Path: a.cwd + "/main.go", Output: "package main\n\nfunc main() {}\n", Duration: 40 * time.Millisecond}}},
		&agent.ItemCompleted{Item: agent.Item{ID: "t2c", Kind: agent.ItemToolCall, Status: agent.ItemDone, Tool: &agent.ToolCall{
			Name: "Read", Kind: agent.ToolRead, Path: a.cwd + "/README.md", Output: "# tern-agents-ui\n", Duration: 30 * time.Millisecond}}},
		&agent.ItemCompleted{Item: agent.Item{ID: "td", Kind: agent.ItemToolCall, Status: agent.ItemDone, Tool: &agent.ToolCall{Name: "TodoWrite", Kind: agent.ToolTodo}}},
		&agent.PlanUpdated{ItemID: "td", Steps: []agent.PlanStep{{Text: "Find why TERN_THEME is ignored", Status: agent.StepCompleted},
			{Text: "Swap the override order", Status: agent.StepInProgress}, {Text: "Add a test", Status: agent.StepPending}}},
		&agent.ItemCompleted{Item: agent.Item{ID: "a1", Kind: agent.ItemAssistantMessage, Status: agent.ItemDone,
			Text: "Found it: `Load` applies environment overrides **before** reading `config.toml`, so the file wins.\n\nI'll swap the order and add a test."}},
		&agent.ItemCompleted{Item: agent.Item{ID: "t3", Kind: agent.ItemToolCall, Status: agent.ItemDone, Tool: &agent.ToolCall{
			Name: "Edit", Kind: agent.ToolEdit, Path: a.cwd + "/config.go", Duration: 640 * time.Millisecond,
			Input: json.RawMessage(`{"old_string":"\tapplyEnv(cfg)\n\treadFile(cfg)","new_string":"\treadFile(cfg)\n\tapplyEnv(cfg) // the environment wins"}`)}}},
		&agent.ItemCompleted{Item: agent.Item{ID: "t3b", Kind: agent.ItemToolCall, Status: agent.ItemDone, Tool: &agent.ToolCall{
			Name: "Bash", Kind: agent.ToolShell, ExitCode: &exit0, Duration: 120 * time.Millisecond,
			Command: "python3 - <<'PY'\nimport os, tomllib\nwith open('config.toml', 'rb') as f:\n    cfg = tomllib.load(f)\nprint('file theme:', cfg.get('theme'))\nprint('env theme:', os.environ.get('TERN_THEME'))\nprint('wins:', os.environ.get('TERN_THEME') or cfg.get('theme'))\nPY",
			Input:   json.RawMessage(`{"description":"Compare the file and environment themes"}`),
			Output:  "file theme: dark\nenv theme: gruvbox\nwins: gruvbox"}}},
		&agent.ItemCompleted{Item: agent.Item{ID: "t4", Kind: agent.ItemToolCall, Status: agent.ItemDone, Tool: &agent.ToolCall{
			Name: "Bash", Kind: agent.ToolShell, Command: "go test ./...", ExitCode: &exit0, Duration: 3100 * time.Millisecond,
			Output: "ok  \tconfig\t0.412s\nok  \tcmd/tern\t1.022s"}}},
		&agent.ItemCompleted{Item: agent.Item{ID: "t5", Kind: agent.ItemToolCall, Status: agent.ItemDone, Tool: &agent.ToolCall{
			Name: "Agent", Kind: agent.ToolSubagent, Input: json.RawMessage(`{"description":"Check other env overrides"}`),
			Output: "No other override is applied before the file."}}},
		&agent.TaskUpdated{Task: agent.Task{ID: "task1", Kind: agent.TaskSubagent, Status: agent.TaskCompleted, ToolCallID: "t5",
			Description: "Check other env overrides", ToolUses: 4, Duration: 8 * time.Second, Output: "No other override is applied before the file.",
			Subagent: &agent.Subagent{Type: "Explore", Model: "haiku", Effort: agent.EffortLow}, Usage: &agent.Usage{TotalTokens: 18200, CostUSD: 0.01}}},
		&agent.ItemCompleted{Item: agent.Item{ID: "a2", Kind: agent.ItemAssistantMessage, Status: agent.ItemDone,
			Text: "Fixed. `TERN_THEME` now overrides the file:\n\n| Source | Before | After |\n| --- | --- | --- |\n| `config.toml` | wins | loses |\n| `TERN_THEME` | ignored | **wins** |\n\nTests pass."}},
		&agent.ItemCompleted{Item: agent.Item{ID: "u2", Kind: agent.ItemUserMessage, Status: agent.ItemDone, Time: now,
			Text: "Great. Commit it and run the linter."}},
		&agent.PlanUpdated{Steps: []agent.PlanStep{{Text: "Fix the override order", Status: agent.StepCompleted},
			{Text: "Run the linter", Status: agent.StepInProgress}, {Text: "Commit", Status: agent.StepPending}}},
		&agent.UsageUpdated{Usage: agent.Usage{ContextTokens: 64000, ContextWindow: 200000}},
	}
	for _, ev := range events {
		a.apply(ev)
	}
	a.costUSD = 0.04
	a.turnUsage["u1"] = turnUsage{duration: 41 * time.Second, input: 38200, output: 2100, cost: 0.03}
	a.thinking["r1"] = thinkSpan{start: now.Add(-50 * time.Second), end: now.Add(-46 * time.Second)}
	a.upsertLocked(a.entries["u1"].item)
	a.addLocal(hookCard("SessionStart", "SessionStart:startup", "# Shared agent memory\n\nPreferences recorded with sr-remember.\n\n- Use mature libraries where they fit."))
	a.addLocal(localCard{Title: "Side question: what does applyEnv do?", Text: "It copies `TERN_*` variables over the loaded settings.", Tone: "info"})
	switch scene {
	case "running", "permission", "question", "ask", "plan", "queue":
		a.status, a.turnStart = "running", now.Add(-12*time.Second)
		a.goal = &agent.Goal{Objective: "LOAD: Improve dashboard load times"}
		// A long-named background command, to show the dock row stays in the column.
		a.apply(&agent.TaskUpdated{Task: agent.Task{ID: "bg1", Kind: agent.TaskShell, Status: agent.TaskRunning,
			Description: "Count 1 to 60 with one-second pauses and time it, then report the elapsed seconds and any drift from wall-clock time"}})
		a.apply(&agent.ItemStarted{Item: agent.Item{ID: "t6", Kind: agent.ItemToolCall, Status: agent.ItemRunning, Tool: &agent.ToolCall{
			Name: "Bash", Kind: agent.ToolShell, Command: "golangci-lint run ./...", Output: "config.go:44:2: unused variable 'tmp'\n"}}})
	}
	switch scene {
	case "ask":
		a.apply(&agent.UserInputRequest{ID: "q2", Questions: []agent.Question{
			{ID: "style", Header: "Style", Text: "Which **commit message** style should I use?", AllowOther: true, Options: []agent.QuestionOption{
				{Label: "Conventional", Description: "type(scope): summary", Preview: "```\nconfig: let TERN_THEME override the file\n```"},
				{Label: "Plain sentence", Description: "One short sentence", Preview: "```\nLet TERN_THEME override the config file\n```"}}},
			{ID: "push", Header: "Push", Text: "Push after committing?", Options: []agent.QuestionOption{{Label: "Yes"}, {Label: "No"}}},
		}})
	case "plan":
		a.mode = agent.PermissionPlan
		a.apply(&agent.PermissionRequest{ID: "p2", Kind: agent.PermissionPlanApproval, ToolName: "ExitPlanMode",
			Plan: "# Plan: lint and commit\n\n## Steps\n1. Run `golangci-lint run ./...` and fix the unused variable in `config.go`.\n2. Run the tests.\n3. Commit with a conventional message.\n\n## Risks\n- None: the change is local to `config.go`."})
	case "rewind":
		a.status = "idle"
		a.startRewindLocked("u1")
	case "config":
		a.status = "idle"
		a.cmdConfig("")
	case "picker":
		a.status = "idle"
		a.cmdMode("")
	case "queue":
		a.apply(&agent.ItemCompleted{Item: agent.Item{ID: "u3", Kind: agent.ItemUserMessage, Status: agent.ItemDone, Text: "Then push it to origin."}})
		a.queued["u3"] = true
		a.apply(&agent.ItemCompleted{Item: agent.Item{ID: "u4", Kind: agent.ItemUserMessage, Status: agent.ItemDone, Text: "And open a PR with a short description."}})
		a.queued["u4"] = true
	case "permission":
		a.apply(&agent.PermissionRequest{ID: "p1", Kind: agent.PermissionCommand, ToolName: "Bash",
			Command: "git commit -am 'config: let TERN_THEME override the file'", Description: "Commit the fix",
			Decisions: []agent.PermissionDecision{agent.DecisionAllow, agent.DecisionAllowSession, agent.DecisionDeny}})
	case "question":
		a.apply(&agent.UserInputRequest{ID: "q1", Questions: []agent.Question{{ID: "msg", Header: "Commit",
			Text:    "Which commit message style should I use?",
			Options: []agent.QuestionOption{{Label: "Conventional", Description: "config: …"}, {Label: "Plain sentence"}, {Label: "Let me type it"}}}}})
	}
}

// demoContext is a context breakdown in the shape Claude Code reports.
const demoContext = `{"totalTokens":112400,"maxTokens":200000,"percentage":56,"model":"claude-haiku-5-5",
"autoCompactThreshold":167000,"isAutoCompactEnabled":true,
"categories":[{"id":"system_prompt","name":"System prompt","tokens":3536,"kind":"used"},
{"id":"system_tools","name":"System tools","tokens":28956,"kind":"used"},
{"id":"custom_agents","name":"Custom agents","tokens":1503,"kind":"used"},
{"id":"memory_files","name":"Memory files","tokens":3968,"kind":"used"},
{"id":"skills","name":"Skills","tokens":6762,"kind":"used"},
{"id":"messages","name":"Messages","tokens":67675,"kind":"used"},
{"id":"autocompact_buffer","name":"Autocompact buffer","tokens":33000,"kind":"buffer"},
{"id":"free_space","name":"Free space","tokens":54600,"kind":"free"}],
"memoryFiles":[{"path":"/home/me/.claude/CLAUDE.md","type":"User","tokens":3968}],
"agents":[{"agentType":"code-reviewer","source":"user","tokens":820},{"agentType":"explore","source":"plugin","tokens":683}],
"skills":{"totalSkills":3,"tokens":6762,"skillFrontmatter":[{"name":"release","source":"project","tokens":2100},{"name":"debug","source":"user","tokens":2662},{"name":"review","source":"user","tokens":2000}]},
"messageBreakdown":{"toolCallTokens":9200,"toolResultTokens":41300,"attachmentTokens":621,"assistantMessageTokens":11040,"userMessageTokens":5514}}`
