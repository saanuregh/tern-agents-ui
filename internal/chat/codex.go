package chat

import (
	"context"
	"encoding/json"
	"flag"
	"path/filepath"

	agent "github.com/saanuregh/agent-cli-connector"
	"github.com/saanuregh/agent-cli-connector/codex"
)

type codexBackend struct{ provider *codex.Provider }

// Codex is the backend for the Codex CLI. It leaves the client name unset:
// a name becomes Codex's originator, and Codex tailors its model catalog
// to the originator.
func Codex() backend { return codexBackend{provider: codex.New(codex.Options{})} }

func (codexBackend) Name() string               { return "Codex" }
func (codexBackend) Product() string            { return "Codex" }
func (codexBackend) Command() string            { return "tcodex" }
func (b codexBackend) Provider() agent.Provider { return b.provider }

// DefaultModel is empty, so ~/.codex/config.toml chooses the model.
func (codexBackend) DefaultModel() string { return "" }

func (codexBackend) Defaults(string) (agent.PermissionMode, agent.Effort) { return "", "" }

func (codexBackend) Prepare(cfg *agent.Config) { cfg.ProviderOptions = &codex.SessionOptions{} }

// Flags are Codex's own: the service tier, the personality, and how much
// reasoning summary to show.
func (codexBackend) Flags(fs *flag.FlagSet) func(cfg *agent.Config) {
	tier := fs.String("service-tier", "", "service tier, such as fast or flex (default: your Codex settings)")
	personality := fs.String("personality", "", "Codex's personality (default: your Codex settings)")
	summary := fs.String("reasoning-summary", "", "reasoning summary: auto, concise, detailed or none")
	return func(cfg *agent.Config) {
		opts := cfg.ProviderOptions.(*codex.SessionOptions)
		opts.ServiceTier, opts.Personality, opts.ReasoningSummary = *tier, *personality, *summary
	}
}

func (b codexBackend) Start(ctx context.Context, cfg agent.Config) (agent.Session, error) {
	return b.provider.Start(ctx, cfg)
}

func (codexBackend) Warm(context.Context, agent.Config) {}
func (codexBackend) Close()                             {}

// Efforts include Codex's none and minimal.
func (codexBackend) Efforts() []agent.Effort {
	return []agent.Effort{agent.EffortNone, agent.EffortMinimal, agent.EffortLow, agent.EffortMedium, agent.EffortHigh,
		agent.EffortXHigh, agent.EffortMax}
}

func (codexBackend) Has(f feature) bool {
	switch f {
	case featReview, featGoals, featShell, featTurnDiff, featArchive, featFeedback, featExport:
		return true
	}
	return false
}

// SessionExists reads one turn of the stored thread.
func (b codexBackend) SessionExists(ctx context.Context, id, cwd string) error {
	_, err := b.provider.ReadHistory(ctx, id, agent.ReadHistoryOptions{Cwd: cwd, Limit: 1})
	return err
}

// fuzzyFiles is the reply to Codex's fuzzyFileSearch.
type fuzzyFiles struct {
	Files []struct {
		Path      string `json:"path"`
		Root      string `json:"root"`
		MatchType string `json:"match_type"`
	} `json:"files"`
}

// FileSuggestions searches file names under the chat's folder.
func (codexBackend) FileSuggestions(ctx context.Context, s agent.Session, query, cwd string) ([]string, error) {
	cs, ok := s.(*codex.Session)
	if !ok {
		return nil, agent.ErrUnsupported
	}
	raw, err := cs.Client().FuzzyFileSearch(ctx, query, []string{cwd})
	if err != nil {
		return nil, err
	}
	return fuzzyPaths(raw, cwd)
}

// fuzzyPaths turns a fuzzyFileSearch reply into paths relative to cwd;
// directories end in a slash.
func fuzzyPaths(raw json.RawMessage, cwd string) ([]string, error) {
	var res fuzzyFiles
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	var out []string
	for _, f := range res.Files {
		p := f.Path
		if !filepath.IsAbs(p) {
			p = filepath.Join(firstNonEmpty(f.Root, cwd), p)
		}
		if rel, err := filepath.Rel(cwd, p); err == nil {
			p = rel
		}
		if f.MatchType == "directory" {
			p += "/"
		}
		out = append(out, filepath.ToSlash(p))
	}
	return out, nil
}

// Models asks the session's own app-server; the provider would start
// another one just to list them.
func (b codexBackend) Models(ctx context.Context, s agent.Session) []agent.Model {
	var models []agent.Model
	var err error
	if cs, ok := s.(*codex.Session); ok {
		models, err = cs.Client().ListModels(ctx, false)
	} else {
		models, err = b.provider.ListModels(ctx)
	}
	if err != nil {
		return nil
	}
	return models
}
