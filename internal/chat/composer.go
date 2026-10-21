package chat

import (
	"strings"
	"unicode"
	"unicode/utf16"
)

const (
	undoLimit = 200
	sentLimit = 100
)

// composer is the message being written. Tern draws it and, with native
// editing, owns the selection; offsets on the wire are UTF-16 code units,
// kept here as rune indexes.
type composer struct {
	text   []rune
	cursor int
	undo   []snapshot
	redo   []snapshot

	history []string // sent messages, oldest first
	hist    int      // index into history while browsing; len(history) when not
	stash   string   // the draft that browsing replaced
}

type snapshot struct {
	text   string
	cursor int
}

func (c *composer) String() string { return string(c.text) }

func (c *composer) save() {
	c.undo = append(c.undo, snapshot{string(c.text), c.cursor})
	if len(c.undo) > undoLimit {
		c.undo = c.undo[len(c.undo)-undoLimit:]
	}
	c.redo = nil
}

func (c *composer) set(s string) {
	c.text = []rune(s)
	c.cursor = len(c.text)
}

// reset clears the draft after sending, keeping undo so it can come back.
func (c *composer) reset() {
	c.save()
	c.text, c.cursor = nil, 0
	c.hist = len(c.history)
}

func (c *composer) insert(s string) {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	r := []rune(s)
	c.save()
	c.text = append(c.text[:c.cursor], append(r, c.text[c.cursor:]...)...)
	c.cursor += len(r)
}

func (c *composer) deleteRange(from, to int) {
	from, to = max(0, min(from, len(c.text))), max(0, min(to, len(c.text)))
	if from >= to {
		return
	}
	c.save()
	c.text = append(c.text[:from], c.text[to:]...)
	c.cursor = from
}

// cursor16 is the cursor as a UTF-16 offset for the editor node.
func (c *composer) cursor16() int { return len(utf16.Encode(c.text[:c.cursor])) }

// runeIndex converts a UTF-16 offset into c.text to a rune index, rounding
// down inside a surrogate pair and clamping to the end.
func (c *composer) runeIndex(u int) int {
	n := 0
	for i, r := range c.text {
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		if n+w > u {
			return i
		}
		n += w
	}
	return len(c.text)
}

// edit applies a native edit: replace from..to (UTF-16) with text.
func (c *composer) edit(from, to int, text string, cursor int) {
	f, t := c.runeIndex(from), c.runeIndex(to)
	if t < f {
		f, t = t, f
	}
	if f != t || text != "" {
		c.save()
		c.text = append(c.text[:f], append([]rune(text), c.text[t:]...)...)
	}
	c.cursor = c.runeIndex(cursor)
}

func (c *composer) doUndo() bool {
	if len(c.undo) == 0 {
		return false
	}
	c.redo = append(c.redo, snapshot{string(c.text), c.cursor})
	s := c.undo[len(c.undo)-1]
	c.undo = c.undo[:len(c.undo)-1]
	c.text, c.cursor = []rune(s.text), min(s.cursor, len([]rune(s.text)))
	return true
}

func (c *composer) doRedo() bool {
	if len(c.redo) == 0 {
		return false
	}
	c.undo = append(c.undo, snapshot{string(c.text), c.cursor})
	s := c.redo[len(c.redo)-1]
	c.redo = c.redo[:len(c.redo)-1]
	c.text, c.cursor = []rune(s.text), min(s.cursor, len([]rune(s.text)))
	return true
}

func (c *composer) prevChar() int { return max(c.cursor-1, 0) }
func (c *composer) nextChar() int { return min(c.cursor+1, len(c.text)) }

func (c *composer) prevWord() int {
	i := c.cursor
	for i > 0 && unicode.IsSpace(c.text[i-1]) {
		i--
	}
	for i > 0 && !unicode.IsSpace(c.text[i-1]) {
		i--
	}
	return i
}

func (c *composer) nextWord() int {
	i := c.cursor
	for i < len(c.text) && unicode.IsSpace(c.text[i]) {
		i++
	}
	for i < len(c.text) && !unicode.IsSpace(c.text[i]) {
		i++
	}
	return i
}

func (c *composer) lineStart() int {
	i := c.cursor
	for i > 0 && c.text[i-1] != '\n' {
		i--
	}
	return i
}

func (c *composer) lineEnd() int {
	i := c.cursor
	for i < len(c.text) && c.text[i] != '\n' {
		i++
	}
	return i
}

// moveLine moves one logical line up or down keeping the column; false at
// the first or last line (where ↑ and ↓ browse history).
func (c *composer) moveLine(dir int) bool {
	ls := c.lineStart()
	col := c.cursor - ls
	if dir < 0 {
		if ls == 0 {
			return false
		}
		prevEnd := ls - 1
		ps := prevEnd
		for ps > 0 && c.text[ps-1] != '\n' {
			ps--
		}
		c.cursor = min(ps+col, prevEnd)
		return true
	}
	le := c.lineEnd()
	if le >= len(c.text) {
		return false
	}
	ns := le + 1
	ne := ns
	for ne < len(c.text) && c.text[ne] != '\n' {
		ne++
	}
	c.cursor = min(ns+col, ne)
	return true
}

func (c *composer) remember(text string) {
	if n := len(c.history); n == 0 || c.history[n-1] != text {
		c.history = append(c.history, text)
		if len(c.history) > sentLimit {
			c.history = c.history[len(c.history)-sentLimit:]
		}
	}
	c.hist = len(c.history)
}

// browse walks sent messages; false when there is nothing to show.
func (c *composer) browse(dir int) bool {
	n := len(c.history)
	if n == 0 {
		return false
	}
	if c.hist >= n {
		c.stash = string(c.text)
	}
	i := max(0, min(c.hist+dir, n))
	if i == c.hist {
		return false
	}
	c.hist = i
	if i == n {
		c.set(c.stash)
	} else {
		c.set(c.history[i])
	}
	return true
}

// token returns the word under the cursor and where it starts: the text
// completion works on (a leading "/" command or an "@" path).
func (c *composer) token() (string, int) {
	start := c.cursor
	for start > 0 && !unicode.IsSpace(c.text[start-1]) {
		start--
	}
	return string(c.text[start:c.cursor]), start
}

// replaceToken swaps the word under the cursor for s.
func (c *composer) replaceToken(s string) {
	_, start := c.token()
	end := c.cursor
	for end < len(c.text) && !unicode.IsSpace(c.text[end]) {
		end++
	}
	c.save()
	r := []rune(s)
	c.text = append(c.text[:start], append(r, c.text[end:]...)...)
	c.cursor = start + len(r)
}

// completion is the open list of suggestions above the composer.
type completion struct {
	kind  string // "command" or "file"; "" when closed
	query string // the token the suggestions answer
	items []suggestion
	sel   int
}

type suggestion struct {
	Value  string // replaces the token
	Label  string
	Detail string
}

func (c *completion) close()     { *c = completion{} }
func (c *completion) open() bool { return c.kind != "" && len(c.items) > 0 }
