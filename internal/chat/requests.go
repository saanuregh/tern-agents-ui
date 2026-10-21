package chat

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strconv"
	"strings"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/stencil-hq/tern-sdk/go/tern"
)

// answerDraft holds a question or form answer while the user builds it.
type answerDraft struct {
	picks   map[string][]string // question id → chosen labels or typed text
	fields  map[string]any      // elicitation form values by property name
	editing string              // form field the composer types into

	q         int               // the question shown; len(questions) is the review tab
	cursor    int               // the row under the cursor
	other     string            // the "Other" text being typed for the shown question
	typing    bool              // keys go to other
	otherPick map[string]string // question id → the Other answer among its picks
}

func (a *app) draftFor(id string) *answerDraft {
	d := a.answers[id]
	if d == nil {
		d = &answerDraft{picks: map[string][]string{}, fields: map[string]any{}, otherPick: map[string]string{}}
		a.answers[id] = d
	}
	return d
}

func (a *app) firstRequestLocked() agent.Request {
	for _, r := range a.requests {
		if u, ok := r.(*agent.UserInputRequest); ok && u.Async {
			continue
		}
		return r
	}
	return nil
}

// respond answers a request off the lock and drops it from the dock, even
// when the harness reports the resolution late.
// When the answer fails to send, the request comes back so it can still be
// answered.
func (a *app) respond(r agent.Request, send func(ctx context.Context) error) {
	id, gen, draft := r.RequestID(), a.gen, a.answers[r.RequestID()]
	a.dropRequestLocked(id)
	a.async("Answer", func(ctx context.Context) error {
		err := send(ctx)
		if err == nil || errors.Is(err, agent.ErrRequestDone) {
			return nil
		}
		a.mu.Lock()
		if gen == a.gen && !slices.ContainsFunc(a.requests, func(x agent.Request) bool { return x.RequestID() == id }) {
			a.requests = append([]agent.Request{r}, a.requests...)
			if draft != nil {
				a.answers[id] = draft
			}
			a.changed()
		}
		a.mu.Unlock()
		return err
	})
}

// permit answers a permission request. A refusal carries the composer's
// text, if any, as the reason Claude sees.
func (a *app) permitLocked(r *agent.PermissionRequest, decision agent.PermissionDecision) {
	resp := agent.PermissionResponse{Decision: decision}
	if decision == agent.DecisionDeny {
		resp.Message = strings.TrimSpace(a.composer.String())
		if resp.Message != "" {
			a.composer.reset()
		}
	}
	a.respond(r, func(ctx context.Context) error { return r.Respond(ctx, resp) })
}

// approvePlanLocked accepts a plan and leaves plan mode for mode.
func (a *app) approvePlanLocked(r *agent.PermissionRequest, mode agent.PermissionMode) {
	a.permitLocked(r, agent.DecisionAllow)
	a.mode = mode
	if sess, _, err := a.liveSession(); err == nil {
		a.async("Permission mode", func(ctx context.Context) error { return sess.SetPermissionMode(ctx, mode) })
	}
}

func (a *app) requestKeyLocked(k tern.Key) bool {
	switch r := a.firstRequestLocked().(type) {
	case *agent.PermissionRequest:
		if r.Kind == agent.PermissionPlanApproval {
			return false
		}
		switch {
		case k.Is("1"):
			a.permitLocked(r, agent.DecisionAllow)
		case k.Is("2") && offers(r, agent.DecisionAllowSession):
			a.permitLocked(r, agent.DecisionAllowSession)
		case k.Is("a") && offers(r, agent.DecisionAllowAlways) && len(r.Decisions) > 0:
			a.permitLocked(r, agent.DecisionAllowAlways)
		case k.Is("3"):
			a.permitLocked(r, agent.DecisionDeny)
		case k.Is("escape") && offers(r, agent.DecisionCancel):
			a.permitLocked(r, agent.DecisionCancel)
		default:
			return false
		}
		return true
	case *agent.ElicitationRequest:
		switch {
		case k.Is("enter") && (r.Mode == "url" || r.URL != ""):
			a.respond(r, func(ctx context.Context) error { return r.Accept(ctx, nil) })
		case k.Is("enter"):
			a.acceptFormLocked(r)
		case k.Is("escape"):
			a.respond(r, func(ctx context.Context) error { return r.Decline(ctx) })
		default:
			return false
		}
		return true
	case *agent.UserInputRequest:
		n, err := strconv.Atoi(k.Text)
		if err != nil || k.Ctrl || k.Alt || k.Meta {
			return false
		}
		d := a.draftFor(r.ID)
		for _, q := range r.Questions {
			if len(d.picks[q.ID]) == 0 && n >= 1 && n <= len(q.Options) {
				a.chooseLocked(r, q.ID, q.Options[n-1].Label)
				return true
			}
		}
	}
	return false
}

func offers(r *agent.PermissionRequest, d agent.PermissionDecision) bool {
	return len(r.Decisions) == 0 && d != agent.DecisionAllowAlways || slices.Contains(r.Decisions, d)
}

// chooseLocked records an option; single-choice questions answer the
// request once every question has an answer.
func (a *app) chooseLocked(r *agent.UserInputRequest, qid, label string) {
	d := a.draftFor(r.ID)
	var q *agent.Question
	for i := range r.Questions {
		if r.Questions[i].ID == qid {
			q = &r.Questions[i]
		}
	}
	if q == nil {
		return
	}
	if q.MultiSelect {
		picked := d.picks[qid]
		if i := slices.Index(picked, label); i >= 0 {
			d.picks[qid] = slices.Delete(picked, i, i+1)
		} else {
			d.picks[qid] = append(picked, label)
		}
		return
	}
	d.picks[qid] = []string{label}
	for _, q := range r.Questions {
		if q.MultiSelect || len(d.picks[q.ID]) == 0 {
			return
		}
	}
	a.submitAnswersLocked(r)
}

func (a *app) submitAnswersLocked(r *agent.UserInputRequest) {
	d := a.draftFor(r.ID)
	answers := map[string][]string{}
	for _, q := range r.Questions {
		answers[q.ID] = slices.Clone(d.picks[q.ID])
	}
	a.respond(r, func(ctx context.Context) error { return r.Answer(ctx, answers) })
}

// answerFromComposerLocked uses the typed text as an answer: for the first
// free-text question still open, or for the form field being edited.
func (a *app) answerFromComposerLocked(text string) bool {
	switch r := a.firstRequestLocked().(type) {
	case *agent.PermissionRequest:
		// Typed text and Enter deny, telling Claude why.
		if r.Kind == agent.PermissionPlanApproval {
			return false
		}
		a.permitLocked(r, agent.DecisionDeny)
		return true
	case *agent.UserInputRequest:
		d := a.draftFor(r.ID)
		for _, q := range r.Questions {
			if len(d.picks[q.ID]) == 0 && (len(q.Options) == 0 || q.AllowOther) {
				d.picks[q.ID] = []string{text}
				for _, q := range r.Questions {
					if len(d.picks[q.ID]) == 0 {
						return true
					}
				}
				a.submitAnswersLocked(r)
				return true
			}
		}
	case *agent.ElicitationRequest:
		d := a.draftFor(r.ID)
		if d.editing == "" {
			return false
		}
		f := formFields(r.Schema)
		for _, field := range f {
			if field.Name == d.editing {
				d.fields[field.Name] = field.parse(text)
			}
		}
		d.editing = ""
		return true
	}
	return false
}

// formField is one property of an elicitation form's JSON Schema.
type formField struct {
	Name        string
	Title       string
	Description string
	Type        string
	Enum        []string
	Required    bool
	Default     any
}

func (f formField) parse(text string) any {
	switch f.Type {
	case "integer":
		if n, err := strconv.ParseInt(text, 10, 64); err == nil {
			return n
		}
	case "number":
		if n, err := strconv.ParseFloat(text, 64); err == nil {
			return n
		}
	case "boolean":
		return text == "true" || text == "yes" || text == "y"
	}
	return text
}

func formFields(schema json.RawMessage) []formField {
	var s struct {
		Properties map[string]struct {
			Type        string `json:"type"`
			Title       string `json:"title"`
			Description string `json:"description"`
			Enum        []any  `json:"enum"`
			OneOf       []struct {
				Const any    `json:"const"`
				Title string `json:"title"`
			} `json:"oneOf"`
			Default any `json:"default"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if json.Unmarshal(schema, &s) != nil {
		return nil
	}
	var out []formField
	for name, p := range s.Properties {
		f := formField{Name: name, Title: firstNonEmpty(p.Title, name), Description: p.Description, Type: p.Type,
			Required: slices.Contains(s.Required, name), Default: p.Default}
		for _, e := range p.Enum {
			f.Enum = append(f.Enum, toString(e))
		}
		for _, o := range p.OneOf {
			f.Enum = append(f.Enum, toString(o.Const))
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Required != out[j].Required {
			return out[i].Required
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func toString(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case nil:
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func (a *app) acceptFormLocked(r *agent.ElicitationRequest) {
	d := a.draftFor(r.ID)
	values := map[string]any{}
	for _, f := range formFields(r.Schema) {
		switch v, ok := d.fields[f.Name]; {
		case ok:
			values[f.Name] = v
		case f.Default != nil:
			values[f.Name] = f.Default
		case f.Required:
			a.toastLocked("error", f.Title+" is required")
			return
		}
	}
	content, _ := json.Marshal(values)
	a.respond(r, func(ctx context.Context) error { return r.Accept(ctx, content) })
}
