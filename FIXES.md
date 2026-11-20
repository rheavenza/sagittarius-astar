# Bug-fix & Hardening Pass (`fix/review-bugfixes`)

These fixes come from a full review of the codebase (Nov 2026). This pass covers **correctness
bugs, safety, and project upkeep only**. New features come in a later pass (see "Deferred" at the bottom).

Legend: `[x]` done · `[ ]` not done

---

## 1. Conversation / protocol correctness

- [x] **Gemini overwrote its system prompt with every later system message.**
  `gemini.go` set `systemInstruction` once per `RoleSystem` message, so the last one won.
  That could be command output, a `/read()` attachment, or a loop-breaker alert, and the persona
  was lost. Now only the *first* system message becomes the system instruction. Later ones become
  user-role `[system note]` text. Consecutive user-role contents are merged, so parallel function
  responses go out in one content.
- [x] **Loop breakers left tool calls unanswered.** Both breakers dropped the rest of the batch.
  The assistant message then had N `tool_calls` but fewer than N tool results, and
  OpenAI-compatible APIs (DeepSeek) reject that with HTTP 400. Every skipped call now gets a
  synthetic `skipped` tool result.
- [x] **Cancelling a write, or the sudo prompt, also left a tool call unanswered.** Both now
  return a "rejected/cancelled by user" tool result.
- [x] **Ctrl+C during a tool skipped the *next* tool and could produce duplicate results.**
  The abort handler increments `pendingToolIdx`, and so does the result handler. On top of that,
  the killed goroutine also delivered a result. Ctrl+C now only cancels and sets an abort flag.
  The real result arrives once, the rest of the batch is marked aborted, and the app returns to
  input without calling the model again.
- [x] **Gemini thinking display never received thoughts.** `ThinkingConfig{IncludeThoughts: true}`
  was never set. It is now set for thinking-capable models (not 1.x / 2.0).
- [x] **Gemini thought signatures were discarded.** Newer Gemini models require `thoughtSignature`
  to be echoed on function-call parts. It is now stored on `llm.ToolCall` and sent back.
  Thought *text* is no longer sent back to Gemini, since it has no effect there.
- [x] **`<thought>` vs `<think>` mismatch.** The coder persona asked for `<thought>` but
  `SplitReasoning` only parsed `<think>`. The parser now accepts both, and the persona uses `<think>`.
- [x] **DeepSeek sent `tool_choice: "auto"` with no tools.** It is now only sent when tools exist.
- [x] **History pruning could split a turn.** It could leave the history starting with an
  assistant or tool message, and it ignored tool-call argument size. Pruning now cuts only at user
  turns, never drops the latest user turn, and counts tool-call arguments.

## 2. Command execution

- [x] **Timeouts / Ctrl+C didn't kill child processes.** Only `bash` was killed, and a background
  child holding the stdout pipe could hang `cmd.Run()`. Commands now run in their own process
  group, the whole group is killed, and `WaitDelay` is set.
- [x] **Unbounded output in memory.** stdout/stderr are now capped at 1 MiB each, and
  `Combined()` keeps head + tail (≈32 KB) for the LLM.
- [x] **Timeouts threw away partial output.** A timed-out or cancelled command now returns its
  partial output plus a `[killed: …]` note, not a bare error.
- [x] **Sudo via KDE askpass.** When a graphical session is detected and `ksshaskpass`
  (or `$SUDO_ASKPASS`) exists, `sudo` becomes `sudo -A`. You get a KDE dialog for each command and
  aig never holds the password. The old in-app password prompt remains as a fallback for
  headless / Windows.

## 3. Tool safety

- [x] **MCP tools bypassed all approval and their names clashed with built-ins.** MCP tools are
  now exposed as `mcp__<server>__<tool>`, so the filesystem server's `write_file` no longer
  shadows the native one. They ask for confirmation unless the server is marked
  `"trusted": true` in config or `/approve-tools on` is active.
- [x] **Prompt-injection exfiltration path.** `read_file`/`list_directory`/`search_regex`/
  `map_workspace` run without asking only *inside the working directory*. Outside it they ask
  first. `web_fetch` always asks (shows the URL) unless `/approve-tools on`.
- [x] **`./aig.md` silently replaced the system prompt.** Any repo you ran `aig` in could set
  your system prompt. aig now asks once per file, using its content hash, and remembers the answer
  in `~/.config/aig/trusted_prompts.json`. If the file changes, it asks again.
- [x] **`edit_file` stale overwrite.** If the file changes between the diff preview and your
  confirmation, the write is refused, not applied over the newer content.

## 4. MCP host

- [x] **Server-initiated requests were treated as responses.** For example, a server `ping` with
  `id: 1` was routed to our pending call #1. Messages with a `method` are now handled as requests
  or notifications: `ping` gets `{}`, other requests get `-32601`.
- [x] **One failing server disabled all MCP tools.** Discovery is now per-server and concurrent.
  Failed servers are logged and skipped.
- [x] **Server stderr was discarded.** It is now written to the aig log, one line per entry.
- [x] **Shutdown could hang forever** on a server that ignores stdin EOF, because
  `Client.Close` waited unboundedly. That wait is now bounded, and the manager then kills the process.
- [x] Non-text content blocks (image/resource) are now shown as placeholders, not dropped silently.

## 5. Tool robustness

- [x] `search_regex` now skips `.git`, `node_modules`, `vendor` and other dot/ignored directories,
  as well as binary files and files over 2 MB.
- [x] `map_workspace` has entry and depth limits, and it now skips unreadable directories instead
  of failing outright.
- [x] `web_fetch` / `web_search` bodies are capped (5 MB). Regexes are precompiled.
- [x] Truncation no longer cuts a multi-byte UTF-8 character in half.

## 6. Internal hygiene

- [x] Streaming goroutines no longer read the live `Model`. History, provider and tools are copied first.
- [x] The provider is rebuilt only when the API key, provider or model changes, not on every request.
- [x] Duplicated `run_command` launch code (3 copies) merged into one helper.
- [x] Loop-breaker counters reset at each new user turn.
- [x] Sessions are written atomically (temp file + rename).
- [x] Config: built-in personas are no longer copied into `config.json`, so updates to them now
  take effect. Persona resolution exists in one place, and JSON errors are reported, not ignored.
- [x] `sysadmin` persona rewritten for Bazzite / Fedora Atomic / KDE Plasma. It previously
  described Arch, pacman and Hyprland.
- [x] Default Gemini model changed from `gemini-2.0-flash` to `gemini-2.5-flash`.
- [x] Removed unused `coalesceEnv`. The Makefile `MODULE` now matches `go.mod`. The flake uses Go 1.25.

## 7. Tests

- [x] Config tests no longer write into the source tree or the real `~/.config`.
- [x] New tests: history pruning, Gemini content conversion, reasoning-tag splitting, diff,
  skipped-tool synthesis, workspace path checks, UTF-8 truncation, stale-write detection,
  sandbox process-group kill + output cap + timeout output, MCP client (server-initiated ping,
  name prefixing), atomic session save, prompt trust store.

---

## Notes for you

- **Existing `config.json`:** if it still has a `"personas"` block (written by the old default
  generator), those copies still override the built-ins. Delete the block to get the updated
  `sysadmin` / `coder` prompts. Keep any personas you wrote yourself.
- **MCP tool names changed** to `mcp__<server>__<tool>`. Old sessions that called MCP tools by
  their bare name will just get "unknown tool" if those calls are replayed.
- **`/approve-tools on`** now also covers MCP and `web_fetch` confirmations, so with it on the
  exfiltration protections are off. That's intended: it's the "YOLO" switch.
- `flake.nix` still has `vendorHash = fakeHash`. Run `nix build` once and paste the hash it reports.

## Deferred (feature / refactor pass)

- Pull an `agent` package out of `internal/tui/model.go` (the loop, tool routing, approvals
  behind an event stream), with a fake-provider test harness.
- `Tool` interface + risk levels shared by native and MCP tools.
- Remove the legacy "regex on markdown code blocks" execution path.
- O(n²) streaming re-render (`SplitReasoning` + full viewport join on every token).
- Real isolation for commands (bubblewrap / Landlock / distrobox).
- Proper search API, HTML→markdown, parallel read-only tools, token/cost meter, summarizing old
  turns, per-folder allowlists, API keys in KWallet.
