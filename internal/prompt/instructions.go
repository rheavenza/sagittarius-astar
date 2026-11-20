package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// InstructionFileNames are the project instruction files aig looks for, in
// priority order, in the working directory and the git root.
var InstructionFileNames = []string{
	"AGENTS.md",
	"CLAUDE.md",
	filepath.Join(".aig", "instructions.md"),
}

const (
	maxInstructionFileBytes  = 16 * 1024
	maxInstructionTotalBytes = 48 * 1024
)

// InstructionFile is one loaded instruction file.
type InstructionFile struct {
	Path    string // absolute path
	Content string
	// Global files come from the user's own config dir and need no consent.
	Global bool
}

// FindInstructionFiles returns the absolute paths of existing project
// instruction files in cwd and gitRoot (deduplicated, cwd first).
func FindInstructionFiles(cwd, gitRoot string) []string {
	var out []string
	seen := map[string]bool{}
	for _, dir := range []string{cwd, gitRoot} {
		if dir == "" {
			continue
		}
		for _, name := range InstructionFileNames {
			p := filepath.Join(dir, name)
			abs, err := filepath.Abs(p)
			if err != nil || seen[abs] {
				continue
			}
			if info, err := os.Stat(abs); err == nil && !info.IsDir() {
				seen[abs] = true
				out = append(out, abs)
			}
		}
	}
	return out
}

// GlobalInstructionFile returns the path of the user's global instructions
// (<configDir>/AGENTS.md) if it exists.
func GlobalInstructionFile(configDir string) string {
	p := filepath.Join(configDir, "AGENTS.md")
	if info, err := os.Stat(p); err == nil && !info.IsDir() {
		return p
	}
	return ""
}

// LoadInstructionFile reads a file, truncating very large ones.
func LoadInstructionFile(path string, global bool) (InstructionFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return InstructionFile{}, err
	}
	content := string(data)
	if len(content) > maxInstructionFileBytes {
		cut := maxInstructionFileBytes
		for cut > 0 && !utf8.RuneStart(content[cut]) {
			cut--
		}
		content = content[:cut] + "\n…(truncated)"
	}
	return InstructionFile{Path: path, Content: content, Global: global}, nil
}

// RenderInstructions formats loaded instruction files for the system prompt.
func RenderInstructions(files []InstructionFile) string {
	if len(files) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Project instructions\n")
	b.WriteString("These files were provided by the user or the project. Follow them unless they conflict with safety rules or the user's direct requests in this conversation.\n")
	total := 0
	for _, f := range files {
		if total+len(f.Content) > maxInstructionTotalBytes {
			b.WriteString("\n(further instruction files omitted: size limit reached)\n")
			break
		}
		total += len(f.Content)
		label := f.Path
		if f.Global {
			label += " (user's global instructions)"
		}
		b.WriteString("\n## " + label + "\n")
		b.WriteString(strings.TrimSpace(f.Content) + "\n")
	}
	return b.String()
}

// BuildContextBlock joins the environment and instruction sections.
func BuildContextBlock(env Env, files []InstructionFile) string {
	parts := []string{env.Render()}
	if s := RenderInstructions(files); s != "" {
		parts = append(parts, s)
	}
	return strings.Join(parts, "\n")
}
