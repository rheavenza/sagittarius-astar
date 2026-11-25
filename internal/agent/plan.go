package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/clitorhea/sagittarius-astar.git/internal/llm"
)

// The task plan: a short checklist the model keeps with update_plan while it
// works through a multi-step task. Front-ends show it (PlanEvent), and it is
// carried through compaction in the summary message, so the model and the
// user keep the same picture of what is done and what is left.
//
// The plan is not stored separately: it is rebuilt from the history at the
// start of every turn (the latest update_plan call, or the plan block of a
// compaction summary), so sessions, /new, /load and resume just work.

// Plan step statuses.
const (
	PlanPending    = "pending"
	PlanInProgress = "in_progress"
	PlanDone       = "done"
)

const maxPlanSteps = 30

// PlanItem is one step of the plan.
type PlanItem struct {
	Step   string `json:"step"`
	Status string `json:"status"`
}

// PlanEvent reports the plan after update_plan changed it.
type PlanEvent struct {
	Items       []PlanItem
	Explanation string // why the plan changed, if the model said
}

func (PlanEvent) agentEvent() {}

type planState struct {
	mu    sync.Mutex
	items []PlanItem
}

func (p *planState) get() []PlanItem {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]PlanItem(nil), p.items...)
}

func (p *planState) set(items []PlanItem) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.items = append([]PlanItem(nil), items...)
}

// Plan returns the current plan (empty when the model hasn't made one).
func (a *Agent) Plan() []PlanItem { return a.plan.get() }

func updatePlanDefinition() llm.Tool {
	return llm.Tool{
		Name: "update_plan",
		Description: "Keep a short checklist for a multi-step task (3 or more steps), shown to the user as you work. " +
			"Send the whole list every time: it replaces the previous one. Mark exactly one step in_progress while you work on it, " +
			"and mark a step done as soon as it is finished, before starting the next. Add, remove or reword steps when the task changes. " +
			"Skip it for simple questions and one-step changes.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"items": map[string]any{
					"type":        "array",
					"description": "All steps, in order.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"step":   map[string]any{"type": "string", "description": "A short imperative step, e.g. \"Add retry to the OpenAI provider\""},
							"status": map[string]any{"type": "string", "enum": []string{PlanPending, PlanInProgress, PlanDone}},
						},
						"required": []string{"step", "status"},
					},
				},
				"explanation": map[string]any{
					"type":        "string",
					"description": "Optional: one sentence on why the plan changed (shown to the user).",
				},
			},
			"required": []string{"items"},
		},
	}
}

// parsePlan validates update_plan arguments.
func parsePlan(args map[string]any) ([]PlanItem, error) {
	raw, ok := args["items"].([]any)
	if !ok {
		return nil, fmt.Errorf("update_plan: 'items' must be an array of {step, status}")
	}
	if len(raw) > maxPlanSteps {
		return nil, fmt.Errorf("update_plan: at most %d steps (got %d); merge small steps", maxPlanSteps, len(raw))
	}
	items := make([]PlanItem, 0, len(raw))
	inProgress := 0
	for i, r := range raw {
		m, ok := r.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("update_plan: item %d must be an object with step and status", i+1)
		}
		step, _ := m["step"].(string)
		status, _ := m["status"].(string)
		step = strings.TrimSpace(step)
		status = strings.ToLower(strings.TrimSpace(status))
		if step == "" {
			return nil, fmt.Errorf("update_plan: item %d has an empty step", i+1)
		}
		switch status {
		case "", "todo":
			status = PlanPending
		case "in-progress", "active", "doing":
			status = PlanInProgress
		case "completed", "complete":
			status = PlanDone
		}
		switch status {
		case PlanPending, PlanDone:
		case PlanInProgress:
			inProgress++
		default:
			return nil, fmt.Errorf("update_plan: item %d has status %q; use pending, in_progress or done", i+1, status)
		}
		items = append(items, PlanItem{Step: step, Status: status})
	}
	if inProgress > 1 {
		return nil, fmt.Errorf("update_plan: %d steps are in_progress; mark only the one you are working on", inProgress)
	}
	return items, nil
}

// updatePlan runs an update_plan call.
func (t *turn) updatePlan(call llm.ToolCall) outcome {
	items, err := parsePlan(call.Args)
	if err != nil {
		return outcome{err: err}
	}
	explanation, _ := call.Args["explanation"].(string)
	t.emit(ToolStartEvent{Call: call, Auto: true})
	t.a.plan.set(items)
	t.emit(PlanEvent{Items: append([]PlanItem(nil), items...), Explanation: strings.TrimSpace(explanation)})

	done, total := PlanProgress(items)
	msg := fmt.Sprintf("Plan updated (%d/%d done).", done, total)
	switch {
	case total > 0 && done == total:
		msg += " All steps are done: verify the result and answer the user."
	case !hasStatus(items, PlanInProgress):
		msg += " No step is in_progress: mark the one you start next."
	}
	return outcome{result: msg}
}

// PlanProgress counts done and total steps.
func PlanProgress(items []PlanItem) (done, total int) {
	for _, it := range items {
		if it.Status == PlanDone {
			done++
		}
	}
	return done, len(items)
}

func hasStatus(items []PlanItem, status string) bool {
	for _, it := range items {
		if it.Status == status {
			return true
		}
	}
	return false
}

// RenderPlan formats the plan as a markdown checklist.
func RenderPlan(items []PlanItem) string {
	var b strings.Builder
	for _, it := range items {
		mark := " "
		switch it.Status {
		case PlanDone:
			mark = "x"
		case PlanInProgress:
			mark = ">"
		}
		fmt.Fprintf(&b, "- [%s] %s\n", mark, it.Step)
	}
	return b.String()
}

// planMarker starts the machine-readable copy of the plan inside a
// compaction summary.
const planMarker = "<!-- aig-plan "

// planBlock is the plan section appended to a compaction summary: readable
// for the model, with a JSON copy that planFromHistory reads back.
func planBlock(items []PlanItem) string {
	if len(items) == 0 {
		return ""
	}
	js, _ := json.Marshal(items)
	done, total := PlanProgress(items)
	return fmt.Sprintf("## Current plan (update_plan, %d/%d done)\n%s%s%s -->\n", done, total, RenderPlan(items), planMarker, js)
}

// PlanFromHistory rebuilds the plan: the latest successful update_plan call,
// or the plan block of the latest compaction summary, whichever is newer.
func PlanFromHistory(history []llm.Message) []PlanItem {
	failed := map[string]bool{}
	for i := len(history) - 1; i >= 0; i-- {
		m := history[i]
		switch m.Role {
		case llm.RoleTool:
			if m.ToolName == "update_plan" && strings.HasPrefix(m.Content, "Error:") {
				failed[m.ToolCallID] = true
			}
		case llm.RoleAssistant:
			for j := len(m.ToolCalls) - 1; j >= 0; j-- {
				c := m.ToolCalls[j]
				if c.Name != "update_plan" || failed[c.ID] {
					continue
				}
				if items, err := parsePlan(c.Args); err == nil {
					return items
				}
			}
		}
		if k := strings.LastIndex(m.Content, planMarker); k >= 0 &&
			(strings.HasPrefix(m.Content, CompactedPrefix) || strings.HasPrefix(m.Content, TurnCompactedPrefix)) {
			rest := m.Content[k+len(planMarker):]
			if end := strings.Index(rest, "-->"); end >= 0 {
				var items []PlanItem
				if json.Unmarshal([]byte(strings.TrimSpace(rest[:end])), &items) == nil {
					return items
				}
			}
		}
	}
	return nil
}
