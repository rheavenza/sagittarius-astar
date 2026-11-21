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

// Check is a named project check shown in the "Definition of done" section.
type Check struct {
	Name    string
	Command string
}

// RenderChecks formats the project's checks as its definition of done.
func RenderChecks(source string, checks []Check) string {
	if len(checks) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Definition of done\n")
	b.WriteString("This project defines checks in " + source + ". A code change is not finished until they pass: run them with the run_checks tool (all of them, or the relevant ones by name), fix any failures, and say in your answer which checks you ran and their result. Skip this only for changes that cannot affect them (docs, comments) and say so.\n")
	for _, c := range checks {
		b.WriteString("- " + c.Name + ": `" + c.Command + "`\n")
	}
	return b.String()
}

// RenderRepoMap wraps a rendered repository map for the system prompt.
func RenderRepoMap(root, repoMap string) string {
	if strings.TrimSpace(repoMap) == "" {
		return ""
	}
	return "# Repository map\n" +
		"The most-referenced definitions in " + root + ", computed at session start (it does not update as files change). " +
		"Use it to decide where to look. Call repo_map (dir, focus) for more symbols with line numbers, and read_file before editing.\n" +
		"```\n" + strings.TrimSpace(repoMap) + "\n```\n"
}

// BuildContextBlock joins the environment and the optional sections
// (instructions, then extra sections such as checks and the repo map; ""
// entries are skipped).
func BuildContextBlock(env Env, files []InstructionFile, extra ...string) string {
	parts := []string{env.Render()}
	if s := RenderInstructions(files); s != "" {
		parts = append(parts, s)
	}
	for _, s := range extra {
		if s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n")
}
