# AI Handover Context & Project Status
**Project Name:** `aig` (sagittarius-a*)
**Type:** Terminal-based AI Assistant (TUI)
**Stack:** Go, Bubble Tea, Lipgloss, Glamour, x/sys/windows

## 🎯 Purpose of this file
This file serves as a compressed "brain dump" for future AI agents working on this project. Read this before starting a task to understand the architecture, recent changes, and known quirks, thereby minimizing token usage on redundant codebase discovery.

---

## 🏗️ Architecture & Core Modules
- **`internal/agent/`**: the agent loop, independent of any UI. `Agent.Run(ctx, history)` returns an event channel (`StreamStartEvent`, `TokenEvent`, `MessageEvent`, `ToolStartEvent`, `ToolResultEvent`, `ApprovalEvent`, `NoticeEvent`, final `DoneEvent`). Approval policy lives in `execute.go`; loop breakers and "every tool_call gets a result" in `agent.go`.
- **`internal/tools/`**: native tool definitions and implementations (read/search/find/edit/multi_edit/write/run/web), edit matching (`edits.go`), post-edit checks (`checks.go`), risk levels (`risk.go`).
- **`internal/tui/`**: Bubble Tea front-end. `turn.go` maps agent events to display and answers `ApprovalRequest`s; `model.go` holds state, slash commands, layout; `providers.go` provider/model switching.
- **`internal/prompt/`**: environment block + instruction files (AGENTS.md…) appended to the system prompt.
- **`internal/llm/`**: providers (Gemini, OpenAI-compatible DeepSeek/NIM), history pruning, `ThinkSplitter` for inline `<think>` reasoning.
- **`internal/sandbox/`**: command execution (process groups, output caps, askpass sudo, stdin wiring).
- **`internal/checkpoint/`**: workspace checkpoints (git snapshot commits under `refs/aig/checkpoints/<session>/`, or an in-memory journal outside git) backing `/undo`, `/redo`, `/diff`.
- **`internal/mcp/`**, **`internal/config/`**, **`internal/session/`**: MCP host, configuration, persistence.

---

## 🕒 Recent Implementations & Fixes

1. **Model Context Protocol (MCP) Host Integration**
   - **Subprocess Manager (`internal/mcp/manager.go`)**: Manages the lifecycle of MCP servers via standard I/O pipes. Supports SIGINT-to-SIGKILL graceful termination within 5 seconds on exit.
   - **Multiplexed Client (`internal/mcp/client.go`)**: Multiplexes concurrent requests asynchronously. Features an enlarged 10 MiB scan buffer for massive directory/file returns.
   - **Tool Registry (`internal/mcp/registry.go`)**: Performs dynamic initialization handshakes, runs `tools/list` on start, translates JSON schemas into LLM-compatible shapes, and routes tool calls to the correct client.
   - **TUI Integration**: Added `mcpToolCallCmd` to run MCP calls in non-blocking Bubble Tea commands while maintaining TUI interactivity.

2. **Advanced Reasoning Streaming & Rendering**
   - **Real-Time Thought Streaming**: Upgraded Gemini and DeepSeek providers to stream reasoning tokens immediately, prefixed with a null byte (`\x00`).
   - **Visual Rendering**: Created a distinct styled display for reasoning (italicized, left-border indented Catppuccin palette).
   - **StatusBar Toggle**: Toggles status text to `● deliberating` when the model is in its thinking cycle.
   - **Plan-Action-Reflection (PAR) Loop**: Dynamically injects planning and reflection instructions into system prompts when tools are active.

3. **Loop Breaker Circuit**
   - **Consecutive Same-Call Breaker**: Intercepts tool execution if the exact same tool and argument hash are called 3 times consecutively. Halts the execution loop and injects a warning context to force model re-evaluation.
   - **Consecutive Error Breaker**: Intercepts and aborts tool loops after 3 consecutive failures to prevent cascading resource waste.

4. **Dynamic API Key Reloading & Windows Selection**
   - **API Key Resolving**: Resolves keys dynamically on each generation run rather than caching on start.
   - **Quick Edit Mode**: Restores Quick Edit Mode on Windows Terminal to enable native selection without breaking the TUI.

5. **Review bug-fix pass (`fix/review-bugfixes`)** — see `FIXES.md` for the full list (protocol correctness, process-group kills, approvals, MCP hardening, tests).

---

## ⚠️ Crucial Rules & Quirks for Future AIs

- **Reasoning Prefix (`\x00`)**: Providers prefix native reasoning tokens with a null byte; `agent.stream` turns them into `TokenEvent{Reasoning: true}`. Keep the prefix in providers.
- **MCP Server Graces**: When adding or managing MCP servers, ensure `Registry.Shutdown()` is called *before* `Manager.Shutdown()` to drain pending requests safely before closing the pipes.
- **History ownership**: during a turn the agent owns the conversation; the TUI appends each `MessageEvent.Msg` to `m.history` in order and must not modify history while `m.events != nil`.
- **Bubble Tea copies the Model by value**: keep `strings.Builder` / splitters behind pointers (`*streamState`).
- **stdin for commands**: never hand `os/exec` a non-`*os.File` reader that stays open — use `sandbox.ExecuteWithStdin`, which wires an OS pipe (`attachStdin`).
- **Checkpoints**: `agent.turn.ensureCheckpoint` runs once per turn right before the first mutation (after approval). Any new mutating tool must call it. Git snapshots must never touch the user's index: always use `GIT_INDEX_FILE` with a temp copy (`gitRepo.tempIndex`).
- **Plan mode** is snapshotted per turn (`turn.planMode`); it filters tool definitions to read/network risk and denies everything else in `execute`.
- **Layout is measured, not hard-coded** (`layout.go`): `recalculateDimensions` runs after every update and sizes the viewport from the rendered header/status/input heights; `View` clamps the frame to the terminal height. Render input-area boxes with `m.activeBox`/`m.idleBox` (fixed width) and conversation rules with `m.convDivider()` (viewport width, which is one column narrower for the scrollbar). A frame taller than the terminal causes duplicated lines.
- **Permission modes** live in `agent.Mode` (+ `PlanMode`); the policy is in `agent/execute.go`. Destructive command patterns: `tools.DangerousCommand`.
- **Loop breakers** are per turn (state on `agent.turn`), so they reset automatically for each user message.
- **Every tool_call needs a result**: Any path that stops a tool batch early (loop breaker, user abort, denial, cancelled write/sudo) MUST append a tool message for each unanswered call (`answerRemainingToolCalls` / `skippedToolResults` in `internal/tui/toolflow.go`). OpenAI-compatible APIs return HTTP 400 otherwise.
- **Gemini system messages**: Only the first `RoleSystem` message becomes Gemini's system instruction; later ones are sent as `[system note]` user text (`buildGeminiContents`). Don't reintroduce "last one wins".
- **MCP names**: MCP tools are exposed as `mcp__<server>__<tool>` (`mcp.ExposedName`); the registry maps back to the original name for `tools/call`.
- **Tool approvals**: `run_command`, writes, MCP tools (unless `trusted`), `web_fetch`, and filesystem reads outside `workspaceRoot` require confirmation unless `/approve-tools on`. Use `askToolApproval` for new sensitive tools.
- **Sudo**: Prefer the askpass path (`sandbox.AskpassHelper`). Never write to an `io.Pipe` on the Update goroutine — writes block until read (use `seedStdin`).
- **Goroutines in tea.Cmds** must use snapshotted values, never read the live `Model`.
- **Context block**: `prompt.BuildContextBlock` (env + instruction files) is computed once at startup in `main.go` and appended to the first system message of the outgoing copy only (`withContextBlock`). Keep it stable within a session (prompt caching); don't put per-request data (timestamps, git dirty state) in it.
- **read_file line numbers**: output is `%6d\t` prefixed. `applyEdit` strips accidentally copied prefixes; keep both in sync if the format changes.
- **Write tools**: `write_file`, `edit_file`, `multi_edit` all go through `stateConfirmingWrite`; `PostEditCheck` runs in the commit goroutine.
- **Dynamic Context Injection**: Dynamic system instructions (like the PAR loop directive) should only be injected into the *outgoing* history array during streaming, keeping the session database/history clean.
