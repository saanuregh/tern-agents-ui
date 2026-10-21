// Package chat is a chat with a coding agent drawn natively by Tern. It
// speaks the Tern Surface Protocol on its terminal (an inline surface:
// transcript above, composer and status docked below) and drives the
// agent's CLI through agent-cli-connector.
package chat

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/claude"
	"github.com/stencil-hq/tern-sdk/go/tern"
)

// version is this program's version, reported to Tern and the agent.
const version = "0.3.0"

// command is the running program's name, for messages printed outside Tern.
var command = "tclaude"

// Main runs the chat with agent b; it parses the command line and exits on
// failure.
func Main(b backend) {
	command = b.Command()
	modelHelp := "model to use"
	if b.DefaultModel() == "" {
		modelHelp += " (default: your " + b.Name() + " settings)"
	}
	model := flag.String("model", b.DefaultModel(), modelHelp)
	mode := flag.String("mode", "", "permission mode: "+joinModes(b.Provider().Capabilities().PermissionModes)+" (default: your "+b.Name()+" settings)")
	effort := flag.String("effort", "", "reasoning effort, such as low or high (default: the model's)")
	resume := flag.String("resume", "", "session id to resume")
	cont := flag.Bool("continue", false, "resume the most recent conversation in the directory")
	cwd := flag.String("cwd", "", "working directory (default: the current one)")
	logFile := flag.String("log", "", "write diagnostics to this file")
	debug := flag.Bool("debug", false, "with -log, also log "+b.Product()+"'s own output, which can include conversation text")
	var addDirs listFlag
	flag.Var(&addDirs, "add-dir", "another folder the agent may use (repeat for more)")
	systemPrompt := flag.String("system-prompt", "", "replace the agent's system prompt")
	appendPrompt := flag.String("append-system-prompt", "", "add to the agent's system prompt")
	mcpConfig := flag.String("mcp-config", "", `JSON file of extra MCP servers: {"mcpServers": {"name": {"command": …} | {"url": …}}}`)
	noSave := flag.Bool("no-save", false, "don't store the conversation")
	title := flag.String("title", "", "name the new conversation")
	applyAgentFlags := b.Flags(flag.CommandLine)
	demo := flag.String("demo", "", "show a sample screen without starting "+b.Name()+": welcome, complete, context, chat, running, permission, question, ask, plan, rewind, config, picker or queue")
	flag.Parse()

	if err := checkFlags(b, *mode, *effort, *resume, *cont); err != nil {
		fail(err)
	}
	dir, err := workDir(*cwd)
	if err != nil {
		fail(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if *logFile != "" {
		f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			fail(err)
		}
		defer f.Close()
		level := slog.LevelInfo
		if *debug {
			level = slog.LevelDebug
		}
		logger = slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: level}))
	}
	cfg := agent.Config{
		Cwd:            dir,
		Model:          *model,
		PermissionMode: agent.PermissionMode(*mode),
		Effort:         agent.Effort(*effort),

		AdditionalDirectories: addDirs,
		SystemPrompt:          *systemPrompt,
		AppendSystemPrompt:    *appendPrompt,
		Ephemeral:             *noSave,
		Title:                 *title,
	}
	if *mcpConfig != "" {
		servers, err := readMCPConfig(*mcpConfig)
		if err != nil {
			fail(err)
		}
		cfg.MCPServers = servers
	}
	b.Prepare(&cfg)
	applyAgentFlags(&cfg)
	if opts, ok := cfg.ProviderOptions.(*claude.SessionOptions); ok && *debug && *logFile != "" {
		// Claude Code's own debug log, next to ours.
		opts.Debug, opts.DebugFile = true, *logFile+".claude"
	}

	ctx := context.Background()
	opts := tern.Options{App: b.Command(), Version: version,
		Features: []string{tern.FeatureEdit, tern.FeatureUndo, tern.FeatureSend}}
	// Our own output writer lets the activity signals share the SDK's
	// terminal; raw mode and signals are then ours to handle.
	var tty *ttyOut
	if isTerminal() {
		tty = &ttyOut{f: os.Stdout}
		opts.Out, opts.MakeRaw = tty, rawTerminal
	}
	s, err := tern.Connect(ctx, opts)
	if errors.Is(err, tern.ErrUnsupported) {
		if *demo != "" {
			fail(errors.New("-demo needs Tern"))
		}
		a := newApp(ctx, b, cfg, logger)
		cfg.Resume = *resume
		if *cont {
			cfg.Resume = newestSession(ctx, a, dir)
		}
		plain(ctx, a, cfg, dir)
		return
	}
	if err != nil {
		fail(err)
	}
	if tty != nil {
		closeOnSignal(func() {
			tty.clear()
			_ = s.Close()
		})
	}
	a := newApp(ctx, b, cfg, logger)
	a.session, a.tty = s, tty
	sf, err := s.Open(tern.SurfaceOptions{Mode: tern.Inline, Title: b.Name() + " · " + filepath.Base(dir), Role: "agent.chat"})
	if err != nil {
		_ = s.Close()
		fail(err)
	}
	a.surface = sf
	_ = sf.Stylesheet("claude", stylesheet)
	go a.renderLoop()

	if *demo != "" {
		a.demo = true
		a.mu.Lock()
		a.loadDemoLocked(*demo)
		a.mu.Unlock()
		a.changed()
		err = s.Run(a.ctx, func(in tern.Input) error {
			switch in := in.(type) {
			case tern.Key:
				return a.key(in)
			case *tern.ErrorEvent:
				logger.Warn("tern error", "msg", in.Msg, "id", in.ID, "sheet", in.Sheet)
			}
			a.changed()
			return nil
		})
		a.quit()
		b.Close()
		if tty != nil {
			tty.clear()
		}
		_ = s.Close()
		return
	}
	go a.contextLoop()
	start := cfg
	start.Resume = *resume
	if *cont && start.Resume == "" {
		start.Resume = newestSession(ctx, a, dir)
	}
	a.start(start)

	err = s.Run(a.ctx, func(in tern.Input) error {
		switch in := in.(type) {
		case tern.Key:
			return a.key(in)
		case *tern.Resize, *tern.Theme:
			a.changed()
		case *tern.ErrorEvent:
			// Tern rejected part of a frame or the stylesheet.
			logger.Warn("tern error", "msg", in.Msg, "id", in.ID, "sheet", in.Sheet)
		}
		return nil
	})
	a.mu.Lock()
	id, sess, sent := a.info.ID, a.sess, a.sent
	a.mu.Unlock()
	a.quit()
	b.Close()
	if tty != nil {
		tty.clear()
	}
	_ = s.Close()
	if sess != nil {
		_ = sess.Close()
	}
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
		fail(err)
	}
	if id != "" && sent {
		fmt.Println(resumeHint(id, dir))
	}
}

// checkFlags refuses values the claude CLI would only reject after starting.
func checkFlags(b backend, mode, effort, resume string, cont bool) error {
	modes := b.Provider().Capabilities().PermissionModes
	if mode != "" && !slices.Contains(modes, agent.PermissionMode(mode)) {
		return fmt.Errorf("-mode %q: use one of %s", mode, joinModes(modes))
	}
	efforts := b.Efforts()
	if effort != "" && !slices.Contains(efforts, agent.Effort(effort)) {
		return fmt.Errorf("-effort %q: use one of %v", effort, efforts)
	}
	if resume != "" && cont {
		return errors.New("-resume and -continue exclude each other")
	}
	return nil
}

// listFlag is a flag that may be given more than once.
type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

// splitList reads a comma-separated flag.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// readMCPConfig reads MCP servers in the format Claude Code's .mcp.json
// uses: {"mcpServers": {"name": {"command", "args", "env"} or {"type",
// "url", "headers"}}}.
func readMCPConfig(path string) (map[string]agent.MCPServer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var file struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("-mcp-config %s: %w", path, err)
	}
	out := map[string]agent.MCPServer{}
	for name, s := range file.MCPServers {
		if s.Command == "" && s.URL == "" {
			return nil, fmt.Errorf("-mcp-config %s: server %q has neither command nor url", path, name)
		}
		out[name] = agent.MCPServer{Transport: agent.MCPTransport(s.Type), Command: s.Command, Args: s.Args, Env: s.Env, URL: s.URL, Headers: s.Headers}
	}
	return out, nil
}

// joinModes lists permission modes for help and errors.
func joinModes(modes []agent.PermissionMode) string {
	names := make([]string, len(modes))
	for i, m := range modes {
		names[i] = string(m)
	}
	return strings.Join(names, ", ")
}

// workDir is dir, or the current directory, made absolute and checked.
func workDir(dir string) (string, error) {
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		dir = wd
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if st, err := os.Stat(dir); err != nil {
		return "", err
	} else if !st.IsDir() {
		return "", fmt.Errorf("-cwd %s is not a directory", dir)
	}
	return dir, nil
}

// resumeHint is the command that resumes id. Conversations are stored per
// directory, so it names the directory when the shell is elsewhere.
func resumeHint(id, dir string) string {
	if wd, err := os.Getwd(); err == nil && wd == dir {
		return "Resume this conversation with: " + command + " -resume " + id
	}
	return fmt.Sprintf("Resume this conversation with: %s -cwd %q -resume %s", command, dir, id)
}

// newestSession is the most recent stored conversation in dir, or "".
func newestSession(ctx context.Context, a *app, dir string) string {
	lister, ok := a.backend.Provider().(agent.SessionLister)
	if !ok {
		return ""
	}
	sessions, err := lister.ListSessions(ctx, agent.ListSessionsOptions{Cwd: dir, Limit: 1})
	if err != nil || len(sessions) == 0 {
		return ""
	}
	return sessions[0].ID
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, command+":", err)
	os.Exit(1)
}

// plain chats line by line outside Tern: replies stream as text, and tool
// calls that need permission, questions and forms are refused.
func plain(ctx context.Context, a *app, cfg agent.Config, dir string) {
	cfg.Handlers.Permission = func(context.Context, *agent.PermissionRequest) agent.PermissionResponse {
		return agent.PermissionResponse{Decision: agent.DecisionDeny, Message: "Permissions can only be granted inside Tern."}
	}
	cfg.Handlers.UserInput = func(context.Context, *agent.UserInputRequest) agent.UserInputResponse {
		return agent.UserInputResponse{Cancelled: true}
	}
	cfg.Handlers.Elicitation = func(context.Context, *agent.ElicitationRequest) agent.ElicitationResponse {
		return agent.ElicitationResponse{Action: agent.ElicitationDecline}
	}
	// Turns are read through their own streams; the session's is unread.
	cfg.EventBufferLimit = 1
	cfg.Logger = a.logger
	sess, err := a.backend.Provider().Start(ctx, cfg)
	if err != nil {
		fail(err)
	}
	defer sess.Close()
	fmt.Fprintln(os.Stderr, "Not running in Tern: plain mode. Tool calls that need permission are refused.")
	in := bufio.NewScanner(os.Stdin)
	sent := false
	for fmt.Print("> "); in.Scan(); fmt.Print("> ") {
		text := strings.TrimSpace(in.Text())
		if text == "" {
			continue
		}
		turn, err := sess.Send(ctx, agent.Text(text))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			continue
		}
		sent = true
		for ev := range turn.Events() {
			if d, ok := ev.(*agent.TextDelta); ok && d.SubagentID == "" {
				fmt.Print(d.Delta)
			}
		}
		fmt.Println()
	}
	if sent || cfg.Resume != "" {
		fmt.Println(resumeHint(sess.ID(), dir))
	}
}

// stylesheet adds what Tern's chat skin doesn't draw: buttons, the welcome
// starters, request cards and local command cards. Tern scopes it to this
// surface.
const stylesheet = `
[data-role='agent.starters'] { max-width: 640px; margin: 18px auto 14px; justify-content: center; }
[data-role='agent.starters'] [data-role='omp.btn'] { padding: 6px 14px; font: 13px/1.3 var(--sans); }
[data-role='agent.where'] { margin-top: 6px; text-align: center; }

.sf-card[data-role='agent.request'],
.sf-card[data-role='agent.local'] {
	font-family: var(--sans);
}
.sf-card[data-role='agent.request'] :is(.sf-code, .sf-diff, .sf-text.mono) { font-family: var(--mono); }
/* Dock rows of ours sit in the composer's column, like the working line.
   Tern scopes this sheet to the surface's regions and matches only their
   descendants, so rules name roles, never the region. */
.sf-row:is([data-role='omp.working'], [data-role='agent.task']),
.sf-text[data-role='agent.more'] {
	width: calc(100% - 24px);
	max-width: 736px;
	margin: 0 auto !important;
	padding: 0 4px !important;
	box-sizing: border-box;
}
/* Room for the context bar's labels under the working line. */
.sf-row[data-role='omp.working'] { margin-bottom: 10px !important; }
/* Nothing is wider than its column: a task row's parts shrink and clip,
   and long unbroken text breaks inside cards. */
.sf-row[data-role='agent.task'] > * { min-width: 0; }
.sf-row[data-role='agent.task'] > :first-child { overflow: hidden; }
.sf-row[data-role='agent.task'] > :last-child { flex: none; }
:is(.sf-card, .sf-overlay)[data-role^='agent.'] { min-width: 0; overflow-wrap: anywhere; }
/* The background-jobs pill: small, at the right of the column, like the
   todo pill. */
.sf-row[data-role='agent.jobs'] {
	width: max-content;
	max-width: calc(100% - 24px);
	margin: 0 12px 0 auto !important;
	padding: 3px 10px;
	border-radius: 999px;
	background: var(--l2);
	box-shadow: inset 0 0 0 1px color-mix(in srgb, var(--t1) 12%, transparent);
	font: 12.5px/1.3 var(--mono);
	cursor: pointer;
}
/* A long command's "N more lines" toggle, under its clamped script. */
.sf-row[data-role='agent.more-lines'] { padding: 2px 16px 4px; font: 12.5px/1.4 var(--sans); cursor: pointer; }
.sf-row[data-role='agent.more-lines']:hover .sf-text { color: var(--t1); }
/* An agent row's name gives way first, then its task; the badges and
   stats keep their size. */
.sf-agent-row { min-width: 0; }
.sf-agent-row > .sf-agent-name { min-width: 0; flex: 0 1 auto; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.sf-agent-row > :is(.sf-agent-g, .sf-badge, .sf-agent-stats) { flex: none; }
.sf-card[data-role='agent.request'] {
	width: calc(100% - 24px);
	max-width: 736px;
	margin: 0 auto 8px;
	border-radius: 14px;
	box-shadow: inset 0 0 0 1px color-mix(in srgb, var(--tc) 35%, transparent), 0 10px 30px -18px rgba(0, 0, 0, 0.35);
	background: color-mix(in srgb, var(--tc) 5%, var(--card));
}
.sf-card[data-role='agent.local'] {
	border-radius: 12px;
}
.sf-card:is([data-role='agent.request'], [data-role='agent.local']) > .sf-card-body {
	padding: 4px 16px 14px;
	gap: 10px;
}
.sf-card:is([data-role='agent.request'], [data-role='agent.local']) > .sf-card-head {
	padding: 12px 16px 2px;
	font: 600 13.5px/1.3 var(--sans);
}


.sf-overlay[data-role='agent.context'] { font-family: var(--sans); }
.sf-overlay[data-role='agent.context'] .sf-ov-body { gap: 12px; padding: 4px 16px 14px; }
[data-role='agent.context.summary'] { font-size: 15px; }
[data-role='agent.context.row'] { min-height: 22px; }
:is([data-role='agent.context.tokens'], [data-role='agent.context.pct']) {
	font-family: var(--mono);
	font-variant-numeric: tabular-nums;
	text-align: right;
}
[data-role='agent.context.tokens'] { min-width: 52px; color: var(--t2); }
[data-role='agent.context.pct'] { min-width: 50px; }
.sf-md ol, .sf-md ul { padding-left: 24px; }
.sf-md .sf-t-path { font-family: var(--mono); font-size: 0.92em; }
`
