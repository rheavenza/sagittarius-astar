// Package prompt builds the dynamic context aig appends to the system prompt:
// an environment block (where am I, what OS, which project) and the
// project's instruction files (AGENTS.md, CLAUDE.md, …).
//
// Everything here is computed once per session so the system prompt prefix
// stays stable across requests, which keeps provider prompt caching effective.
package prompt

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Env describes the machine and project aig is running in.
type Env struct {
	Cwd     string
	OS      string // pretty OS name, e.g. "Bazzite 44 (Linux)"
	Ostree  bool   // image-based / immutable root (Fedora Atomic, Bazzite, …)
	Date    string // YYYY-MM-DD
	GitRoot string
	Branch  string
	Stack   []string // detected project markers, e.g. "Go (module x)", "Makefile"
	Tools   []string // useful CLI tools found on PATH
	Shell   string
}

// gitTimeout bounds each git probe so a slow repo can't delay startup.
const gitTimeout = 2 * time.Second

// probeTools are CLI tools worth telling the model about if present.
var probeTools = []string{"git", "rg", "go", "gofmt", "python3", "node", "npm", "cargo", "make", "podman", "distrobox", "flatpak", "rpm-ostree", "ujust"}

// DetectEnv inspects cwd and the host. It never fails; unknown fields stay empty.
func DetectEnv(cwd string) Env {
	e := Env{
		Cwd:   cwd,
		OS:    osName(),
		Date:  time.Now().Format("2006-01-02"),
		Shell: shellDescription(),
	}
	if _, err := os.Stat("/run/ostree-booted"); err == nil {
		e.Ostree = true
	}
	e.GitRoot = gitOutput(cwd, "rev-parse", "--show-toplevel")
	if e.GitRoot != "" {
		e.Branch = gitOutput(cwd, "rev-parse", "--abbrev-ref", "HEAD")
	}
	root := e.GitRoot
	if root == "" {
		root = cwd
	}
	e.Stack = detectStack(root)
	for _, t := range probeTools {
		if _, err := exec.LookPath(t); err == nil {
			e.Tools = append(e.Tools, t)
		}
	}
	return e
}

// Render formats the environment block for the system prompt.
func (e Env) Render() string {
	var b strings.Builder
	b.WriteString("# Environment\n")
	wd := "- Working directory: " + e.Cwd
	if e.GitRoot != "" {
		wd += fmt.Sprintf(" (git repo root: %s, branch: %s)", e.GitRoot, e.Branch)
	} else {
		wd += " (not a git repository)"
	}
	b.WriteString(wd + "\n")
	osLine := "- OS: " + e.OS
	if e.Ostree {
		osLine += " — image-based (ostree): /usr is read-only; install software with flatpak, brew or distrobox; rpm-ostree layering is a last resort and needs a reboot"
	}
	b.WriteString(osLine + "\n")
	b.WriteString("- run_command shell: " + e.Shell + "\n")
	b.WriteString("- Date: " + e.Date + "\n")
	if len(e.Stack) > 0 {
		b.WriteString("- Project: " + strings.Join(e.Stack, ", ") + "\n")
	}
	if len(e.Tools) > 0 {
		b.WriteString("- CLI tools on PATH: " + strings.Join(e.Tools, ", ") + "\n")
	}
	return b.String()
}

func osName() string {
	if runtime.GOOS != "linux" {
		return runtime.GOOS
	}
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return "Linux"
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
			return strings.Trim(v, `"'`) + " (Linux)"
		}
	}
	return "Linux"
}

func shellDescription() string {
	if runtime.GOOS == "windows" {
		return "powershell.exe -NoProfile -NonInteractive -Command"
	}
	return "/bin/bash -c (non-interactive, no TTY)"
}

func gitOutput(dir string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// detectStack looks for well-known project marker files in root.
func detectStack(root string) []string {
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(root, name))
		return err == nil
	}
	var out []string
	if exists("go.mod") {
		label := "Go"
		if mod := goModule(filepath.Join(root, "go.mod")); mod != "" {
			label += " (module " + mod + ")"
		}
		out = append(out, label)
	}
	markers := []struct{ file, label string }{
		{"package.json", "Node.js (package.json)"},
		{"Cargo.toml", "Rust (Cargo)"},
		{"pyproject.toml", "Python (pyproject)"},
		{"requirements.txt", "Python (requirements.txt)"},
		{"CMakeLists.txt", "C/C++ (CMake)"},
		{"pom.xml", "Java (Maven)"},
		{"build.gradle", "Java/Kotlin (Gradle)"},
		{"Makefile", "Makefile"},
		{"flake.nix", "Nix flake"},
		{"Containerfile", "Containerfile"},
		{"Dockerfile", "Dockerfile"},
	}
	for _, m := range markers {
		if exists(m.file) {
			out = append(out, m.label)
		}
	}
	return out
}

func goModule(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
