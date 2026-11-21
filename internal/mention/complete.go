package mention

import (
	"path"
	"sort"
	"strings"
)

// Index holds the paths @-completion picks from: files plus their parent
// directories (with a trailing slash).
type Index struct {
	entries []string
}

// NewIndex builds an index from slash-separated file paths.
func NewIndex(files []string) *Index {
	seen := map[string]bool{}
	var entries []string
	add := func(e string) {
		if !seen[e] {
			seen[e] = true
			entries = append(entries, e)
		}
	}
	for _, f := range files {
		for d := path.Dir(f); d != "." && d != "/"; d = path.Dir(d) {
			add(d + "/")
		}
		add(f)
	}
	sort.Strings(entries)
	return &Index{entries: entries}
}

// Len is the number of entries.
func (ix *Index) Len() int { return len(ix.entries) }

// Complete returns up to n entries matching the partial path q: prefix
// matches first, then base-name prefixes, substrings and fuzzy
// (subsequence) matches; shallower and shorter paths first within each.
func (ix *Index) Complete(q string, n int) []string {
	type cand struct {
		e            string
		score, depth int
	}
	lq := strings.ToLower(q)
	var cs []cand
	for _, e := range ix.entries {
		le := strings.ToLower(e)
		if le == lq {
			continue // already typed in full
		}
		score := -1
		base := strings.ToLower(path.Base(strings.TrimSuffix(e, "/")))
		switch {
		case strings.HasPrefix(le, lq):
			score = 0
		case strings.HasPrefix(base, lq):
			score = 1
		case strings.Contains(le, lq):
			score = 2
		case subsequence(lq, le):
			score = 3
		}
		if score < 0 {
			continue
		}
		// Depth below what was typed: "internal/" lists its children first.
		depth := strings.Count(strings.TrimSuffix(e, "/"), "/") - strings.Count(q, "/")
		cs = append(cs, cand{e, score, depth})
	}
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		if a.score != b.score {
			return a.score < b.score
		}
		if a.depth != b.depth {
			return a.depth < b.depth
		}
		if len(a.e) != len(b.e) {
			return len(a.e) < len(b.e)
		}
		return a.e < b.e
	})
	out := make([]string, 0, min(n, len(cs)))
	for _, c := range cs[:min(n, len(cs))] {
		out = append(out, c.e)
	}
	return out
}

func subsequence(q, s string) bool {
	rq := []rune(q)
	if len(rq) == 0 {
		return true
	}
	i := 0
	for _, r := range s {
		if r == rq[i] {
			i++
			if i == len(rq) {
				return true
			}
		}
	}
	return false
}

// Partial returns the @mention being typed at the end of text (without the
// @), and whether there is one.
func Partial(text string) (string, bool) {
	i := strings.LastIndexAny(text, " \t\n")
	tok := text[i+1:]
	if !strings.HasPrefix(tok, "@") || strings.Contains(tok, `"`) {
		return "", false
	}
	return tok[1:], true
}
