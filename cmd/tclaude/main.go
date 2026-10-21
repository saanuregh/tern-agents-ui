// Command tclaude is a chat with Claude Code drawn natively by Tern.
//
//	tclaude [-model ID] [-mode MODE] [-effort LEVEL] [-resume ID | -continue] [-cwd DIR] [-log FILE [-debug]]
package main

import "github.com/saanuregh/tern-agents-ui/internal/chat"

func main() { chat.Main(chat.Claude()) }
