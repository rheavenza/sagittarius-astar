package llm

import "strings"

// Some models emit their reasoning inline as <think>…</think> (or
// <thought>…</thought>) instead of a separate reasoning channel. These helpers
// split it from the answer text.

var thinkTags = [][2]string{{"<think>", "</think>"}, {"<thought>", "</thought>"}}

// SplitReasoning splits text into answer content and inline reasoning. The
// boolean reports whether the text ends inside an unclosed reasoning block.
func SplitReasoning(input string) (string, string, bool) {
	var s ThinkSplitter
	s.Write(input)
	s.Flush()
	return s.Content(), s.Reasoning(), s.InThink()
}

// ThinkSplitter incrementally separates inline reasoning from content as
// tokens stream in. Each Write costs O(len(chunk)), unlike re-splitting the
// whole buffer on every token. Tags split across chunks are handled by
// holding back a possible partial tag until the next Write (or Flush).
type ThinkSplitter struct {
	content   strings.Builder
	reasoning strings.Builder
	inThink   bool
	closeTag  string
	pending   string // possible partial tag at the end of the last chunk
}

// Write consumes the next chunk of streamed text.
func (s *ThinkSplitter) Write(chunk string) {
	buf := s.pending + chunk
	s.pending = ""
	for buf != "" {
		if !s.inThink {
			idx, open, closeTag := findThinkOpen(buf)
			if idx == -1 {
				keep := partialSuffix(buf, thinkTags[0][0], thinkTags[1][0])
				s.content.WriteString(buf[:len(buf)-keep])
				s.pending = buf[len(buf)-keep:]
				return
			}
			s.content.WriteString(buf[:idx])
			buf = buf[idx+len(open):]
			s.inThink, s.closeTag = true, closeTag
			continue
		}
		idx := strings.Index(buf, s.closeTag)
		if idx == -1 {
			keep := partialSuffix(buf, s.closeTag)
			s.reasoning.WriteString(buf[:len(buf)-keep])
			s.pending = buf[len(buf)-keep:]
			return
		}
		s.reasoning.WriteString(buf[:idx])
		buf = buf[idx+len(s.closeTag):]
		s.inThink = false
	}
}

// Flush emits any held-back partial tag as plain text (end of stream).
func (s *ThinkSplitter) Flush() {
	if s.pending == "" {
		return
	}
	if s.inThink {
		s.reasoning.WriteString(s.pending)
	} else {
		s.content.WriteString(s.pending)
	}
	s.pending = ""
}

// Content returns the answer text so far (excluding any held-back partial tag).
func (s *ThinkSplitter) Content() string { return s.content.String() }

// Reasoning returns the inline reasoning so far.
func (s *ThinkSplitter) Reasoning() string { return s.reasoning.String() }

// InThink reports whether the stream is currently inside a reasoning block.
func (s *ThinkSplitter) InThink() bool { return s.inThink }

// findThinkOpen returns the index of the earliest reasoning open tag in s,
// with that tag and its matching close tag.
func findThinkOpen(s string) (int, string, string) {
	best, open, closeTag := -1, "", ""
	for _, pair := range thinkTags {
		if i := strings.Index(s, pair[0]); i != -1 && (best == -1 || i < best) {
			best, open, closeTag = i, pair[0], pair[1]
		}
	}
	return best, open, closeTag
}

// partialSuffix returns the length of the longest suffix of s that is a
// proper prefix of one of tags (i.e. a tag that may complete in the next chunk).
func partialSuffix(s string, tags ...string) int {
	best := 0
	for _, tag := range tags {
		max := len(tag) - 1
		if max > len(s) {
			max = len(s)
		}
		for k := max; k > best; k-- {
			if strings.HasSuffix(s, tag[:k]) {
				best = k
				break
			}
		}
	}
	return best
}
