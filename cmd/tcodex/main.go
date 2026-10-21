// Command tcodex is a chat with Codex drawn natively by Tern.
//
//	tcodex [-model ID] [-mode MODE] [-effort LEVEL] [-resume ID | -continue] [-cwd DIR] [-log FILE [-debug]]
package main

import "github.com/saanuregh/tern-agents-ui/internal/chat"

func main() { chat.Main(chat.Codex()) }
