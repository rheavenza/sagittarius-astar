package tools

import (
	"regexp"
	"strings"
)

// Risk classifies what a tool can do. The agent's approval policy is based on
// it, for native and MCP tools alike.
type Risk int

const (
	// RiskRead only reads local state (files, directory listings).
	RiskRead Risk = iota
	// RiskNetwork talks to the internet (search, fetch).
	RiskNetwork
	// RiskWrite modifies files.
	RiskWrite
	// RiskExec runs arbitrary commands.
	RiskExec
	// RiskExternal is an MCP tool: its behaviour is unknown to aig.
	RiskExternal
)

func (r Risk) String() string {
	switch r {
	case RiskRead:
		return "read"
	case RiskNetwork:
		return "network"
	case RiskWrite:
		return "write"
	case RiskExec:
		return "exec"
	default:
		return "external"
	}
}

// nativeRisk maps each native tool to its risk level.
var nativeRisk = map[string]Risk{
	"read_file":        RiskRead,
	"list_directory":   RiskRead,
	"search_regex":     RiskRead,
	"find_files":       RiskRead,
	"map_workspace":    RiskRead,
	"repo_map":         RiskRead,
	"project_overview": RiskRead,
	"explore":          RiskRead, // a subagent limited to RiskRead tools
	"read_output":      RiskRead, // run by the agent (its output store)
	// Run by the agent (internal/agent), not tools.Execute. They are not in
	// Definitions, so explore subagents never get them.
	"update_plan":         RiskRead, // the agent's task checklist
	"ask_user":            RiskRead, // a question to the user
	"read_process_output": RiskRead, // output of the agent's background processes
	"kill_process":        RiskExec, // stops a background process (blocked in plan mode)
	"web_search":          RiskNetwork,
	"web_fetch":           RiskNetwork,
	"web_scrape":          RiskNetwork,
	"write_file":          RiskWrite,
	"edit_file":           RiskWrite,
	"multi_edit":          RiskWrite,
	"run_command":         RiskExec,
	"run_checks":          RiskExec,
}

// RiskOf returns the risk of a native tool, and whether the name is a native tool.
func RiskOf(name string) (Risk, bool) {
	r, ok := nativeRisk[name]
	return r, ok
}

// IsWriteTool reports whether name is a native file-mutation tool.
func IsWriteTool(name string) bool {
	r, ok := nativeRisk[name]
	return ok && r == RiskWrite
}

// dangerousPatterns are commands that still require confirmation in auto
// mode: hard or impossible to undo, or affecting more than the workspace.
var dangerousPatterns = []struct {
	re     *regexp.Regexp
	reason string
}{
	{regexp.MustCompile(`\brm\s+(-[a-zA-Z]*[rR][a-zA-Z]*f|-[a-zA-Z]*f[a-zA-Z]*[rR]|-[rR]\s+-f|-f\s+-[rR]|--recursive\b.*--force|--force\b.*--recursive)`), "recursive forced delete"},
	{regexp.MustCompile(`\bmkfs(\.\w+)?\b`), "formats a filesystem"},
	{regexp.MustCompile(`\bdd\b.*\bof=/dev/`), "writes to a block device"},
	{regexp.MustCompile(`>\s*/dev/(sd|nvme|vd|mmcblk)`), "writes to a block device"},
	{regexp.MustCompile(`(^|[;&|(]\s*|\bsudo\s+|\bsystemctl\s+)(shutdown|reboot|poweroff|halt)\b`), "shuts down or reboots"},
	{regexp.MustCompile(`\bgit\s+push\b.*(\s--force\b|\s-f\b|--force-with-lease|\s\+\S)`), "force-pushes"},
	{regexp.MustCompile(`\bgit\s+reset\s+--hard\b`), "discards git changes"},
	{regexp.MustCompile(`\bgit\s+clean\s+-[a-zA-Z]*f`), "deletes untracked files"},
	{regexp.MustCompile(`\bgit\s+(checkout|restore)\s+(--\s+)?\.(\s|$)`), "discards working-tree changes"},
	{regexp.MustCompile(`\brpm-ostree\s+(reset|rebase|uninstall|override|install|deploy|rollback)\b`), "changes the OS deployment"},
	{regexp.MustCompile(`\b(chmod|chown)\s+(-[a-zA-Z]*R|--recursive)`), "recursive permission change"},
	{regexp.MustCompile(`:\(\)\s*\{\s*:\|:&\s*\};:`), "fork bomb"},
	{regexp.MustCompile(`\b(curl|wget)\b[^|]*\|\s*(sudo\s+)?(ba|z|fi)?sh\b`), "pipes a download into a shell"},
	{regexp.MustCompile(`\bsystemctl\s+(disable|mask|stop)\b`), "disables or stops a service"},
	{regexp.MustCompile(`\b(flatpak|dnf|brew)\s+(remove|uninstall|erase)\b`), "uninstalls software"},
	{regexp.MustCompile(`\bfind\b.*\s-(delete|exec(dir)?\s+(sudo\s+)?rm)\b`), "deletes the files find matches"},
	{regexp.MustCompile(`\bgit\s+branch\b.*\s(-[a-zA-Z]*D\b|--delete\b.*--force\b|--force\b.*--delete\b)`), "force-deletes a git branch"},
	{regexp.MustCompile(`\bgit\s+stash\s+(drop|clear)\b`), "discards stashed changes"},
	{regexp.MustCompile(`\b(shred|wipefs)\b`), "destroys data"},
	{regexp.MustCompile(`\b(docker|podman)\s+((system|volume|image|container)\s+prune|volume\s+rm)\b`), "deletes containers, images or volumes"},
	{regexp.MustCompile(`\bdistrobox\s+(rm|remove)\b`), "deletes a container"},
}

// reCmdSep splits a command line into simple commands (roughly: it doesn't
// understand quoting, which only makes the check stricter).
var reCmdSep = regexp.MustCompile(`&&|\|\||[;|&()\n]`)

// riskyRecursiveRm reports a recursive rm (without -f, which the patterns
// above catch) that reaches outside the working tree: an absolute or home
// path, a parent directory, the whole current directory, a bare glob, or
// targets fed in by xargs. `rm -r build` stays allowed: deleting a
// directory inside the workspace is ordinary cleanup.
func riskyRecursiveRm(command string) bool {
	for _, seg := range reCmdSep.Split(command, -1) {
		f := strings.Fields(seg)
		i, xargs := commandWord(f)
		if i < 0 {
			continue
		}
		if name := f[i]; name != "rm" && !strings.HasSuffix(name, "/rm") && name != `\rm` {
			continue
		}
		recursive := false
		var targets []string
		for _, a := range f[i+1:] {
			switch {
			case a == "--recursive":
				recursive = true
			case a == "--" || strings.HasPrefix(a, "--"):
			case strings.HasPrefix(a, "-") && len(a) > 1:
				if strings.ContainsAny(a[1:], "rR") {
					recursive = true
				}
			default:
				targets = append(targets, strings.Trim(a, `"'`))
			}
		}
		if !recursive {
			continue
		}
		if xargs {
			return true
		}
		for _, t := range targets {
			if strings.HasPrefix(t, "/") || strings.HasPrefix(t, "~") ||
				strings.HasPrefix(t, "$HOME") || strings.HasPrefix(t, "${HOME}") ||
				t == ".." || strings.HasPrefix(t, "../") || strings.Contains(t, "/../") || strings.HasSuffix(t, "/..") ||
				t == "." || t == "./" || t == "*" || t == "./*" || t == ".*" {
				return true
			}
		}
	}
	return false
}

// commandWord returns the index of the program in a simple command's words,
// skipping wrappers (sudo, env, xargs, …), their options and VAR=value
// prefixes; xargs reports whether the program's arguments come from xargs.
// i is -1 when there is no program word.
func commandWord(f []string) (i int, xargs bool) {
	for i = 0; i < len(f); i++ {
		w := f[i]
		switch {
		case w == "sudo" || w == "command" || w == "env" || w == "nice" || w == "time":
		case w == "xargs":
			xargs = true
		case i > 0 && strings.HasPrefix(w, "-"):
			// an option of the wrapper before it (sudo -E, xargs -0, …)
		case !strings.HasPrefix(w, "-") && strings.Contains(w, "="):
			// VAR=value prefix
		default:
			return i, xargs
		}
	}
	return -1, false
}

// DangerousCommand reports whether command is destructive enough that it
// must be confirmed even in auto mode, and why.
func DangerousCommand(command string) (bool, string) {
	for _, p := range dangerousPatterns {
		if p.re.MatchString(command) {
			return true, p.reason
		}
	}
	if riskyRecursiveRm(command) {
		return true, "recursive delete outside the working tree"
	}
	return false, ""
}
