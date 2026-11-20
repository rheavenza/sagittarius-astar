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

const deniedToolMsg = "The user denied this tool call. Do not retry it without asking the user."

// ask emits an approval request and waits for the answer (or cancellation).
func (t *turn) ask(ctx context.Context, req *ApprovalRequest) (ApprovalReply, bool) {
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
// Policy:
//   - file writes always ask (diff preview);
//   - run_command asks unless auto-approve is on;
//   - MCP tools ask unless their server is trusted or auto-approve is on;
//   - web_fetch and reads outside the workspace ask unless auto-approve is on;
//   - other read-only tools run directly.
func (t *turn) execute(ctx context.Context, call llm.ToolCall) outcome {
	a := t.a
	auto := a.AutoApprove()

	if a.mcp != nil && a.mcp.IsMCPTool(call.Name) {
		asked := !auto && !a.mcp.IsTrusted(call.Name)
		if asked {
			req := newRequest(&ApprovalRequest{Kind: ApproveTool, Call: call, Title: "MCP tool " + call.Name, Risk: tools.RiskExternal})
			if _, ok := t.ask(ctx, req); !ok {
				return deniedOutcome(deniedToolMsg)
			}
		}
		t.emit(ToolStartEvent{Call: call, Auto: !asked})
		res, err := a.mcp.Call(ctx, call.Name, call.Args)
		return outcome{result: res, err: err}
	}

	risk, known := tools.RiskOf(call.Name)
	if !known {
		return outcome{err: fmt.Errorf("unknown tool: %s", call.Name)}
	}

	switch risk {
	case tools.RiskExec:
		return t.runCommand(ctx, call, auto)
	case tools.RiskWrite:
		return t.write(ctx, call)
	}

	asked := false
	if !auto {
		if need, reason := tools.NeedsApproval(call, a.workspaceRoot); need {
			asked = true
			req := newRequest(&ApprovalRequest{Kind: ApproveTool, Call: call, Title: call.Name + " — " + reason, Risk: risk})
			if _, ok := t.ask(ctx, req); !ok {
				return deniedOutcome(deniedToolMsg)
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

	req := newRequest(&ApprovalRequest{Kind: ApproveWrite, Call: call, Title: call.Name + ": " + pw.Path, Write: pw, Risk: tools.RiskWrite})
	if _, ok := t.ask(ctx, req); !ok {
		return deniedOutcome("The user rejected this file change. Do not retry it without asking the user.")
	}

	t.emit(ToolStartEvent{Call: call})
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

	if !auto {
		req := newRequest(&ApprovalRequest{Kind: ApproveCommand, Call: call, Title: "run_command", Command: command, Risk: tools.RiskExec})
		rep, ok := t.ask(ctx, req)
		if !ok {
			return deniedOutcome("Command skipped by user.")
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
				return deniedOutcome("Command cancelled by user (sudo password not provided).")
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
