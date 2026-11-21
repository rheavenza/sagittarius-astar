package tui

// @file mentions: Tab completion while typing, attachment on submit
// (internal/mention does the work).

import (
	"os"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/clitorhea/sagittarius-astar.git/internal/mention"
	"github.com/clitorhea/sagittarius-astar.git/internal/workspace"
)

const (
	mentionSuggestions = 6
	mentionIndexMaxAge = 30 * time.Second
	mentionIndexFiles  = 20000
)

// mentionState lives behind a pointer: the Model is copied by value.
type mentionState struct {
	mu     sync.Mutex
	ix     *mention.Index
	loaded time.Time

	sugg    []string // current suggestions
	sel     int      // selected suggestion while cycling with Tab
	cycling bool     // Tab was the last key: the list is frozen
	base    string   // text before the mention being completed
}

func (s *mentionState) index() *mention.Index {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ix == nil || time.Since(s.loaded) > mentionIndexMaxAge {
		cwd, _ := os.Getwd()
		files, _, _ := workspace.ListFiles(cwd, mentionIndexFiles)
		s.ix, s.loaded = mention.NewIndex(files), time.Now()
	}
	return s.ix
}

// updateMentions recomputes the suggestions for the text being typed. It
// returns whether the suggestion line appeared or disappeared.
func (m *Model) updateMentions() bool {
	s := m.mentions
	if s == nil || s.cycling {
		return false
	}
	had := len(s.sugg) > 0
	q, ok := mention.Partial(m.textarea.Value())
	if !ok {
		s.sugg = nil
	} else {
		s.sugg = s.index().Complete(q, mentionSuggestions)
	}
	return had != (len(s.sugg) > 0)
}

// resetMentions ends completion (any key but Tab, or a submit).
func (m *Model) resetMentions() {
	if m.mentions != nil {
		m.mentions.cycling = false
	}
}

// completeMention handles Tab on an @mention: the first Tab inserts the best
// match, further Tabs cycle through the list. ok is false if there is no
// mention to complete.
func (m *Model) completeMention() bool {
	s := m.mentions
	if s == nil {
		return false
	}
	if !s.cycling {
		text := m.textarea.Value()
		q, ok := mention.Partial(text)
		if !ok {
			return false
		}
		s.sugg = s.index().Complete(q, mentionSuggestions)
		if len(s.sugg) == 0 {
			return true // a mention with no match: swallow the Tab
		}
		s.base = text[:len(text)-len(q)-1]
		s.sel = 0
		s.cycling = true
	} else {
		s.sel = (s.sel + 1) % len(s.sugg)
	}
	e := s.sugg[s.sel]
	if strings.ContainsAny(e, " \t") {
		e = `"` + e + `"`
	}
	m.textarea.SetValue(s.base + "@" + e)
	m.textarea.CursorEnd()
	return true
}

// mentionLine renders the suggestions shown under the input ("" if none).
func (m Model) mentionLine() string {
	if m.mentions == nil || len(m.mentions.sugg) == 0 {
		return ""
	}
	dim := lipgloss.NewStyle().Foreground(colorSubtext)
	sel := lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	parts := []string{dim.Render("@ Tab:")}
	for i, e := range m.mentions.sugg {
		if m.mentions.cycling && i == m.mentions.sel {
			parts = append(parts, sel.Render(e))
		} else {
			parts = append(parts, dim.Render(e))
		}
	}
	return lipgloss.NewStyle().MaxWidth(max(m.width-4, 10)).Render(strings.Join(parts, "  "))
}
