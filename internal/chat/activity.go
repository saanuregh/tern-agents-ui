package chat

import (
	"encoding/base64"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	agent "github.com/saanuregh/agent-cli-connector"
	"golang.org/x/term"
)

// Tern shows a pane's agent as working, waiting on you, done or failed
// from OSC 7501 program status reports, on the tab and in its attention
// inbox and banners (with the question, or the error). The report is
// omp's: state=<state>[:kind=<kind>]:app=<name>[:msg=<base64 text>], and
// state=clear when the program stops reporting. The SDK writes the
// protocol to the same terminal, so both go through one lock.

const (
	statusStart = "\x1b]7501;"
	statusEnd   = "\x1b\\"
	// statusMsgMax bounds a report's message, as omp does.
	statusMsgMax = 2048
)

// activity is what the agent is doing, as a program status report says.
type activity struct {
	state string // idle, working, blocked, done or error
	kind  string // for blocked: question or permission
	msg   string // for blocked and error: the question, the request or the error
}

// ttyOut is the terminal's output, shared by the SDK and the status
// reports so that neither writes inside the other's sequence.
type ttyOut struct {
	mu   sync.Mutex
	f    *os.File
	last string // the report last written
}

func (t *ttyOut) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.f.Write(p)
}

// report writes the status for app when it changed.
func (t *ttyOut) report(app string, act activity) {
	t.write(statusReport(app, act))
}

// clear ends the reports, as a program does on exit.
func (t *ttyOut) clear() {
	if t.last != "" {
		t.write(statusStart + "state=clear" + statusEnd)
	}
}

func (t *ttyOut) write(seq string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if seq == t.last {
		return
	}
	t.last = seq
	_, _ = t.f.WriteString(seq)
}

// statusReport encodes act as omp does: the message only for blocked and
// error, whitespace folded, cut to statusMsgMax bytes, base64.
func statusReport(app string, act activity) string {
	b := "state=" + act.state
	if act.state == "blocked" {
		b += ":kind=" + act.kind
	}
	b += ":app=" + app
	if act.state == "blocked" || act.state == "error" {
		if msg := strings.Join(strings.Fields(act.msg), " "); msg != "" {
			if len(msg) > statusMsgMax {
				msg = strings.ToValidUTF8(msg[:statusMsgMax], "")
			}
			b += ":msg=" + base64.StdEncoding.EncodeToString([]byte(msg))
		}
	}
	return statusStart + b + statusEnd
}

// rawTerminal makes stdin raw as the SDK would, for a session whose
// output goes through ttyOut; the SDK leaves raw mode to the caller then.
func rawTerminal() (func() error, error) {
	fd := int(os.Stdin.Fd())
	state, err := term.GetState(fd)
	if err != nil {
		return nil, err
	}
	if _, err := term.MakeRaw(fd); err != nil {
		return nil, err
	}
	return func() error { return term.Restore(fd, state) }, nil
}

// isTerminal reports whether stdin and stdout are a terminal, which the
// SDK otherwise checks itself.
func isTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// closeOnSignal closes the session on SIGINT, SIGTERM or SIGHUP, which
// restores the terminal, then lets the signal end the process. The SDK
// does this itself only when it makes the terminal raw.
func closeOnSignal(closeFn func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		sig := <-ch
		closeFn()
		signal.Stop(ch)
		if p, err := os.FindProcess(os.Getpid()); err == nil {
			_ = p.Signal(sig)
		}
	}()
}

// activityLocked is what the agent does now: waiting on a request, in a
// turn, or how the last turn ended.
func (a *app) activityLocked() activity {
	switch r := a.firstRequestLocked().(type) {
	case *agent.UserInputRequest:
		var qs []string
		for _, q := range r.Questions {
			qs = append(qs, q.Text)
		}
		return activity{state: "blocked", kind: "question", msg: strings.Join(qs, " ")}
	case *agent.PermissionRequest:
		msg := permissionTitle(a.backend.Name(), r, a.cwd)
		if r.Command != "" {
			msg += " " + r.Command
		}
		return activity{state: "blocked", kind: "permission", msg: msg}
	case *agent.ElicitationRequest:
		return activity{state: "blocked", kind: "permission", msg: firstNonEmpty(r.Message, r.Title, r.Server)}
	}
	if a.running() {
		return activity{state: "working"}
	}
	return a.lastOutcome
}
