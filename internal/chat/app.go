package chat

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/claude"
	"github.com/saanuregh/agent-cli-connector/codex"
	"github.com/stencil-hq/tern-sdk/go/tern"
)

// historyLimit bounds the stored items shown when a session is resumed.
const historyLimit = 400

// app is the whole program state. mu guards every field below it: node
// handlers run on the input goroutine, Claude's events on another, and the
// renderer on a third.
type app struct {
	lastOutcome     activity             // how the last turn ended, for Tern's status: idle, done or error
	tty             *ttyOut              // the terminal, for Tern's activity signals; nil when the SDK owns it
	renaming        *renameTarget        // the stored conversation the composer names, from /resume
	summaries       map[string]bool      // reasoning items that are summaries
	toolStart       map[string]time.Time // when running tools started, from progress reports
	stdin           map[string]string    // what was typed into a running command (Codex)
	hookRunning     string               // the hook running now, for the working line
	resumedAt       int                  // where history ends in the order, for the "Resumed" line; -1 when none
	branch          string               // the folder's git branch, read at branchAt
	branchAt        time.Time
	openScripts     map[string]bool              // commands whose whole script is shown
	fromTern        bool                         // the draft being submitted came from Tern, not the keyboard
	queuedTurns     map[string]*agent.Turn       // stand-ins for queued messages the agent announces only when they run
	turnDiff        string                       // Codex: what the current or last turn changed
	otherLimits     map[string][]agent.RateLimit // Codex: allowances besides the main one, by id
	otherLimitOrder []string                     // their ids, in the order they first came
	account         accountInfo                  // Codex: the signed-in account
	readGroups      map[string]cachedNode        // built "Read N files" rows by their first read
	demo            bool                         // a -demo screen: nothing starts Claude
	sent            bool                         // a message was sent, so the conversation is stored
	turnSeen        map[string]bool              // turns that started or ended, so a late send can't mark them queued
	countedResult   *byte                        // the result already counted: a steered turn reports it once per message
	pending         pendingChoices               // model, effort and mode chosen before the session attached
	deferred        *agent.Config                // a new conversation, started by its first message
	firstSend       func(started bool)           // sends that first message once the session attached, or gives it back
	ctx             context.Context
	quit            context.CancelFunc
	backend         backend       // the agent: Claude or Codex
	models          []agent.Model // what /model offers, fetched when a session attaches
	logger          *slog.Logger
	session         *tern.Session
	surface         *tern.Surface
	redraw          chan struct{}

	mu   sync.Mutex
	base agent.Config // settings every new session starts from
	cwd  string

	// The live Claude session. gen changes whenever it is replaced, so
	// events and replies from an old session are dropped.
	sess   agent.Session
	gen    int
	status string // starting, idle, running, compacting, waiting, closed, error
	err    string
	info   agent.SessionInfo

	transcript *agent.Transcript
	entries    map[string]*entry
	order      []string
	subOrder   map[string][]string // subagent task id → its item ids, in order
	localSeq   int
	tasks      map[string]agent.Task // by task id
	plan       []agent.PlanStep
	requests   []agent.Request
	queued     map[string]bool // turn ids sent while a turn ran
	rateLimits []agent.RateLimit
	goal       *agent.Goal

	model         string
	mode          agent.PermissionMode
	effort        agent.Effort
	costUSD       float64
	contextTokens int64
	contextWindow int64

	composer  composer
	complete  completion
	menu      *menu
	expandAll bool // Ctrl+O: every tool call and thought unfolded
	showPlan  bool // Ctrl+T: the full todo list instead of the pill

	ctxUsage    *contextUsage // the latest context breakdown
	ctxAt       time.Time
	ctxBusy     bool
	showContext bool // the live context viewer is open

	attachments     []attachment            // files going with the next message
	sentAttachments map[string][]attachment // by the message's client id
	blobs           map[string]string       // image path → blob id sent to Tern
	pathCache       map[string]string       // path text → absolute path, "" when missing

	turnUsage      map[string]turnUsage   // by the turn's user message id
	turnFails      map[string]turnFailure // by the turn's user message id
	thinking       map[string]thinkSpan   // reasoning item id → when it ran
	retry          *agent.Retry           // the retry being waited out, if any
	retryAt        time.Time
	planCursor     int                         // the highlighted plan-approval choice
	planAt         map[string][]agent.PlanStep // todo tool call id → the list it set
	rewind         *rewindState                // the in-place rewind, while choosing
	revealRewind   bool                        // scroll the rewind line into view next frame
	suggestion     string                      // Claude's predicted next prompt
	background     []claude.BackgroundTask
	toolSummary    map[string]string // tool call id → Claude's one-line summary
	prefs          *prefsState       // the /config sheet, when open
	thinkingBudget string
	suggestPaused  bool
	planFeedback   string                  // a plan request being answered with feedback
	compaction     *agent.Compaction       // the latest finished compaction
	answers        map[string]*answerDraft // drafts of question and form answers, by request id
	toasts         []toast
	toastSeq       int
	quitting       time.Time // first Ctrl+C on an empty, idle composer
	turnStart      time.Time // when Claude last started working, for the timer
	lastEsc        time.Time
}

// turnUsage is what one turn cost, for the footer under it.
type turnUsage struct {
	duration      time.Duration
	input, output int64
	cost          float64
	stopped       string // why a finished turn stopped early, such as the turn limit
}

// stopReasons explain the native reasons a successful turn can end early.
var stopReasons = map[string]string{
	"max_turns":     "stopped at the turn limit",
	"max_tokens":    "stopped at the output limit",
	"refusal":       "%s declined",
	"tool_deferred": "a hook deferred a tool",
}

// turnFailure is a turn that ended in an error or an interrupt.
type turnFailure struct {
	message  string
	code     string
	aborted  bool
	resetsAt time.Time // when an exhausted usage limit resets
}

type thinkSpan struct{ start, end time.Time }

// entry is one transcript row: a Claude item or a local card (a command's
// output). ver changes with every update so its node is rebuilt.
type entry struct {
	item     agent.Item
	local    *localCard
	ver      int
	node     tern.Element
	built    int
	expanded bool
}

// localCard is the output of a slash command, shown in the transcript but
// never sent to Claude.
type localCard struct {
	Title     string
	Meta      string // muted, after the title
	Text      string // Markdown, or plain text with Plain
	Plain     bool   // Text is program output: wrapped, not Markdown
	Tone      string
	Collapsed bool // starts folded to its head
	Choices   []choice
}

// choice is one row of a picker or a card's button.
type choice struct {
	Label   string
	Detail  string
	Run     func()
	Icon    string
	Current bool           // marked with a check
	Facts   map[string]any // values for the picker's columns
	Badges  []string
	Preview string // Markdown shown beside the list while selected
	Tone    string // a status dot
	Mono    bool
}

type toast struct {
	id    int
	level string
	text  string
}

func newApp(ctx context.Context, b backend, cfg agent.Config, logger *slog.Logger) *app {
	ctx, quit := context.WithCancel(ctx)
	a := &app{
		ctx:     ctx,
		quit:    quit,
		logger:  logger,
		backend: b,
		redraw:  make(chan struct{}, 1),
		base:    cfg,
		cwd:     cfg.Cwd,
		model:   cfg.Model,
		mode:    cfg.PermissionMode,
		effort:  cfg.Effort,
		answers: map[string]*answerDraft{},

		sentAttachments: map[string][]attachment{},
		blobs:           map[string]string{},
		pathCache:       map[string]string{},
	}
	a.resetLocked()
	return a
}

func (a *app) resetLocked() {
	a.deferred, a.firstSend = nil, nil
	a.transcript = &agent.Transcript{}
	a.entries = map[string]*entry{}
	a.order = nil
	a.subOrder = map[string][]string{}
	a.tasks = map[string]agent.Task{}
	a.plan = nil
	a.requests = nil
	a.queued = map[string]bool{}
	a.rateLimits = nil
	a.goal = nil
	a.costUSD, a.contextTokens, a.contextWindow = 0, 0, 0
	a.info = agent.SessionInfo{}
	a.menu = nil
	a.turnUsage, a.turnFails, a.thinking = map[string]turnUsage{}, map[string]turnFailure{}, map[string]thinkSpan{}
	a.retry, a.compaction = nil, nil
	a.planAt = map[string][]agent.PlanStep{}
	a.toolSummary = map[string]string{}
	a.suggestion, a.background = "", nil
	a.ctxUsage, a.ctxAt, a.showContext, a.ctxBusy = nil, time.Time{}, false, false
	a.answers, a.planFeedback, a.rewind = map[string]*answerDraft{}, "", nil
	a.turnSeen, a.countedResult = map[string]bool{}, nil
	a.lastOutcome = activity{state: "idle"}
	a.readGroups = map[string]cachedNode{}
	a.queuedTurns = map[string]*agent.Turn{}
	a.openScripts = map[string]bool{}
	a.summaries, a.toolStart, a.stdin, a.hookRunning, a.resumedAt = map[string]bool{}, map[string]time.Time{}, map[string]string{}, "", -1
	a.turnDiff, a.otherLimits, a.otherLimitOrder, a.account = "", map[string][]agent.RateLimit{}, nil, accountInfo{}
}

// accountInfo is who the agent is signed in as: label is the email and
// organization, or the kind of account; known is false until it is read.
type accountInfo struct {
	label, plan, auth string
	known             bool
}

// accountLocked is the signed-in account, from whichever agent runs.
func (a *app) accountLocked() accountInfo {
	if cs, err := a.claudeSession(); err == nil {
		acct := cs.Account()
		return accountInfo{label: joinNonEmpty(" · ", acct.Email, acct.Organization), plan: acct.SubscriptionType,
			auth: firstNonEmpty(acct.TokenSource, acct.APIKeySource, acct.APIProvider), known: true}
	}
	return a.account
}

// changed asks the renderer for a frame; renders coalesce.
func (a *app) changed() {
	select {
	case a.redraw <- struct{}{}:
	default:
	}
}

// renderLoop draws at most one frame per 16 ms, so a token stream costs
// a handful of frames per second rather than one per delta.
func (a *app) renderLoop() {
	focused := false
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-a.redraw:
		}
		a.mu.Lock()
		view := a.view()
		act, name := a.activityLocked(), a.backend.Command()
		// A question or the rewind bar replaces the editor; focus it again
		// when it returns.
		editor := a.rewind == nil && a.askRequestLocked() == nil
		a.mu.Unlock()
		if !editor {
			focused = false
		}
		if a.tty != nil {
			a.tty.report(name, act)
		}
		if err := a.surface.Render(view); err != nil {
			a.logger.Warn("render", "err", err)
		} else if a.takeReveal() {
			_ = a.surface.Reveal("main.rewind-here", "start")
		} else if editor && !focused && len(a.redraw) == 0 {
			// With a frame pending the editor may be gone before Tern
			// applies the focus; wait for the view to settle.
			focused = a.surface.Focus("dock.composer.input") == nil
		}
		time.Sleep(16 * time.Millisecond)
	}
}

// async runs fn off the input and event goroutines, so a slow Claude call
// never stalls typing. A failure is shown as a toast.
func (a *app) async(what string, fn func(ctx context.Context) error) {
	go func() {
		ctx, cancel := context.WithTimeout(a.ctx, 2*time.Minute)
		defer cancel()
		if err := fn(ctx); err != nil && !errors.Is(err, context.Canceled) {
			a.mu.Lock()
			a.toastLocked("error", what+": "+err.Error())
			a.mu.Unlock()
		}
	}()
}

func (a *app) toastLocked(level, text string) {
	a.toastSeq++
	id := a.toastSeq
	a.toasts = append(a.toasts, toast{id: id, level: level, text: text})
	a.changed()
	time.AfterFunc(6*time.Second, func() {
		a.mu.Lock()
		a.toasts = slices.DeleteFunc(a.toasts, func(t toast) bool { return t.id == id })
		a.mu.Unlock()
		a.changed()
	})
}

// pendingChoices are settings changed while no session could take them.
type pendingChoices struct {
	model  string
	effort agent.Effort
	mode   agent.PermissionMode
}

// start replaces the live session with a new one from cfg. With a resume
// target, the stored history is shown first.
func (a *app) start(cfg agent.Config) {
	a.mu.Lock()
	old := a.sess
	a.gen++
	gen := a.gen
	a.sess = nil
	a.status, a.err = "starting", ""
	a.resetLocked()
	cfg.Cwd = a.cwd
	if cfg.Model == "" {
		cfg.Model = a.model
	}
	if cfg.PermissionMode == "" {
		cfg.PermissionMode = a.mode
	}
	if cfg.Effort == "" {
		cfg.Effort = a.effort
	}
	cfg.Logger = a.logger
	a.sent = cfg.Resume != "" // a new conversation is stored once a message is sent
	// A new conversation waits for its first message: starting the agent
	// runs its start-up hooks and fills its context for nothing.
	deferred := cfg.Resume == "" && !cfg.Fork
	if deferred {
		a.deferred, a.status = &cfg, "ready"
		if len(a.models) == 0 {
			a.fetchModelsLocked(gen, nil)
		}
		// Shown only: the agent applies its settings itself.
		mode, effort := a.backend.Defaults(a.cwd)
		a.mode, a.effort = cmp.Or(cfg.PermissionMode, mode), cmp.Or(cfg.Effort, effort)
	}
	a.mu.Unlock()
	a.changed()
	go func() {
		if old != nil {
			_ = old.Close()
		}
		if deferred {
			return
		}
		if cfg.Resume != "" {
			a.loadHistory(gen, cfg.Resume)
		}
		sess, err := a.backend.Start(a.ctx, cfg)
		a.attach(gen, sess, err)
	}()
}

// firstSendLocked hands the deferred first message on once the start
// ended, in a session or not.
func (a *app) firstSendLocked(started bool) {
	if send := a.firstSend; send != nil {
		a.firstSend = nil
		send(started)
	}
}

// launchLocked starts the deferred conversation with the model, effort
// and mode chosen meanwhile; then runs once it attached.
func (a *app) launchLocked(then func(started bool)) {
	cfg := *a.deferred
	a.deferred = nil
	a.firstSend = func(started bool) {
		if !started {
			a.deferred = &cfg // the next message tries again
		}
		then(started)
	}
	// Only choices made meanwhile: the shown defaults are the agent's own.
	p := a.pending
	cfg.Model, cfg.Effort, cfg.PermissionMode = cmp.Or(p.model, cfg.Model), cmp.Or(p.effort, cfg.Effort), cmp.Or(p.mode, cfg.PermissionMode)
	a.pending = pendingChoices{}
	a.status = "starting"
	gen := a.gen
	go func() {
		sess, err := a.backend.Start(a.ctx, cfg)
		a.attach(gen, sess, err)
	}()
}

// adopt makes sess, already started (a fork) from session generation
// from, the live session. It reports false, closing sess, when another
// session has replaced that one meanwhile.
func (a *app) adopt(from int, sess agent.Session) bool {
	a.mu.Lock()
	if from != a.gen {
		a.mu.Unlock()
		_ = sess.Close()
		return false
	}
	old := a.sess
	a.gen++
	gen := a.gen
	a.sess, a.sent, a.err = nil, true, ""
	a.status = "starting"
	a.resetLocked()
	a.mu.Unlock()
	go func() {
		if old != nil {
			_ = old.Close()
		}
		a.loadHistory(gen, sess.ID())
		a.attach(gen, sess, nil)
	}()
	a.changed()
	return true
}

func (a *app) attach(gen int, sess agent.Session, err error) {
	a.mu.Lock()
	if gen != a.gen {
		a.mu.Unlock()
		if sess != nil {
			_ = sess.Close()
		}
		return
	}
	if err != nil {
		a.status, a.err = "error", a.backend.Name()+" did not start: "+err.Error()
		a.firstSendLocked(false)
		a.mu.Unlock()
		a.changed()
		return
	}
	a.sess = sess
	a.info = sess.Info()
	a.status = "idle"
	a.setInfoLocked(a.info)
	a.fetchModelsLocked(gen, sess)
	// Ready the next /new; the spare's settings are this session's.
	go a.backend.Warm(a.ctx, a.sessionConfigLocked(""))
	if cs, ok := sess.(*codex.Session); ok {
		a.fetchAccountLocked(gen, cs)
	}
	// Choices made while the agent was starting apply now.
	p := a.pending
	a.pending = pendingChoices{}
	if p.model != "" && p.model != a.model {
		a.setModelLocked(p.model)
	}
	if p.effort != "" && p.effort != a.effort {
		a.setEffortLocked(p.effort)
	}
	if p.mode != "" && p.mode != a.mode {
		a.setModeLocked(p.mode)
	}
	a.refreshContextLocked()
	a.firstSendLocked(true)
	a.mu.Unlock()
	a.changed()
	for ev := range sess.Events() {
		a.mu.Lock()
		if gen == a.gen {
			a.apply(ev)
		}
		a.mu.Unlock()
		a.changed()
	}
	a.mu.Lock()
	if gen == a.gen && a.status != "error" {
		a.endSessionLocked(sess.Err())
	}
	a.mu.Unlock()
	a.changed()
}

// endSessionLocked tidies up after the claude process exits: nothing will
// finish what was running, and queued messages will never be sent.
func (a *app) endSessionLocked(err error) {
	wasRunning := a.running()
	a.status, a.sess, a.retry = "closed", nil, nil
	reason := a.backend.Name() + " exited"
	if err != nil {
		a.err = err.Error()
		reason += ": " + err.Error()
	}
	for _, e := range a.entries {
		if e.local == nil && e.item.Status == agent.ItemRunning {
			e.item.Status = agent.ItemCancelled
			e.ver++
		}
	}
	var unsent []string
	var turn string
	for _, id := range a.order {
		e := a.entries[id]
		if e == nil || e.local != nil || e.item.Kind != agent.ItemUserMessage || e.item.Synthetic {
			continue
		}
		if a.queued[id] {
			unsent = append(unsent, e.item.Text)
		} else {
			turn = id
		}
	}
	if turn = a.turnOf(turn); wasRunning && turn != "" {
		if _, done := a.turnUsage[turn]; !done {
			a.turnFails[turn] = turnFailure{message: reason}
			a.markTurnLocked(turn)
		}
	}
	a.order = slices.DeleteFunc(a.order, func(id string) bool { return a.queued[id] })
	a.queued, a.queuedTurns = map[string]bool{}, map[string]*agent.Turn{}
	if len(unsent) > 0 && len(a.composer.text) == 0 {
		a.composer.set(strings.Join(unsent, "\n\n"))
	}
	a.toastLocked("error", reason+". /continue resumes this conversation.")
}

func (a *app) loadHistory(gen int, id string) {
	reader, ok := a.backend.Provider().(agent.HistoryReader)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(a.ctx, 15*time.Second)
	defer cancel()
	items, err := reader.ReadHistory(ctx, id, agent.ReadHistoryOptions{Cwd: a.cwd, Limit: historyLimit})
	a.mu.Lock()
	defer a.mu.Unlock()
	if gen != a.gen {
		return
	}
	if err != nil {
		a.toastLocked("error", "Could not read the session's history: "+err.Error())
		return
	}
	for _, it := range items {
		// Stored todo changes come back as plan items named after their
		// todo call: they feed that call's checklist and the dock instead.
		if tool, ok := strings.CutSuffix(it.ID, "/plan"); ok && it.Kind == agent.ItemPlan {
			var steps []agent.PlanStep
			if json.Unmarshal(it.Raw, &steps) == nil {
				a.planAt[tool], a.plan = steps, steps
				if e := a.entries[tool]; e != nil {
					e.ver++
				}
				continue
			}
		}
		if changed, ok := a.transcript.Apply(&agent.ItemCompleted{Item: it}); ok {
			a.upsertLocked(changed)
		}
	}
}

// apply folds one Claude event into the state. Caller holds mu.
func (a *app) apply(ev agent.Event) {
	if changed, ok := a.transcript.Apply(ev); ok {
		switch ev.(type) {
		case *agent.ItemsRemoved, *agent.ConversationReset:
			a.syncOrderLocked()
		default:
			a.upsertLocked(changed)
		}
	}
	if _, ok := ev.(*agent.ConversationReset); ok {
		// Claude Code's /clear: what belonged to the old conversation goes.
		for _, ids := range a.subOrder {
			for _, id := range ids {
				delete(a.entries, id)
			}
		}
		a.subOrder, a.tasks, a.plan, a.planAt = map[string][]string{}, map[string]agent.Task{}, nil, map[string][]agent.PlanStep{}
		a.queued, a.queuedTurns, a.compaction, a.suggestion, a.ctxUsage = map[string]bool{}, map[string]*agent.Turn{}, nil, "", nil
		a.resumedAt = -1
		a.refreshContextLocked()
	}
	switch ev := ev.(type) {
	case *agent.SessionStarted:
		a.setInfoLocked(ev.Info)
		if ev.Resumed && len(a.order) > 0 {
			a.resumedAt = len(a.order)
		}
	case *agent.SessionUpdated:
		a.setInfoLocked(ev.Info)
	case *agent.StatusChanged:
		if !a.running() && ev.Status == agent.StatusRunning {
			a.turnStart = time.Now()
		}
		a.status = string(ev.Status)
	case *agent.TurnStarted:
		// A queued turn follows the last without an idle status between.
		a.turnStart = time.Now()
		a.turnSeen[ev.TurnID] = true
		a.turnDiff = ""
		a.unqueueLocked(ev.TurnID)
		a.dropStandInLocked(ev.TurnID)
	case *agent.TurnCompleted:
		a.turnSeen[ev.TurnID] = true
		a.pathCache = map[string]string{}
		a.unqueueLocked(ev.TurnID)
		a.refreshContextLocked()
		a.retry = nil
		if r := ev.Result; r != nil && !a.sameResultLocked(r) {
			a.costUSD += r.Usage.CostUSD
			turn := firstNonEmpty(ev.TurnID, r.TurnID)
			u := r.Usage
			a.turnUsage[turn] = turnUsage{duration: r.Duration, input: u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens, output: u.OutputTokens, cost: u.CostUSD,
				stopped: strings.ReplaceAll(stopReasons[r.StopReason], "%s", a.backend.Name())}
			switch {
			case r.Err == nil:
				a.lastOutcome = activity{state: "done"}
			case errors.Is(r.Err, agent.ErrInterrupted):
				a.lastOutcome = activity{state: "idle"}
			default:
				a.lastOutcome = activity{state: "error", msg: r.Err.Error()}
			}
			switch {
			case errors.Is(r.Err, agent.ErrInterrupted):
				a.turnFails[turn] = turnFailure{aborted: true}
			case errors.Is(r.Err, agent.ErrCancelled):
				// A queued message that was dropped never ran.
				a.order = slices.DeleteFunc(a.order, func(id string) bool { return id == turn })
				delete(a.entries, turn)
			case r.Err != nil:
				f := turnFailure{message: r.Err.Error()}
				var te *agent.TurnError
				if errors.As(r.Err, &te) {
					f.message = firstNonEmpty(te.Message, f.message)
					if te.HTTPStatus > 0 {
						f.code = fmt.Sprint(te.HTTPStatus)
					} else {
						f.code = te.Code
					}
					f.resetsAt = te.ResetsAt
				}
				a.turnFails[turn] = f
			}
			a.markTurnLocked(turn)
		}
	case *agent.UsageUpdated:
		if ev.Usage.ContextTokens > 0 {
			a.contextTokens = ev.Usage.ContextTokens
		}
		if ev.Usage.ContextWindow > 0 {
			a.contextWindow = ev.Usage.ContextWindow
		}
	case *agent.TaskUpdated:
		a.tasks[ev.Task.ID] = ev.Task
		if ev.Task.ToolCallID != "" {
			if it, ok := a.transcript.Item(ev.Task.ToolCallID); ok {
				a.upsertLocked(it)
			}
		}
	case *agent.PlanUpdated:
		a.plan = slices.Clone(ev.Steps)
		if ev.ItemID != "" {
			a.planAt[ev.ItemID] = a.plan
			if e := a.entries[ev.ItemID]; e != nil {
				e.ver++
			}
		}
	case *agent.RateLimitUpdated:
		a.rateLimits = slices.Clone(ev.Limits)
	case *agent.GoalUpdated:
		a.goal = ev.Goal
	case *agent.Retry:
		if ev.Done {
			a.retry = nil
		} else {
			r := *ev
			a.retry, a.retryAt = &r, time.Now()
		}
	case *agent.TextDelta, *agent.ReasoningDelta, *agent.ToolInputDelta, *agent.ItemStarted:
		// Claude announces no end to a retry: any progress means it worked.
		a.retry = nil
		if r, ok := ev.(*agent.ReasoningDelta); ok && r.Summary {
			a.summaries[r.ItemID] = true
		}
	case *agent.Compaction:
		if ev.Done && a.status == string(agent.StatusCompacting) {
			// Claude sends no status when compaction ends.
			a.status = string(agent.StatusRunning)
		}
		switch {
		case ev.Done && ev.Error != "":
			a.toastLocked("error", "Compaction failed: "+ev.Error)
		case ev.Done:
			c := *ev
			a.compaction = &c
			a.refreshContextLocked()
		}
	case *agent.HookRun:
		name := firstNonEmpty(ev.Name, ev.Event)
		switch {
		case !ev.Done:
			a.hookRunning = name
		case ev.Outcome == agent.HookFailed || ev.Outcome == agent.HookBlocked:
			a.hookRunning = ""
			a.toastLocked("error", fmt.Sprintf("Hook %s %s: %s", name, ev.Outcome, firstLine(ev.Output)))
		default:
			a.hookRunning = ""
			// A hook that says something is shown, folded: Claude Code passes
			// what SessionStart and UserPromptSubmit hooks print to the model,
			// and shows its own users none of it.
			if out := strings.TrimSpace(ev.Output); out != "" {
				a.addLocal(hookCard(ev.Event, ev.Name, out))
			}
		}
	case *agent.Notice:
		msg := ev.Message
		if ev.Code == "auth" {
			msg += " · run claude /login in a terminal, then /continue"
		}
		a.toastLocked(string(ev.Level), msg)
	case *claude.ToolProgress:
		// The tool's own clock, so a redrawn row keeps counting from it.
		a.toolStart[ev.ToolUseID] = time.Now().Add(-ev.Elapsed)
	case *claude.CommandLifecycle:
		if ev.State != "" && ev.State != "queued" {
			a.turnSeen[ev.CommandUUID] = true
			a.unqueueLocked(ev.CommandUUID)
		}
	case *codex.TerminalInteraction:
		a.stdin[ev.ItemID] += ev.Stdin
		if e := a.entries[ev.ItemID]; e != nil {
			e.ver++
		}
	case *codex.DiffUpdated:
		a.turnDiff = ev.Diff
	case *codex.OtherRateLimitUpdated:
		a.applyOtherLimitsLocked(ev)
	case *claude.DeferredToolUse:
		a.toastLocked("warning", "A hook deferred "+ev.ToolName+"; it did not run")
	case *agent.EventsDropped:
		a.toastLocked("error", fmt.Sprintf("%d events were dropped", ev.Count))
	case *claude.PromptSuggestion:
		a.suggestion = strings.TrimSpace(ev.Suggestion)
	case *claude.BackgroundTasksChanged:
		a.background = slices.Clone(ev.Tasks)
	case *claude.ToolUseSummary:
		for _, id := range ev.ToolUseIDs {
			a.toolSummary[id] = ev.Summary
		}
		for _, id := range ev.ToolUseIDs {
			if e := a.entries[id]; e != nil {
				e.ver++
			}
		}
	case agent.Request:
		a.requests = append(a.requests, ev)
	case *agent.RequestResolved:
		a.dropRequestLocked(ev.Request.RequestID())
	}
}

func (a *app) setInfoLocked(info agent.SessionInfo) {
	if info.ID != "" {
		a.info.ID = info.ID
	}
	if info.Model != "" {
		a.model = info.Model
	}
	if info.PermissionMode != "" {
		a.mode = info.PermissionMode
	}
	if info.Effort != "" {
		a.effort = info.Effort
	}
	if info.Title != "" {
		a.info.Title = info.Title
	}
	if len(info.Commands) > 0 {
		a.info.Commands = info.Commands
	}
	if len(info.MCPServers) > 0 {
		a.info.MCPServers = info.MCPServers
	}
	if info.Version != "" {
		a.info.Version = info.Version
	}
}

func (a *app) unqueueLocked(turnID string) {
	if turnID == "" || !a.queued[turnID] {
		return
	}
	delete(a.queued, turnID)
	// It was listed when sent; it belongs where it starts running.
	if i := slices.Index(a.order, turnID); i >= 0 {
		a.order = append(slices.Delete(a.order, i, i+1), turnID)
	}
	if it, ok := a.transcript.Item(turnID); ok {
		a.upsertLocked(it)
	}
}

func (a *app) dropRequestLocked(id string) {
	a.requests = slices.DeleteFunc(a.requests, func(r agent.Request) bool { return r.RequestID() == id })
	delete(a.answers, id)
}

// upsertLocked records the latest state of it. Subagent items are kept for
// their Agent call but not listed in the main order.
func (a *app) upsertLocked(it agent.Item) {
	e, ok := a.entries[it.ID]
	if !ok {
		e = &entry{}
		a.entries[it.ID] = e
		if it.SubagentID == "" {
			a.order = append(a.order, it.ID)
		} else {
			a.subOrder[it.SubagentID] = append(a.subOrder[it.SubagentID], it.ID)
		}
	} else if e.local == nil && e.item.SubagentID == "" && it.SubagentID != "" {
		// First seen before its subagent was known: it moves under it.
		a.order = slices.DeleteFunc(a.order, func(id string) bool { return id == it.ID })
		a.subOrder[it.SubagentID] = append(a.subOrder[it.SubagentID], it.ID)
	}
	e.item = it
	e.ver++
	// Only a thought seen running is timed; one from history has no span.
	if span, timed := a.thinking[it.ID]; it.Kind == agent.ItemReasoning && (timed || it.Status == agent.ItemRunning) {
		if span.start.IsZero() {
			span.start = firstTime(it.Time, time.Now())
		}
		if it.Status != agent.ItemRunning && span.end.IsZero() {
			span.end = time.Now()
		}
		a.thinking[it.ID] = span
	}
	// Each Agent call up the chain shows its subagent's work: a task names
	// its call, and otherwise the item's ParentID does.
	for sub, parent, depth := it.SubagentID, it.ParentID, 0; sub != "" && depth < 8; depth++ {
		if t, ok := a.tasks[sub]; ok && t.ToolCallID != "" {
			parent = t.ToolCallID
		}
		p := a.entries[parent]
		if p == nil {
			break
		}
		p.ver++
		sub, parent = p.item.SubagentID, p.item.ParentID
	}
}

// dropStandInLocked removes the queue's stand-in for a message that now
// runs as turn: the agent's own message item takes its place.
func (a *app) dropStandInLocked(turn string) {
	for id, t := range a.queuedTurns {
		if id == turn || t.ID() == turn {
			delete(a.queuedTurns, id)
			delete(a.queued, id)
			delete(a.entries, id)
			a.order = slices.DeleteFunc(a.order, func(o string) bool { return o == id })
		}
	}
}

// joinNonEmpty joins the parts that are not empty.
func joinNonEmpty(sep string, parts ...string) string {
	return strings.Join(slices.DeleteFunc(parts, func(p string) bool { return strings.TrimSpace(p) == "" }), sep)
}

// hookCard is a hook's output, folded: "SessionStart hook · startup · 12
// lines added to the context".
func hookCard(event, name, out string) localCard {
	// Claude names a hook "<event>:<matcher>", such as "SessionStart:startup".
	matcher := strings.TrimPrefix(strings.TrimPrefix(name, event), ":")
	lines := strings.Count(out, "\n") + 1
	meta := plural(lines, "line")
	if event == "SessionStart" || event == "UserPromptSubmit" {
		meta += " added to the context"
	}
	return localCard{Title: firstNonEmpty(event, name) + " hook", Meta: joinNonEmpty(" · ", matcher, meta),
		Text: tail(out, 8000), Plain: true, Collapsed: true}
}

// turnOf is the turn a user message started. Claude names the turn after
// the message; Codex gives it its own id.
func (a *app) turnOf(messageID string) string {
	if e := a.entries[messageID]; e != nil && e.item.TurnID != "" {
		return e.item.TurnID
	}
	return messageID
}

// sameResultLocked reports whether r is a copy of the result counted last.
// A steered turn ends once per message it carried, each with a copy of one
// result; the copies share Raw.
func (a *app) sameResultLocked(r *agent.TurnResult) bool {
	if len(r.Raw) == 0 {
		return false
	}
	if &r.Raw[0] == a.countedResult {
		return true
	}
	a.countedResult = &r.Raw[0]
	return false
}

// addLocal appends a command's output card to the transcript.
func (a *app) addLocal(card localCard) {
	a.localSeq++
	id := fmt.Sprintf("local-%d", a.localSeq)
	a.entries[id] = &entry{local: &card, ver: 1}
	a.order = append(a.order, id)
	a.changed()
}

// syncOrderLocked drops entries the transcript no longer has. Local cards
// stay.
func (a *app) syncOrderLocked() {
	a.order = slices.DeleteFunc(a.order, func(id string) bool {
		e := a.entries[id]
		if e == nil {
			return true
		}
		if e.local != nil {
			return false
		}
		if _, ok := a.transcript.Item(id); ok {
			return false
		}
		delete(a.entries, id)
		return true
	})
}

func (a *app) running() bool {
	switch a.status {
	case "running", "compacting", "waiting":
		return true
	}
	return false
}

// fetchModelsLocked lists the session's models off the lock.
func (a *app) fetchModelsLocked(gen int, sess agent.Session) {
	go func() {
		ctx, cancel := context.WithTimeout(a.ctx, 15*time.Second)
		defer cancel()
		models := a.backend.Models(ctx, sess)
		a.mu.Lock()
		defer a.mu.Unlock()
		if gen == a.gen && len(models) > 0 {
			a.models = models
			a.changed()
		}
	}()
}

// modelLocked is the current model's entry, if the list names it.
func (a *app) modelLocked() (agent.Model, bool) {
	for _, m := range a.models {
		if m.ID == a.model || (m.ResolvedID != "" && m.ResolvedID == a.model) {
			return m, true
		}
	}
	return agent.Model{}, false
}

// codexSession is the live session as a Codex session.
func (a *app) codexSession() (*codex.Session, error) {
	if a.sess == nil {
		return nil, errors.New(a.backend.Name() + " is not running")
	}
	cs, ok := a.sess.(*codex.Session)
	if !ok {
		return nil, agent.ErrUnsupported
	}
	return cs, nil
}

// claudeSession returns the live session as Claude's own type, for native
// controls the uniform API lacks.
func (a *app) claudeSession() (*claude.Session, error) {
	if a.sess == nil {
		return nil, errors.New(a.backend.Name() + " is not running")
	}
	cs, ok := a.sess.(*claude.Session)
	if !ok {
		return nil, agent.ErrUnsupported
	}
	return cs, nil
}

// liveSession returns the live session and its generation, or an error
// toast-worthy explanation. Caller holds mu.
func (a *app) liveSession() (agent.Session, int, error) {
	if a.sess == nil {
		switch a.status {
		case "error":
			return nil, 0, errors.New(a.err)
		case "closed":
			return nil, 0, errors.New(a.backend.Name() + " exited; /continue resumes this conversation")
		}
		if a.deferred != nil {
			return nil, 0, errors.New(a.backend.Name() + " starts with your first message")
		}
		return nil, 0, errors.New(a.backend.Name() + " is still starting")
	}
	return a.sess, a.gen, nil
}

// markTurnLocked redraws the entries of a turn, for its footer.
func (a *app) markTurnLocked(turn string) {
	if e := a.entries[turn]; e != nil {
		e.ver++
	}
}

func firstTime(ts ...time.Time) time.Time {
	for _, t := range ts {
		if !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}

func (a *app) takeReveal() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.revealRewind && a.rewind != nil
	a.revealRewind = false
	return r
}
