package chat

import (
	"context"
	"flag"

	agent "github.com/saanuregh/agent-cli-connector"
)

// backend is what differs between the agents this chat drives. Everything
// the connector's shared interfaces cover stays in the shared code.
type backend interface {
	// Name is the agent as the interface calls it: "Claude" or "Codex".
	Name() string
	// Product names the agent's CLI in the version row and the welcome
	// wordmark, such as "Claude Code".
	Product() string
	// Command is this program's name in usage text and resume hints.
	Command() string
	// Provider lists and reads sessions.
	Provider() agent.Provider
	// Start starts a session, from a process readied by Warm when it can.
	Start(ctx context.Context, cfg agent.Config) (agent.Session, error)
	// Warm readies a process for the next new session, if the agent can.
	Warm(ctx context.Context, cfg agent.Config)
	// Close discards what Warm readied.
	Close()
	// DefaultModel is the -model default; "" lets the agent's config decide.
	DefaultModel() string
	// Prepare adds provider options and environment to a session's config.
	Prepare(cfg *agent.Config)
	// Flags registers the agent's own command-line flags; the returned
	// function applies them to a config Prepare made.
	Flags(fs *flag.FlagSet) func(cfg *agent.Config)
	// Has reports whether the agent offers an optional feature.
	Has(f feature) bool
	// SessionExists reports, with an error, whether id is a stored
	// conversation in cwd.
	SessionExists(ctx context.Context, id, cwd string) error
	// FileSuggestions lists paths for an @-mention query.
	FileSuggestions(ctx context.Context, s agent.Session, query, cwd string) ([]string, error)
	// Efforts are the reasoning efforts -effort accepts.
	Efforts() []agent.Effort
	// Models lists the models the session can switch to.
	Models(ctx context.Context, s agent.Session) []agent.Model
	// Defaults are the permission mode and effort the agent's settings
	// choose, shown until a session reports its own; "" when unknown.
	Defaults(cwd string) (agent.PermissionMode, agent.Effort)
}

// feature is an optional part of the interface; commands and settings for
// a feature the agent lacks are not offered.
type feature int

const (
	featNone              feature = iota // always offered
	featContextBreakdown                 // the context viewer's breakdown
	featSideQuestion                     // /btw
	featFileRewind                       // restoring files when rewinding
	featPromptSuggestions                // the next message as ghost text
	featThinkingBudget                   // a token limit for thinking
	featPlanFile                         // /plan reads the plan file
	featExport                           // /export
	featReview                           // /review
	featGoals                            // /goal
	featShell                            // !command
	featTurnDiff                         // /diff
	featAgents                           // /agents
	featPermissionRules                  // /permissions
	featPlanUsage                        // /usage
	featReload                           // /reload
	featCd                               // /cd
	featMCPAuth                          // /mcp login and logout
	featBackground                       // Ctrl+B moves the running command to the background
	featArchive                          // /archive, /unarchive
	featFeedback                         // /feedback
)
