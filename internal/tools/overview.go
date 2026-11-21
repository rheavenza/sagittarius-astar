package tools

import (
	"fmt"
	"os"

	"github.com/clitorhea/sagittarius-astar.git/internal/llm"
	"github.com/clitorhea/sagittarius-astar.git/internal/summary"
)

// ProjectOverviewDefinition describes project_overview. It is only offered
// when the project has a /summarize index.
func ProjectOverviewDefinition() llm.Tool {
	return llm.Tool{
		Name: "project_overview",
		Description: "Read the project's cached summaries (made by the user with /summarize): with no path, the project overview and a one-line summary of every directory; " +
			"with a directory, its summary plus its subdirectories and files; with a file, that file's summary. Cheap way to learn what a part of the codebase does before reading it. " +
			"Summaries are model-written and can be outdated (changed files are flagged), so verify in the code before editing.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Directory or file, relative to the working directory (default: the whole project).",
				},
			},
		},
	}
}

func projectOverview(call llm.ToolCall) (string, *PendingWrite, error) {
	target, _ := call.Args["path"].(string)
	cwd, err := os.Getwd()
	if err != nil {
		return "", nil, err
	}
	root := summary.RootFor(cwd)
	ix, err := summary.Load(root)
	if err != nil {
		return "", nil, err
	}
	if ix == nil || ix.Overview() == "" {
		return "", nil, fmt.Errorf("project_overview: no summary index for %s yet (the user can create one with /summarize)", root)
	}
	out, err := ix.Lookup(cwd, target)
	return out, nil, err
}
