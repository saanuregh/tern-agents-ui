package chat

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/stencil-hq/tern-sdk/go/tern/ui"
)

// Tern's Markdown links what reads as a file: a separator and an
// extension, or a line position. Directories, bare file names and
// extensionless files stay plain text. linkPaths draws those too, when
// they exist on disk, as linked path spans through the md node's marks.
// Marks apply only to prose, so such a path written as inline code loses
// its backticks first.

var (
	codeSpanRe = regexp.MustCompile("`([^`\n]+)`")
	pathWordRe = regexp.MustCompile(`(?:^|[\s(])((?:~|\.{1,2})?/[^\s` + "`" + `()<>,;]+|[\w.-]+/[^\s` + "`" + `()<>,;]*|[\w-]+\.[A-Za-z0-9]{1,8})`)
	positionRe = regexp.MustCompile(`(:\d+(:\d+)?(-\d+)?|#L\d+)$`)
)

// maxPathMarks bounds the paths one message checks on disk.
const maxPathMarks = 64

// ternLinks reports whether Tern's Markdown already links p.
func ternLinks(p string) bool {
	if positionRe.MatchString(p) {
		return true
	}
	return strings.Contains(p, "/") && filepath.Ext(strings.TrimSuffix(p, "/")) != ""
}

// linkPaths returns text with path code spans Tern won't link unwrapped,
// and the marks that draw every such existing path as a link.
func (a *app) linkPaths(text string) (string, []ui.Span) {
	var marks []ui.Span
	seen := map[string]bool{}
	// mark records a path span for p. In prose it also marks paths Tern
	// links itself, so a shorter mark (a directory) never splits them;
	// marks prefer the longest match.
	mark := func(p string, prose bool) bool {
		// A trailing slash would leave the span's name part empty.
		if len(p) > 1 {
			p = strings.TrimSuffix(p, "/")
		}
		if p == "" || p == "/" || len(p) > 240 || strings.ContainsAny(p, " \t*?$") || (!prose && ternLinks(p)) {
			return false
		}
		if seen[p] {
			return true
		}
		if len(marks) >= maxPathMarks {
			return false
		}
		abs, ok := a.existingPath(positionRe.ReplaceAllString(p, ""))
		if !ok {
			return false
		}
		seen[p] = true
		marks = append(marks, ui.Span{T: p, S: "path", Href: (&url.URL{Scheme: "file", Path: abs}).String()})
		return true
	}
	lines := strings.Split(text, "\n")
	fence := ""
	for i, line := range lines {
		trim := strings.TrimSpace(line)
		if fence != "" {
			if strings.HasPrefix(trim, fence) {
				fence = ""
			}
			continue
		}
		if strings.HasPrefix(trim, "```") || strings.HasPrefix(trim, "~~~") {
			fence = trim[:3]
			continue
		}
		line = codeSpanRe.ReplaceAllStringFunc(line, func(span string) string {
			// Unwrapped, _, * or ~ would read as emphasis; such a span stays code.
			if p := strings.TrimSpace(span[1 : len(span)-1]); !strings.ContainsAny(p, "_*~") && mark(p, false) {
				if len(p) > 1 {
					p = strings.TrimSuffix(p, "/")
				}
				return p
			}
			return span
		})
		for _, m := range pathWordRe.FindAllStringSubmatch(codeSpanRe.ReplaceAllString(line, " "), -1) {
			mark(strings.TrimRight(m[1], ".:!?\"'"), true)
		}
		lines[i] = line
	}
	return strings.Join(lines, "\n"), marks
}

// existingPath resolves p against the chat's folder and home and reports
// whether it exists. Hits are cached until the turn ends, when files may
// have moved.
func (a *app) existingPath(p string) (string, bool) {
	if strings.TrimSpace(p) == "" {
		return "", false
	}
	if v, ok := a.pathCache[p]; ok {
		return v, true
	}
	abs := strings.TrimSuffix(p, "/")
	switch {
	case strings.HasPrefix(abs, "~/"):
		if home, err := os.UserHomeDir(); err == nil {
			abs = filepath.Join(home, abs[2:])
		}
	case !filepath.IsAbs(abs):
		abs = filepath.Join(a.cwd, abs)
	}
	abs = filepath.Clean(abs)
	if _, err := os.Stat(abs); err != nil {
		return "", false // not cached: the file may appear later in the turn
	}
	if len(a.pathCache) > 4096 {
		a.pathCache = map[string]string{}
	}
	a.pathCache[p] = abs
	return abs, true
}
