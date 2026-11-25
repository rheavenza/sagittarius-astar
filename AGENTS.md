# AGENTS.md — aig (Sagittarius A*)

Instructions for AI agents (aig itself, Claude Code, Codex, …) working on this repository.
`PROJECT_STATUS.md` has the change history; `roadmap.md` has what is planned.

## Commands
- Build: `make build` (or `go build ./...`)
- Test: `make test` (`go test -race ./...`). Tests that touch the summary cache must set `XDG_CACHE_HOME`.
- Vet / format: `make vet`, `make fmt`
- The project checks in `.aig/config.json` (build, vet, test) are the definition of done.

## Architecture
- **`internal/agent/`**: the agent loop, independent of any UI. `Agent.Run(ctx, history)` returns an event channel (`StreamStartEvent`, `TokenEvent`, `MessageEvent`, `ToolStartEvent`, `ToolResultEvent`, `ApprovalEvent`, `NoticeEvent`, final `DoneEvent`). Approval policy lives in `execute.go`; loop breakers and "every tool_call gets a result" in `agent.go`.
- **`internal/tools/`**: native tool definitions and implementations (read/search/find/edit/multi_edit/write/run/web), edit matching (`edits.go`), post-edit checks (`checks.go`), risk levels (`risk.go`).
- **`internal/tui/`**: Bubble Tea front-end. `turn.go` maps agent events to display and answers `ApprovalRequest`s; `model.go` holds state, slash commands, layout; `providers.go` provider/model switching.
- **`internal/prompt/`**: environment block + instruction files (AGENTS.md…), project checks and repo map sections appended to the system prompt.
- **`internal/repomap/`**: repository map (definitions + reference ranking + token budget). `Build` → `Map.Render(Options)`. Used by the `repo_map` tool and by `startRepoMap` in `cmd/aig/main.go`.
- **`internal/summary/`**: `/summarize` index. `Scan` builds a `Plan` (cost preview); `Plan.Run` summarizes files, then folders deepest-first, then the project; `Index.Lookup` serves `project_overview`. The store is `os.UserCacheDir()/aig/index/<base>-<sha12>/`.
- **`internal/workspace/`**: `ListFiles` (git ls-files or a walk that skips ignored dirs), `IsIgnoredDir`, workspace containment checks.
- **`internal/llm/`**: providers (Gemini, OpenAI-compatible DeepSeek/NIM), history pruning, `ThinkSplitter` for inline `<think>` reasoning.
- **`internal/sandbox/`**: command execution (process groups, output caps, askpass sudo, stdin wiring).
- **`internal/checkpoint/`**: workspace checkpoints (git snapshot commits under `refs/aig/checkpoints/<session>/`, or an in-memory journal outside git) backing `/undo`, `/redo`, `/diff`.
- **`internal/mcp/`**, **`internal/config/`**, **`internal/session/`**: MCP host, configuration, persistence.

## Rules & quirks
- **Reasoning Prefix (`\x00`)**: Providers prefix native reasoning tokens with a null byte; `agent.stream` turns them into `TokenEvent{Reasoning: true}`. Keep the prefix in providers.
- **MCP Server Graces**: When adding or managing MCP servers, ensure `Registry.Shutdown()` is called *before* `Manager.Shutdown()` to drain pending requests safely before closing the pipes.
- **History ownership**: during a turn the agent owns the conversation; the TUI appends each `MessageEvent.Msg` to `m.history` in order and must not modify history while `m.events != nil`.
- **Bubble Tea copies the Model by value**: keep `strings.Builder` / splitters behind pointers (`*streamState`).
- **stdin for commands**: never hand `os/exec` a non-`*os.File` reader that stays open — use `sandbox.ExecuteWithStdin`, which wires an OS pipe (`attachStdin`).
- **Checkpoints**: `agent.turn.ensureCheckpoint` runs once per turn right before the first mutation (after approval). Any new mutating tool must call it. Git snapshots must never touch the user's index: always use `GIT_INDEX_FILE` with a temp copy (`gitRepo.tempIndex`).
- **Plan mode** is snapshotted per turn (`turn.planMode`); it filters tool definitions to read/network risk and denies everything else in `execute`.
- **Layout is measured, not hard-coded** (`layout.go`): `recalculateDimensions` runs after every update and sizes the viewport from the rendered header/status/input heights; `View` clamps the frame to the terminal height. Render input-area boxes with `m.activeBox`/`m.idleBox` (fixed width) and conversation rules with `m.convDivider()` (viewport width, which is one column narrower for the scrollbar). A frame taller than the terminal causes duplicated lines.
- **Context budget** (`agent/context.go`): `budget` holds the window (`SetContextWindow`, called by the TUI in `startTurn`, or `Options.MaxContextTokens` in tests) and a calibration factor (real/estimated tokens). `turn.fitContext` runs before every request: compaction at `compactAt`, then `PruneHistory` as a hard backstop.
  - Compaction emits `HistoryEvent`; front-ends must replace their history with it.
  - `turn.tailNote` (plan mode) is appended per request and never stored.
  - `shrinkOutput` stores tool results over 16 KB in `Agent.outputs` for `read_output`.
  - Providers report usage only via `llm.ReportUsage(ctx, …)`; the OpenAI stream keeps reading after `finish_reason=tool_calls` to get the usage chunk.
- **Explore subagent** (`agent/explore.go`): a nested `turn` with `sub: true` and its own `toolDefs`. It shares the provider and the context block (without the plan directive). `turn.ask` refuses when `sub` is set, and `execute` lets sub turns use only `RiskRead` tools inside the workspace. Its events go to a private channel: `ToolStartEvent` becomes a `NoticeEvent`, everything else is dropped. A new tool with `RiskRead` becomes available to subagents automatically, so keep `RiskRead` for side-effect-free tools.
- **Summary cache keys** include `promptVersion`: bump it when the summary prompts change. A folder's key hashes its children's summaries, not their contents. Tests must set `XDG_CACHE_HOME`, or they write to the real cache.
- **Repo map ranking**: Go reference keys are scoped (`pkg.Name`, bare names in the package, `.Method`); non-Go names count referencing files and are discounted when generic. Change scores in `rank` and check the result on a real repo, because the unit tests only pin the relative order.
- **Project checks**: `.aig/config.json` → `config.LoadProjectConfig` (keeps the JSON key order) → `tools.ProjectChecks` on `agent.Options`. `run_checks` is offered only when checks exist, it is RiskExec (so plan mode blocks it), and `agent.runChecks` asks once for all checks. `sandbox.Options.MaxTimeout` raises the 120 s command ceiling for it.
- **Permission modes** live in `agent.Mode` (+ `PlanMode`); the policy is in `agent/execute.go`. Destructive command patterns: `tools.DangerousCommand`.
- **Loop breakers** are per turn (state on `agent.turn`), so they reset automatically for each user message.
- **Every tool_call needs a result**: Any path that stops a tool batch early (loop breaker, user abort, denial, cancelled write/sudo) MUST append a tool message for each unanswered call (`turn.answer` / `SkippedToolResults` in `internal/agent/agent.go`). OpenAI-compatible APIs return HTTP 400 otherwise.
- **Gemini system messages**: Only the first `RoleSystem` message becomes Gemini's system instruction; later ones are sent as `[system note]` user text (`buildGeminiContents`). Don't reintroduce "last one wins".
- **MCP names**: MCP tools are exposed as `mcp__<server>__<tool>` (`mcp.ExposedName`); the registry maps back to the original name for `tools/call`.
- **Tool approvals**: `run_command`, writes, MCP tools (unless `trusted`), `web_fetch`, and filesystem reads outside `workspaceRoot` require confirmation unless the permission mode allows them (see `agent/execute.go`). New sensitive tools ask through `turn.ask(ctx, newRequest(...))`.
- **Sudo**: Prefer the askpass path (`sandbox.AskpassHelper`). Never write to an `io.Pipe` synchronously — writes block until read (`runCommand` writes the password from a goroutine).
- **Goroutines in tea.Cmds** must use snapshotted values, never read the live `Model`.
- **Context block**: `prompt.BuildContextBlock` (env + instruction files) is computed once at startup in `main.go` and appended to the first system message of the outgoing copy only (`agent.WithContextBlock`). Keep it stable within a session (prompt caching); don't put per-request data (timestamps, git dirty state) in it.
- **read_file line numbers**: output is `%6d\t` prefixed. `applyEdit` strips accidentally copied prefixes; keep both in sync if the format changes.
- **Write tools**: `write_file`, `edit_file`, `multi_edit` all go through `stateConfirmingWrite`; `PostEditCheck` runs in the commit goroutine.
- **Dynamic Context Injection**: Dynamic system instructions (like the PAR loop directive) should only be injected into the *outgoing* history array during streaming, keeping the session database/history clean.
