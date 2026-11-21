package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/clitorhea/sagittarius-astar.git/internal/llm"
	"github.com/clitorhea/sagittarius-astar.git/internal/logger"
	"github.com/clitorhea/sagittarius-astar.git/internal/sandbox"
	"github.com/clitorhea/sagittarius-astar.git/internal/tools"
	"github.com/clitorhea/sagittarius-astar.git/internal/workspace"
)

// outcome is the result of executing one tool call.
type outcome struct {
	result string
	err    error
	denied bool
}

// content is what goes into the tool message for the model.
func (o outcome) content() string {
	if o.err != nil {
		return "Error: " + o.err.Error()
	}
	return o.result
}

func deniedOutcome(msg string) outcome { return outcome{result: msg, denied: true} }

// deniedBy is deniedOutcome with the front-end's reason, if it gave one.
func deniedBy(rep ApprovalReply, msg string) outcome {
	if rep.Reason != "" {
		msg = rep.Reason
	}
	return deniedOutcome(msg)
}

const deniedToolMsg = "The user denied this tool call. Do not retry it without asking the user."

// ask emits an approval request and waits for the answer (or cancellation).
func (t *turn) ask(ctx context.Context, req *ApprovalRequest) (ApprovalReply, bool) {
	if t.sub {
		return ApprovalReply{}, false // subagents can't reach the user
	}
	t.emit(ApprovalEvent{Req: req})
	select {
	case rep := <-req.reply:
		return rep, rep.Approved
	case <-ctx.Done():
		return ApprovalReply{}, false
	}
}

// execute applies the approval policy and runs one tool call.
//
// Policy by permission mode (see Mode):
//   - file writes ask, except inside the workspace in accept-edits/auto mode;
//   - run_command and run_checks ask, except in auto mode (destructive
//     commands still ask);
//   - MCP tools ask unless their server is trusted or in auto mode;
//   - web_fetch and reads outside the workspace ask unless in auto mode;
//   - other read-only tools run directly.
func (t *turn) execute(ctx context.Context, call llm.ToolCall) outcome {
	a := t.a
	auto := a.AutoApprove()

	if t.planMode {
		risk, known := tools.RiskOf(call.Name)
		if !known || risk >= tools.RiskWrite {
			return deniedOutcome("Plan mode is active: only read-only tools may be used. Finish investigating and present your plan; changes are made after the user approves it with /go.")
		}
	}

	if a.mcp != nil && a.mcp.IsMCPTool(call.Name) {
		asked := !auto && !a.mcp.IsTrusted(call.Name)
		if asked {
			req := newRequest(&ApprovalRequest{Kind: ApproveTool, Call: call, Title: "MCP tool " + call.Name, Risk: tools.RiskExternal})
			if rep, ok := t.ask(ctx, req); !ok {
				return deniedBy(rep, deniedToolMsg)
			}
		}
		t.ensureCheckpoint()
		t.emit(ToolStartEvent{Call: call, Auto: !asked})
		res, err := a.mcp.Call(ctx, call.Name, call.Args)
		return outcome{result: res, err: err}
	}

	risk, known := tools.RiskOf(call.Name)
	if !known {
		return outcome{err: fmt.Errorf("unknown tool: %s", call.Name)}
	}

	if t.sub {
		if risk != tools.RiskRead || call.Name == "explore" {
			return deniedOutcome("Explore subagents may only use local read-only tools.")
		}
		if need, reason := tools.NeedsApproval(call, a.workspaceRoot); need {
			return deniedOutcome("Explore subagents cannot ask the user for approval (" + reason + "). Stay inside the workspace.")
		}
	}
	switch call.Name {
	case "explore":
		return t.explore(ctx, call)
	case "read_output":
		t.emit(ToolStartEvent{Call: call, Auto: true})
		return t.a.readOutput(call)
	}

	switch risk {
	case tools.RiskExec:
		if call.Name == "run_checks" {
			return t.runChecks(ctx, call, auto)
		}
		return t.runCommand(ctx, call, auto)
	case tools.RiskWrite:
		return t.write(ctx, call)
	}

	asked := false
	if !auto {
		if need, reason := tools.NeedsApproval(call, a.workspaceRoot); need {
			asked = true
			req := newRequest(&ApprovalRequest{Kind: ApproveTool, Call: call, Title: call.Name + " — " + reason, Risk: risk})
			if rep, ok := t.ask(ctx, req); !ok {
				return deniedBy(rep, deniedToolMsg)
			}
		}
	}
	t.emit(ToolStartEvent{Call: call, Auto: !asked})
	res, _, err := tools.Execute(call)
	return outcome{result: res, err: err}
}

// write prepares a file change, asks for approval with a diff, commits it and
// runs the post-edit check.
func (t *turn) write(ctx context.Context, call llm.ToolCall) outcome {
	_, pw, err := tools.Execute(call)
	if pw == nil {
		if err == nil || errors.Is(err, tools.ErrWriteConfirmationRequired) {
			err = fmt.Errorf("%s: failed to prepare write", call.Name)
		}
		return outcome{err: err}
	}

	mode := t.a.Mode()
	auto := (mode == ModeAcceptEdits || mode == ModeAuto) &&
		t.a.workspaceRoot != "" && workspace.IsInside(t.a.workspaceRoot, pw.Path)
	if !auto {
		title := call.Name + ": " + pw.Path
		if mode != ModeDefault {
			title += " (outside the workspace)"
		}
		req := newRequest(&ApprovalRequest{Kind: ApproveWrite, Call: call, Title: title, Write: pw, Risk: tools.RiskWrite})
		if rep, ok := t.ask(ctx, req); !ok {
			return deniedBy(rep, "The user rejected this file change. Do not retry it without asking the user.")
		}
	}

	t.ensureCheckpoint()
	if cp := t.a.checkpoints; cp != nil {
		cp.RecordWrite(pw.Path, pw.Original, pw.Existed)
	}
	t.emit(ToolStartEvent{Call: call, Auto: auto})
	if err := tools.CommitWrite(*pw); err != nil {
		return outcome{err: err}
	}
	result := "Successfully wrote " + pw.Path
	if len(pw.Notes) > 0 {
		result += " (" + strings.Join(pw.Notes, "; ") + ")"
	}
	if check := tools.PostEditCheck(ctx, pw.Path, t.a.postEditChecks); check != "" {
		result += "\n\n" + check
	}
	return outcome{result: result}
}

// runCommand handles approval (with editing), the sudo password fallback,
// stdin wiring and execution for run_command.
func (t *turn) runCommand(ctx context.Context, call llm.ToolCall, auto bool) outcome {
	a := t.a
	original, _ := call.Args["command"].(string)
	if strings.TrimSpace(original) == "" {
		return outcome{err: fmt.Errorf("run_command: missing 'command' argument")}
	}
	command := original

	title := "run_command"
	if auto {
		if bad, reason := tools.DangerousCommand(command); bad {
			auto = false
			title = "⚠ destructive command (" + reason + ") — confirmation required even in auto mode"
		}
	}
	if !auto {
		req := newRequest(&ApprovalRequest{Kind: ApproveCommand, Call: call, Title: title, Command: command, Risk: tools.RiskExec})
		rep, ok := t.ask(ctx, req)
		if !ok {
			return deniedBy(rep, "Command skipped by user.")
		}
		if c := strings.TrimSpace(rep.Command); c != "" {
			command = c
		}
	}

	password := ""
	if sandbox.NeedsPassword(command) {
		password = a.cachedSudoPassword()
		if password == "" {
			req := newRequest(&ApprovalRequest{Kind: ApproveSudoPassword, Call: call, Title: "sudo password", Command: command, Risk: tools.RiskExec})
			rep, ok := t.ask(ctx, req)
			if !ok {
				return deniedBy(rep, "Command cancelled by user (sudo password not provided).")
			}
			password = rep.Password
			a.setSudoPassword(password)
		}
	}

	// Copy args so an edited command doesn't rewrite the call stored in history.
	run := call
	run.Args = make(map[string]any, len(call.Args))
	for k, v := range call.Args {
		run.Args[k] = v
	}
	run.Args["command"] = command

	t.ensureCheckpoint()
	pipeR, pipeW := io.Pipe()
	defer pipeW.Close()
	t.emit(ToolStartEvent{Call: run, Command: command, Stdin: pipeW, Auto: auto})
	if password != "" {
		// io.Pipe writes block until read; never block the agent on it.
		go func() { _, _ = fmt.Fprintln(pipeW, password) }()
	}

	out, err := tools.RunCommand(ctx, run, password, pipeR)
	_ = pipeR.Close()

	if password != "" && sudoAuthFailed(out, err) {
		a.setSudoPassword("")
		t.emit(NoticeEvent{Level: NoticeWarn, Text: "Sudo authentication failed; cached password cleared."})
		logger.L.Warn("agent: sudo authentication failed, password cache cleared")
	}
	if err == nil && command != original {
		out = fmt.Sprintf("Note: the user edited the command before running it. Executed:\n%s\n\n%s", command, out)
	}
	return outcome{result: out, err: err}
}

func sudoAuthFailed(out string, err error) bool {
	s := strings.ToLower(out)
	if err != nil {
		s += " " + strings.ToLower(err.Error())
	}
	return strings.Contains(s, "incorrect password") || strings.Contains(s, "sorry, try again")
}

// runChecks runs the project's configured checks (or the named subset) after
// one approval covering all of them.
func (t *turn) runChecks(ctx context.Context, call llm.ToolCall, auto bool) outcome {
	pc := t.a.projectChecks
	if len(pc.Checks) == 0 {
		return outcome{err: fmt.Errorf("run_checks: this project has no checks configured (.aig/config.json)")}
	}
	checks, err := pc.Select(tools.CheckNames(call))
	if err != nil {
		return outcome{err: fmt.Errorf("run_checks: %w", err)}
	}

	var lines []string
	reason := ""
	for _, c := range checks {
		lines = append(lines, c.Name+": "+c.Command)
		if bad, why := tools.DangerousCommand(c.Command); bad && reason == "" {
			reason = c.Name + " " + why
		}
	}
	title := "run_checks in " + pc.Root
	if auto && reason != "" {
		auto = false
		title = "⚠ destructive check (" + reason + ") — confirmation required even in auto mode"
	}
	if !auto {
		req := newRequest(&ApprovalRequest{Kind: ApproveTool, Call: call, Title: title, Command: strings.Join(lines, "\n"), Risk: tools.RiskExec})
		if rep, ok := t.ask(ctx, req); !ok {
			return deniedBy(rep, "Checks skipped by user.")
		}
	}

	t.ensureCheckpoint()
	t.emit(ToolStartEvent{Call: call, Command: strings.Join(lines, " · "), Auto: auto})
	results := tools.RunProjectChecks(ctx, pc, checks)
	return outcome{result: tools.FormatCheckResults(results, len(checks))}
}
