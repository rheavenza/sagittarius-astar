package repomap

import (
	"regexp"
	"strings"
	"unicode"
)

// linePattern matches one kind of definition on a single line; name is the
// index of the capture group holding the defined identifier.
type linePattern struct {
	re   *regexp.Regexp
	kind string
	name int
}

var linePatterns = map[string][]linePattern{
	"python": {
		{regexp.MustCompile(`^(\s*)class\s+([A-Za-z_]\w*)`), "class", 2},
		{regexp.MustCompile(`^(\s*)(?:async\s+)?def\s+([A-Za-z_]\w*)\s*\(`), "func", 2},
	},
	"js": jsPatterns,
	"ts": append([]linePattern{
		{regexp.MustCompile(`^(\s*)(?:export\s+)?(?:declare\s+)?interface\s+([A-Za-z_$][\w$]*)`), "interface", 2},
		{regexp.MustCompile(`^(\s*)(?:export\s+)?(?:declare\s+)?type\s+([A-Za-z_$][\w$]*)\s*(?:<[^=]*>)?\s*=`), "type", 2},
		{regexp.MustCompile(`^(\s*)(?:export\s+)?(?:declare\s+)?(?:const\s+)?enum\s+([A-Za-z_$][\w$]*)`), "type", 2},
	}, jsPatterns...),
	"rust": {
		{regexp.MustCompile(`^(\s*)(?:pub(?:\([^)]*\))?\s+)?(?:const\s+)?(?:async\s+)?(?:unsafe\s+)?(?:extern\s+"[^"]*"\s+)?fn\s+([A-Za-z_]\w*)`), "func", 2},
		{regexp.MustCompile(`^(\s*)(?:pub(?:\([^)]*\))?\s+)?(?:struct|enum|union)\s+([A-Za-z_]\w*)`), "struct", 2},
		{regexp.MustCompile(`^(\s*)(?:pub(?:\([^)]*\))?\s+)?(?:unsafe\s+)?trait\s+([A-Za-z_]\w*)`), "trait", 2},
		{regexp.MustCompile(`^(\s*)(?:pub(?:\([^)]*\))?\s+)?type\s+([A-Za-z_]\w*)`), "type", 2},
		{regexp.MustCompile(`^(\s*)impl(?:<[^>]*>)?\s+(?:[\w:<>, ]+\s+for\s+)?([A-Za-z_]\w*)`), "impl", 2},
	},
	"shell": {
		{regexp.MustCompile(`^(\s*)(?:function\s+)?([A-Za-z_][\w-]*)\s*\(\)\s*\{?`), "func", 2},
		{regexp.MustCompile(`^(\s*)function\s+([A-Za-z_][\w-]*)`), "func", 2},
	},
}

var jsPatterns = []linePattern{
	{regexp.MustCompile(`^(\s*)(?:export\s+)?(?:default\s+)?(?:abstract\s+)?class\s+([A-Za-z_$][\w$]*)`), "class", 2},
	{regexp.MustCompile(`^(\s*)(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s*\*?\s*([A-Za-z_$][\w$]*)\s*[(<]`), "func", 2},
	{regexp.MustCompile(`^(\s*)export\s+(?:const|let|var)\s+([A-Za-z_$][\w$]*)`), "const", 2},
	// Top-level arrow functions only; indented ones are local closures.
	{regexp.MustCompile(`^()(?:const|let)\s+([A-Za-z_$][\w$]*)\s*=\s*(?:async\s+)?(?:\([^)]*\)|[A-Za-z_$][\w$]*)\s*=>`), "func", 2},
	// Class methods: indented `name(args) {`, excluding control flow.
	{regexp.MustCompile(`^(\s+)(?:public\s+|private\s+|protected\s+|static\s+|async\s+|readonly\s+|get\s+|set\s+)*([A-Za-z_$][\w$]*)\s*(?:<[^>]*>)?\s*\([^;]*\)\s*(?::\s*[^{;]+)?\{\s*$`), "method", 2},
}

var jsKeywords = map[string]bool{
	"if": true, "for": true, "while": true, "switch": true, "catch": true, "function": true,
	"return": true, "with": true, "else": true, "do": true, "try": true, "constructor": false,
}

// parseLines extracts definitions with per-language line patterns.
func parseLines(lang, src string) []Symbol {
	pats := linePatterns[lang]
	if len(pats) == 0 {
		return nil
	}
	var out []Symbol
	for i, line := range strings.Split(src, "\n") {
		if len(line) > 1000 {
			continue
		}
		for _, p := range pats {
			mm := p.re.FindStringSubmatch(line)
			if mm == nil {
				continue
			}
			name := mm[p.name]
			if p.kind == "method" && jsKeywords[name] {
				break
			}
			indent := len(mm[1])
			kind := p.kind
			if lang == "python" && kind == "func" && indent > 0 {
				kind = "method"
			}
			sig := strings.TrimSpace(line)
			sig = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(strings.TrimSuffix(sig, "{")), ":"), " =>")
			sig = clip(sig)
			if indent > 0 {
				sig = "  " + sig
			}
			out = append(out, Symbol{
				Kind:     kind,
				Names:    []string{name},
				Sig:      sig,
				Line:     i + 1,
				Exported: exportedIn(lang, name, line),
			})
			break
		}
	}
	return out
}

func exportedIn(lang, name, line string) bool {
	switch lang {
	case "python":
		return !strings.HasPrefix(name, "_")
	case "js", "ts":
		return strings.Contains(line, "export ")
	case "rust":
		return strings.Contains(line, "pub ")
	}
	r := []rune(name)
	return len(r) > 0 && unicode.IsUpper(r[0])
}
