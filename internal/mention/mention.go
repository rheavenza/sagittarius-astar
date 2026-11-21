// Package mention expands @file mentions in a prompt: each mentioned file
// (or directory, or line range) is attached to the message the model gets.
//
//	explain @internal/agent/agent.go
//	compare @a.go:10-40 with @"dir with space/b.go"
//	what's in @internal/tools/ ?
package mention

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	// MaxFileBytes is attached per file; longer files are cut with a note.
	MaxFileBytes = 100 * 1024
	// MaxTotalBytes bounds all attachments of one message.
	MaxTotalBytes = 400 * 1024
	// maxDirEntries are listed for a mentioned directory.
	maxDirEntries = 200
)

// Attachment is one resolved mention.
type Attachment struct {
	Mention   string // as typed, without the @
	Path      string // as shown to the model (relative to cwd when inside it)
	Dir       bool
	Start     int // line range (1-based, inclusive); 0 = whole file
	End       int
	Lines     int // lines attached (entries, for a directory)
	Bytes     int
	Truncated bool
	Err       error // the mention looked like a path but couldn't be attached
}

// Label is a short description for the UI ("path (12 lines)").
func (a Attachment) Label() string {
	if a.Err != nil {
		return "@" + a.Mention + ": " + a.Err.Error()
	}
	p := a.Path
	if a.Start > 0 {
		p += fmt.Sprintf(":%d-%d", a.Start, a.End)
	}
	unit := "line"
	if a.Dir {
		unit = "entr"
	}
	switch {
	case a.Lines == 1 && a.Dir:
		unit += "y"
	case a.Dir:
		unit += "ies"
	case a.Lines != 1:
		unit += "s"
	}
	s := fmt.Sprintf("%s (%d %s", p, a.Lines, unit)
	if a.Truncated {
		s += ", truncated"
	}
	return s + ")"
}

// mentionRe finds @path and @"quoted path" at the start or after whitespace
// (so e-mail addresses don't match).
var mentionRe = regexp.MustCompile(`(^|\s)@("([^"\n]+)"|[^\s"]+)`)

// rangeRe splits "path:10-20" or "path:10".
var rangeRe = regexp.MustCompile(`^(.+):(\d+)(?:-(\d+))?$`)

// LegacyReadRe is the old /read(file) macro, still accepted.
var legacyReadRe = regexp.MustCompile(`/read\(([^)\n]+)\)`)

// Expand resolves the mentions in text against cwd and returns the message
// content (text followed by the attachments) and what was attached. Mentions
// that don't name an existing path are left alone (they may be @handles);
// those that clearly look like paths are reported with Err.
func Expand(text, cwd string) (string, []Attachment) {
	text = legacyReadRe.ReplaceAllString(text, `@"$1"`)
	var atts []Attachment
	var blocks []string
	seen := map[string]bool{}
	total := 0
	for _, m := range mentionRe.FindAllStringSubmatch(text, -1) {
		raw := m[2]
		quoted := m[3] != ""
		if quoted {
			raw = m[3]
		}
		a, block, ok := resolve(raw, quoted, cwd)
		if !ok {
			continue
		}
		key := a.Path + fmt.Sprint(a.Start, a.End)
		if seen[key] {
			continue
		}
		seen[key] = true
		if a.Err == nil && total+len(block) > MaxTotalBytes {
			a.Err = fmt.Errorf("not attached: the attachments of one message are limited to %d KB (the model can read it with read_file)", MaxTotalBytes/1024)
		}
		if a.Err == nil {
			total += len(block)
			blocks = append(blocks, block)
		}
		atts = append(atts, a)
	}
	if len(blocks) == 0 {
		return text, atts
	}
	return text + "\n\n" + strings.Join(blocks, "\n\n"), atts
}

// Split separates the text the user typed from the attachment blocks
// appended by Expand (for displaying a stored message).
func Split(content string) (text string, paths []string) {
	i := strings.Index(content, "\n\n<file path=\"")
	j := strings.Index(content, "\n\n<directory path=\"")
	if i < 0 || (j >= 0 && j < i) {
		i = j
	}
	if i < 0 {
		return content, nil
	}
	for _, m := range blockRe.FindAllStringSubmatch(content[i:], -1) {
		paths = append(paths, m[1])
	}
	return content[:i], paths
}

var blockRe = regexp.MustCompile(`(?m)^<(?:file|directory) path="([^"]*)"`)

// resolve turns one mention into an attachment. ok is false if the mention
// isn't a path at all.
func resolve(raw string, quoted bool, cwd string) (Attachment, string, bool) {
	cands := []string{raw}
	if !quoted {
		// "see @main.go." — try without trailing punctuation.
		for t := raw; t != ""; {
			t2 := strings.TrimRight(t, ".,;:!?)]}'\"")
			if t2 == t {
				break
			}
			cands, t = append(cands, t2), t2
		}
	}
	for _, c := range cands {
		start, end := 0, 0
		p := c
		if _, err := os.Stat(abs(c, cwd)); err != nil {
			if r := rangeRe.FindStringSubmatch(c); r != nil {
				p = r[1]
				start, _ = strconv.Atoi(r[2])
				end = start
				if r[3] != "" {
					end, _ = strconv.Atoi(r[3])
				}
			}
		}
		full := abs(p, cwd)
		st, err := os.Stat(full)
		if err != nil {
			continue
		}
		a := Attachment{Mention: c, Path: display(full, cwd), Dir: st.IsDir(), Start: start, End: end}
		var block string
		if a.Dir {
			block, err = dirBlock(&a, full)
		} else {
			block, err = fileBlock(&a, full)
		}
		a.Err = err
		return a, block, true
	}
	if looksLikePath(raw) {
		return Attachment{Mention: raw, Err: fmt.Errorf("no such file")}, "", true
	}
	return Attachment{}, "", false
}

func fileBlock(a *Attachment, full string) (string, error) {
	data, err := os.ReadFile(full)
	if err != nil {
		return "", err
	}
	if bytes.IndexByte(data[:min(len(data), 8192)], 0) >= 0 || !utf8.Valid(data) {
		return "", fmt.Errorf("binary file, not attached")
	}
	content := string(data)
	attrs := ""
	if a.Start > 0 {
		lines := strings.SplitAfter(content, "\n")
		if lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		if a.Start > len(lines) {
			return "", fmt.Errorf("has only %d lines", len(lines))
		}
		if a.End < a.Start || a.End > len(lines) {
			a.End = len(lines)
		}
		content = strings.Join(lines[a.Start-1:a.End], "")
		attrs = fmt.Sprintf(" lines=\"%d-%d\"", a.Start, a.End)
	}
	if len(content) > MaxFileBytes {
		cut := content[:MaxFileBytes]
		for !utf8.ValidString(cut) {
			cut = cut[:len(cut)-1]
		}
		if i := strings.LastIndexByte(cut, '\n'); i > 0 {
			cut = cut[:i+1]
		}
		a.Truncated = true
		content = cut + fmt.Sprintf("…[truncated: the first %d of %d bytes are attached; use read_file for the rest]\n", len(cut), len(data))
	}
	a.Bytes = len(content)
	a.Lines = strings.Count(content, "\n")
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
		a.Lines++
	}
	return fmt.Sprintf("<file path=%q%s>\n%s</file>", a.Path, attrs, content), nil
}

func dirBlock(a *Attachment, full string) (string, error) {
	entries, err := os.ReadDir(full)
	if err != nil {
		return "", err
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() {
			if n == ".git" || n == "node_modules" {
				continue
			}
			n += "/"
		}
		names = append(names, n)
	}
	sort.Strings(names)
	more := ""
	if len(names) > maxDirEntries {
		more = fmt.Sprintf("…(%d more)\n", len(names)-maxDirEntries)
		names, a.Truncated = names[:maxDirEntries], true
	}
	a.Lines = len(names)
	body := strings.Join(names, "\n")
	if body != "" {
		body += "\n"
	}
	block := fmt.Sprintf("<directory path=%q>\n%s%s</directory>", a.Path, body, more)
	a.Bytes = len(block)
	return block, nil
}

func looksLikePath(s string) bool {
	return strings.ContainsAny(s, "/\\") || filepath.Ext(strings.TrimRight(s, ".,;:!?)]}'\"")) != ""
}

func abs(p, cwd string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[1:])
		}
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	return filepath.Clean(p)
}

// display is p relative to cwd when inside it, else absolute.
func display(p, cwd string) string {
	if rel, err := filepath.Rel(cwd, p); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(rel)
	}
	return p
}
