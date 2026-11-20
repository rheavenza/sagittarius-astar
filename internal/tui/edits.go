package tui

import (
	"fmt"
	"regexp"
	"strings"
)

// editOp is one search/replace operation for edit_file / multi_edit.
type editOp struct {
	Old        string
	New        string
	ReplaceAll bool
}

// reLineNumberPrefix matches the "     42\t" prefix read_file adds, which
// models sometimes copy into old_content.
var reLineNumberPrefix = regexp.MustCompile(`^\s*\d+\t`)

// applyEdit applies op to content. Matching is attempted in order:
//
//  1. exact text (must be unique unless ReplaceAll);
//  2. exact text after stripping copied read_file line-number prefixes;
//  3. line-by-line ignoring leading/trailing whitespace (must be unique);
//     the replacement is re-indented to the file's actual indentation.
//
// It returns the new content and a note when a fallback was used.
func applyEdit(content string, op editOp) (string, string, error) {
	if op.Old == "" {
		return "", "", fmt.Errorf("old_content must not be empty (use write_file to create or overwrite a file)")
	}
	if op.Old == op.New {
		return "", "", fmt.Errorf("old_content and new_content are identical")
	}

	if out, ok, err := replaceExact(content, op); ok || err != nil {
		return out, "", err
	}

	if strippedOld, changed := stripLineNumbers(op.Old); changed {
		strippedNew, _ := stripLineNumbers(op.New)
		alt := editOp{Old: strippedOld, New: strippedNew, ReplaceAll: op.ReplaceAll}
		if out, ok, err := replaceExact(content, alt); ok || err != nil {
			return out, "removed read_file line-number prefixes from old_content", err
		}
		op = alt
	}

	if out, ok, err := replaceFuzzy(content, op); ok || err != nil {
		return out, "matched ignoring whitespace/indentation differences", err
	}

	return "", "", fmt.Errorf("old_content not found%s — re-read the file and copy the exact text", hintForMissing(content, op.Old))
}

// replaceExact reports ok=false (and no error) when there is no exact match.
func replaceExact(content string, op editOp) (string, bool, error) {
	count := strings.Count(content, op.Old)
	switch {
	case count == 0:
		return "", false, nil
	case count == 1 || op.ReplaceAll:
		return strings.ReplaceAll(content, op.Old, op.New), true, nil
	default:
		return "", false, fmt.Errorf("old_content matches %d locations — add more surrounding context to make it unique, or set replace_all", count)
	}
}

// stripLineNumbers removes "N\t" prefixes if every non-blank line has one.
func stripLineNumbers(s string) (string, bool) {
	lines := strings.Split(s, "\n")
	nonBlank := 0
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		nonBlank++
		if !reLineNumberPrefix.MatchString(l) {
			return s, false
		}
	}
	if nonBlank == 0 {
		return s, false
	}
	for i, l := range lines {
		lines[i] = reLineNumberPrefix.ReplaceAllString(l, "")
	}
	return strings.Join(lines, "\n"), true
}

// splitLinesTrimEnd splits s into lines, dropping one trailing empty line
// produced by a final newline.
func splitLinesTrimEnd(s string) []string {
	lines := strings.Split(s, "\n")
	if len(lines) > 1 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func leadingWhitespace(s string) string {
	return s[:len(s)-len(strings.TrimLeft(s, " \t"))]
}

func firstNonBlank(lines []string) string {
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			return l
		}
	}
	return ""
}

// replaceFuzzy finds a unique window of file lines equal to old's lines
// after trimming whitespace, and replaces it with new (re-indented).
func replaceFuzzy(content string, op editOp) (string, bool, error) {
	fileLines := strings.Split(content, "\n")
	oldLines := splitLinesTrimEnd(op.Old)
	if len(oldLines) == 0 || len(oldLines) > len(fileLines) {
		return "", false, nil
	}

	trimmedOld := make([]string, len(oldLines))
	for i, l := range oldLines {
		trimmedOld[i] = strings.TrimSpace(l)
	}

	var hits []int
	for i := 0; i+len(oldLines) <= len(fileLines); i++ {
		ok := true
		for j := range oldLines {
			if strings.TrimSpace(fileLines[i+j]) != trimmedOld[j] {
				ok = false
				break
			}
		}
		if ok {
			hits = append(hits, i)
			if !op.ReplaceAll && len(hits) > 1 {
				return "", false, fmt.Errorf("old_content matches %d locations (ignoring whitespace) — add more surrounding context", len(hits))
			}
		}
	}
	if len(hits) == 0 {
		return "", false, nil
	}

	// Re-indent the replacement: map old_content's indentation onto the
	// file's real indentation at the match.
	oldIndent := leadingWhitespace(firstNonBlank(oldLines))
	newLines := splitLinesTrimEnd(op.New)
	if op.New == "" {
		newLines = nil
	}

	// Apply from the last hit backwards so earlier indices stay valid.
	for k := len(hits) - 1; k >= 0; k-- {
		start := hits[k]
		fileIndent := leadingWhitespace(firstNonBlank(fileLines[start : start+len(oldLines)]))
		repl := make([]string, len(newLines))
		for i, l := range newLines {
			if strings.TrimSpace(l) != "" && strings.HasPrefix(l, oldIndent) {
				repl[i] = fileIndent + l[len(oldIndent):]
			} else {
				repl[i] = l
			}
		}
		tail := append([]string{}, fileLines[start+len(oldLines):]...)
		fileLines = append(append(fileLines[:start], repl...), tail...)
	}
	return strings.Join(fileLines, "\n"), true, nil
}

// hintForMissing points at lines resembling old's first line.
func hintForMissing(content, old string) string {
	first := strings.TrimSpace(firstNonBlank(splitLinesTrimEnd(old)))
	if first == "" {
		return ""
	}
	var lines []string
	for i, l := range strings.Split(content, "\n") {
		if strings.TrimSpace(l) == first {
			lines = append(lines, fmt.Sprint(i+1))
			if len(lines) == 3 {
				break
			}
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return fmt.Sprintf(" (its first line appears at line %s, but the following lines differ)", strings.Join(lines, ", "))
}

// parseEditOps reads multi_edit's "edits" argument.
func parseEditOps(raw any) ([]editOp, error) {
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return nil, fmt.Errorf("multi_edit: 'edits' must be a non-empty array")
	}
	ops := make([]editOp, 0, len(list))
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("multi_edit: edit #%d is not an object", i+1)
		}
		old, ok1 := m["old_content"].(string)
		repl, ok2 := m["new_content"].(string)
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("multi_edit: edit #%d needs string old_content and new_content", i+1)
		}
		all, _ := m["replace_all"].(bool)
		ops = append(ops, editOp{Old: old, New: repl, ReplaceAll: all})
	}
	return ops, nil
}
