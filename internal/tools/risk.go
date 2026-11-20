package tools

import "regexp"

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
	"read_file":      RiskRead,
	"list_directory": RiskRead,
	"search_regex":   RiskRead,
	"find_files":     RiskRead,
	"map_workspace":  RiskRead,
	"web_search":     RiskNetwork,
	"web_fetch":      RiskNetwork,
	"web_scrape":     RiskNetwork,
	"write_file":     RiskWrite,
	"edit_file":      RiskWrite,
	"multi_edit":     RiskWrite,
	"run_command":    RiskExec,
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
}

// DangerousCommand reports whether command is destructive enough that it
// must be confirmed even in auto mode, and why.
func DangerousCommand(command string) (bool, string) {
	for _, p := range dangerousPatterns {
		if p.re.MatchString(command) {
			return true, p.reason
		}
	}
	return false, ""
}
