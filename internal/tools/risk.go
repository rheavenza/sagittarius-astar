package tools

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
