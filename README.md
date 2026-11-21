# Sagittarius A\* — `aig`

> A cross-platform, terminal-based AI agent written in Go.

```
  ✦ aig — Sagittarius A*
  terminal AI agent  ·  type /help for commands
  ──────────────────────────────────────────────
  You
  why does `make test` fail?

  aig
  ⚡ run_command: make test
  ✓ Tool run_command done
  ⚙ Tool: read_file
  ✏️  edit_file: internal/config/config.go
  Diff preview: …
  💾 Write to internal/config/config.go? (y/Esc)
```

---

## Features

### Core Capabilities
- 🤖 **Multi-Provider & Model Integration** — Support for Google Gemini (official Go SDK, including `gemini-2.5-flash` (default), `gemini-2.5-pro`, etc.), DeepSeek (OpenAI-compatible, including `deepseek-v4-flash`, `deepseek-v4-pro`, etc.), and **NVIDIA NIM** (hosted at build.nvidia.com or self-hosted; any model in the catalog, browsable live with `/models`). Switch providers and models on-the-fly inside the active session.
- 🔌 **Model Context Protocol (MCP) Host** — Dynamic host architecture supporting any stdio transport MCP server. Discovers schemas via `tools/list` on startup, translates them dynamically into LLM function declarations, and runs RPC tool calls asynchronously.
- 🧠 **Interleaved Reasoning & PAR Loops** — Renders model thinking phases in real-time under a separate, styled deliberating section (Catppuccin subtext, left border, status badge). Dynamically enforces a **Plan-Action-Reflection (PAR)** directive when tools are active to optimize tool-calling ergonomics.
- ⚡ **Autonomous Tool-Calling Agent Loop** — Runs multi-turn tool loops sequentially to resolve complex tasks (e.g., read file, search, write/edit file, run test command, repeat).
- 🌊 **Real-time Streaming TUI** — Built on Bubble Tea and Bubbles, featuring live token streaming, interactive spinners, and status bar updates.
- 🎨 **Premium Aesthetics** — Custom Catppuccin Mocha themed UI, rounded borders, and dynamic status bars highlighting current state, active model, persona, and auto-approve mode status.
- 📝 **Live Markdown Rendering** — Renders markdown responses on-the-fly using Glamour with automatic dark/light theme detection.
- 🖥️ **Cross-Platform** — Native shell/sandbox support for Linux (bash/sh) and Windows (PowerShell) execution.

### Integrated Agent Tools
The agent can autonomously invoke these tools:
- 📂 **File Utilities**:
  - `read_file` — Reads entire files or targeted line-ranges (`start_line`/`end_line`) with a 50KB safety cap.
  - `write_file` — Overwrites or creates new files (prompts user with a unified diff preview).
  - `edit_file` — Surgery-grade text replacements using exact old/new snippets (verifies single-match uniqueness and displays a diff).
  - `list_directory` — Lists entries with file-size information (similar to `ls`).
- 🔍 **Search & Mapping**:
  - `search_regex` — Runs recursive regex matching across files (similar to `grep -rn`) with path, line-number, and glob filtering.
  - `map_workspace` — Generates a tree layout of the workspace to help orient the agent.
- 🌐 **Web Scraper & Search**:
  - `web_search` — DuckDuckGo query search returning titles, snippets, and source links.
  - `web_fetch` — Fetches raw HTML page text, preserves code blocks, strips noise (scripts, CSS, headers, footers), and formats content for documentation reference.

### Safety & Command Sandboxing
- 🛡️ **Interactive Gating**: Full confirmation dialogs for file mutations (`write_file`, `edit_file`) showing unified diffs.
- ⚡ **Auto-Approve Mode**: Enable auto-approve (`/approve-tools on` or CLI flag) to execute commands (`run_command`), MCP tools, `web_fetch` and out-of-workspace reads without confirmation prompts. Without it, reads inside the working directory run freely; everything else asks first.
- 🔐 **Sudo via KDE dialog**: In a graphical session with `ksshaskpass` (or `$SUDO_ASKPASS`) available, `sudo` is run as `sudo -A` so the password goes through a desktop dialog and is never seen by aig.
- 🔄 **Loop Breaker Circuit**: Automatically intercepts and blocks infinite loops or cascading errors. If the model invokes the exact same tool with identical arguments 3 times consecutively, or experiences 3 consecutive tool failures, execution is aborted, and a warning is injected to force re-evaluation.
- 🛑 **Graceful Interrupts**: Cancel active streams or executing sandbox commands safely using `Ctrl+C` without crashing the application.

---

## Installation

### From Source

```bash
git clone github.com/clitorhea/sagittarius-astar.git
cd sagittarius-astar
go mod tidy
make build          # current OS
make build-all      # linux-amd64 + windows-amd64
./bin/aig
````

### Nix (Development Shell)

```bash
nix develop         # enters the dev shell with Go, gopls, golangci-lint
nix build           # builds the aig package (update vendorHash first)
```

---

## Configuration

`aig` resolves settings from the following sources, in order of priority:
1. CLI Flags (`--provider`, `--model`, `--persona`, etc.)
2. Environment variables (`GEMINI_API_KEY`, `DEEPSEEK_API_KEY`, `NVIDIA_API_KEY`, `NIM_BASE_URL`)
3. JSON Configuration File (`~/.config/aig/config.json`)

On first launch, `aig` generates a default `config.json` at:
- **Linux/macOS:** `~/.config/aig/config.json`
- **Windows:** `%AppData%\aig\config.json`

### `config.json` Structure
```json
{
  "default_provider": "gemini",
  "default_model": "",
  "default_persona": "coder",
  "gemini_api_key": "YOUR_GEMINI_KEY",
  "deepseek_api_key": "YOUR_DEEPSEEK_KEY",
  "nvidia_api_key": "nvapi-...",
  "nim_base_url": "",
  "personas": {
    "reviewer": "Your own persona. Built-ins (default, sysadmin, coder) are always available; entries here override them."
  },
  "mcp_servers": {
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/home/user/workspace"],
      "env": {
        "NODE_ENV": "production"
      },
      "trusted": false
    }
  }
}
```

### Model Context Protocol (MCP) Support
`aig` acts as a dynamic MCP host. If the optional `mcp_servers` map is configured:
1. **Subprocess Management**: Spawns configured servers as subprocesses using stdio pipes at startup.
2. **Dynamic Discovery**: Automatically sends the `initialize` handshake and queries `tools/list` over the pipe.
3. **LLM Translation**: Translates discovered schemas on-the-fly to function declarations for Gemini and OpenAI/DeepSeek models.
4. **Asynchronous Execution**: Dispatches requested tools in non-blocking Bubble Tea commands with `Ctrl+C` interrupt support.
5. **Namespacing & approval**: Tools are exposed as `mcp__<server>__<tool>` so they never shadow built-ins. Each call asks for confirmation unless the server has `"trusted": true` or `/approve-tools on` is active.
6. **Fault isolation**: A server that fails to start or to answer discovery is skipped; its stderr is written to the aig log.

### Safety net: checkpoints, undo, plan mode
- Before the agent's first change in a turn (a file write, command or MCP call), aig saves a **checkpoint**:
  - In a git repo, it snapshots the whole working tree, tracked and untracked files that aren't ignored, into hidden refs `refs/aig/checkpoints/<session>/…`. Your index, HEAD and branches are never touched, and checkpoints survive restarts.
  - Outside git, it keeps the original contents of the files written by aig's edit tools. Changes made by shell commands can't be undone there.
- `/undo` restores the state before the last agent turn and saves a redo point first. `/redo` re-applies it. Repeated `/undo` walks further back.
- `/diff` shows everything that changed since the session's first checkpoint. `/checkpoints` lists them.
- **Permission modes**: press **Shift+Tab** to cycle, or use `/mode <ask|edits|auto|plan>`. The current mode shows in the status bar.
  | Mode | File edits in workspace | Commands | MCP / web_fetch / outside reads |
  |---|---|---|---|
  | ask (default) | ask | ask | ask |
  | accept edits | auto | ask | ask |
  | auto | auto | auto, except destructive ones (`rm -rf`, force-push, `mkfs`, `rpm-ostree rebase`, …) | auto |
  | plan | blocked | blocked | read-only tools only |

  Writes outside the workspace always ask. Set the startup mode with `"permission_mode": "accept-edits"` in config. `/approve-tools on|off` is a shortcut for `/mode auto|ask`.
- `/plan [task]` turns on **plan mode**: the agent can only read and search, then replies with a numbered plan. `/go` approves it and the agent implements it; `/plan off` leaves plan mode.
- To remove old checkpoint refs: `git for-each-ref --format='%(refname)' refs/aig/ | xargs -n1 git update-ref -d`

### Project context & instructions
On startup aig builds a context block that is appended to the system prompt of every request (never stored in the session):
- **Environment**: working dir, git root and branch, OS (with Bazzite/ostree hints), date, detected stack (`go.mod`, `package.json`, …), and useful CLI tools on PATH.
- **Instruction files**: `AGENTS.md`, `CLAUDE.md`, `.aig/instructions.md` in the working directory and the git root (each needs one-time consent, re-asked when the file changes), plus your global `~/.config/aig/AGENTS.md` (always trusted).
- **Repository map** (git repos only): the most-referenced definitions in the repo, about 1,000 tokens. It's built in the background at startup and skipped if it isn't ready within 3 s. Set the size with `"repo_map_tokens": 2048`, or turn it off with `-1`.

### Coding tools
| Tool | What it does |
|---|---|
| `read_file` | Line-numbered output (`N<TAB>line`), line ranges, cut off at 50 KB on a line boundary |
| `find_files` | Glob search (`*.go`, `internal/**/*_test.go`, `*.{ts,tsx}`), `.gitignore`-aware |
| `search_regex` | ripgrep when installed, Go fallback; `.gitignore`-aware, skips binaries and dependency/build dirs (`node_modules`, `vendor`, `target`, …), `case_insensitive` |
| `edit_file` | Exact replace, `replace_all`, tolerant of whitespace/indentation differences and copied line numbers |
| `multi_edit` | Several edits to one file, applied atomically with one diff and one confirmation |
| `repo_map` | Definitions and signatures (with line numbers), grouped by file and ranked by how often they're referenced, cut to a token budget. `dir`, `focus` (paths or symbol names to rank first), `max_tokens` |
| `explore` | Hands a codebase question to a subagent that starts with a fresh context and has only local read-only tools. Only its answer comes back, so the raw tool output never enters the main context |
| `project_overview` | Reads the `/summarize` index: the project overview and folder list, one folder, or one file. Files that changed since are flagged. Offered once an index exists |
| `run_checks` | Runs the project checks from `.aig/config.json` (only offered when some are configured) |

After each confirmed write aig runs a quick check and returns the result to the model: `go vet` on the package (or `gofmt -e`), `python3 -m py_compile`, `bash -n`, `node --check`, JSON validation. Override or extend per extension:
```json
"post_edit_checks": { ".ts": "npx tsc --noEmit -p {dir}", ".py": "off" }
```

### Context management
- **Status bar:** shows how much of the model's context window the last request used, e.g. `ctx 34% (45k/131k)`. The numbers come from the API when the provider reports them; `~` marks an estimate.
- **Windows:** context windows come from a built-in table. Override them with `"context_windows": {"moonshotai/kimi-k3": 262144}`.
- **Automatic compaction:** at about 80% of the window, aig summarizes the older turns with the current model and keeps the recent ones word for word. A 📦 notice shows the before/after size.
  - If a single long turn fills the context, its older large tool outputs are shortened instead.
  - `/compact` does it on demand.
- **Large tool outputs** (over 16 KB) stay out of the conversation. The model sees the first and last 3 KB plus an ID, and fetches the rest with `read_output` (a line range or a regex).

### Project summary (`/summarize`)
- `/summarize` only previews: how many source files, how many are already cached, an estimated token count, and which model would be used.
- `/summarize go` builds the summary in the background while you keep working. A cheap model writes 2–4 sentences per file, then a paragraph per folder, then a project overview.
- Results go to `~/.cache/aig/index/<repo>-<hash>/` (`index.json`, `PROJECT_SUMMARY.md`). Nothing is written into the repo.
- Summaries are cached by content, so re-runs only pay for changed files and the folders above them. A folder whose summary came out the same doesn't trigger new requests above it.
- Skipped: tests, generated and vendored files, and non-source files. Each file sends at most 24 KB to the model.
- `/summarize stop` cancels; finished summaries are kept and `go` resumes. `/summarize show` prints the overview.
- After a run, the agent gets the `project_overview` tool, and the overview is added to the system prompt from the next session.
- Choose the model with `"summary_model": "deepseek:deepseek-v4-flash"` (`provider:model`, or just a model of the current provider). Without it, the session's model is used.

### Project checks (definition of done)
Put the project's checks in `.aig/config.json` (working directory or git root). They run in the order listed, from the directory that contains `.aig/`:
```json
{
  "checks": {
    "build": "go build ./...",
    "vet":   "go vet ./...",
    "test":  "go test ./..."
  },
  "checks_timeout_seconds": 300
}
```
- The model sees them in a *Definition of done* prompt section and gets a `run_checks` tool (`names` optional, all checks by default). It is told a change isn't finished until they pass.
- `run_checks` asks for one approval that covers every check it runs. Auto mode runs them without asking, unless a check matches a destructive pattern. Plan mode blocks it.
- The result has PASS/FAIL per check; only failing checks include output (head and tail). Each check gets 300 s by default, up to 1800 s.
- A repository can ship this file, so it needs the same one-time consent as `AGENTS.md`. To keep checks local, add `.aig/` to `.git/info/exclude`.

### NVIDIA NIM
- Provider name `nim` (alias `nvidia`). Uses the OpenAI-compatible API at `https://integrate.api.nvidia.com/v1`, or `nim_base_url` / `NIM_BASE_URL` for a self-hosted NIM (no API key needed then).
- `/model` and `/models <filter>` fetch the live model list from the endpoint, so new catalog models work without an aig update.
- Tool calling depends on the model: check its card on build.nvidia.com. Models without tool support may reject requests that include tools.
- Mid-conversation system messages are folded into user turns, since many open-model chat templates reject them.

### Per-directory prompt (`./aig.md`)
If the working directory contains `aig.md`, aig asks once before using it as the system prompt, and remembers the approval (by content hash) in `~/.config/aig/trusted_prompts.json`. If the file changes, aig asks again.


---

## Usage

```bash
# Default (Gemini)
aig

# Specify provider and model
aig --provider deepseek --model deepseek-v4-pro

# NVIDIA NIM (hosted) — key from https://build.nvidia.com
export NVIDIA_API_KEY=nvapi-...
aig --provider nim --model meta/llama-3.3-70b-instruct

# Self-hosted NIM container (no key needed)
NIM_BASE_URL=http://localhost:8000/v1 aig --provider nim --model <model-id>
aig -P deepseek -m deepseek-v4-flash      # -P is --provider (-p is headless mode)

# Use a custom system prompt persona
aig --persona sysadmin
aig -s coder

# Resume the most recent session
aig -r
aig --resume

# Resume a specific conversation by ID
aig -r 20260521-170000
```

### Headless mode (`-p`)

`aig -p` runs one prompt without the TUI: the final answer goes to stdout,
tool activity and warnings to stderr, so it composes in pipes and scripts.
Piped stdin is attached to the prompt (or is the prompt, if none is given).

```bash
aig -p "where is the retry logic?"
git diff --staged | aig -p "write a commit message" | git commit -F -
aig -p --permission-mode accept-edits "fix the failing test"
aig -p --output-format json "summarize README.md" | jq -r .result
aig -p --output-format stream-json "…"   # one JSON event per line
aig -r -p "and now the tests"            # continue the latest session
```

- Nobody can approve anything, so calls that need approval in the permission
  mode are **denied** and the model is told why (destructive commands are
  still denied in `auto`). Choose the mode with `--permission-mode`.
- Untrusted `AGENTS.md` / `.aig/config.json` / `aig.md` are skipped without a
  terminal to ask; approve them once in the TUI.
- Every run is saved as a session (`aig -r` resumes it in the TUI or with `-p`).
- `--output-format json` prints one object: `result`, `is_error`, `error`,
  `session_id`, `usage` (summed over requests), `requests`, `tool_calls`,
  `denied`, `duration_ms`.
- Exit codes: `0` success, `1` error (provider failure, tool-round limit),
  `130` interrupted. `-q` hides tool activity and notices.

### In-session commands

| Command | Action |
| ------------- | ------------------------------ |
| `/help` or `/?` | Display help menu listing all commands and shortcuts |
| `/history` | List all saved conversation sessions with timestamps and names |
| `/load <id>` | Resume a conversation by its session ID |
| `/save <name>` or `/rename <name>` | Save/rename the current session with a friendly name |
| `/delete <id>` | Permanently delete a saved conversation |
| `/new` | Start a fresh conversation thread, resetting history but preserving the persona |
| `/clear` | Clear the viewport display while retaining the system prompt |
| `/model` | List available models for the current active provider |
| `/model <id>` | Switch the active model on-the-fly (keeps conversation history) |
| `/models [filter]` | Pick a model from the provider's live list (NIM fetches its catalog), e.g. `/models qwen` |
| `/undo` / `/redo` | Revert / re-apply the agent's last turn of file changes |
| `/diff` | Show all changes since the session's first checkpoint |
| `/checkpoints` | List workspace checkpoints |
| `/mode [ask\|edits\|auto\|plan]` | Permission mode (Shift+Tab cycles) |
| `/plan [task]` / `/go` | Plan mode (read-only investigation → plan), then approve and implement |
| `/provider` | List available providers |
| `/provider <name>` | Switch the active provider on-the-fly (keeps conversation history) |
| `/persona` | List available personas configured |
| `/persona <name>` | Switch the system prompt/persona on-the-fly (takes effect on next message) |
| `/approve-tools on\|off` | Shortcut for `/mode auto` / `/mode ask` |
| `/map [dir]` | Generates and injects a tree-like map of `[dir]` into the conversation history |
| `/exit` | Exit the application |
| `Ctrl+C` | Cancel a running command / token stream |
| `Enter` | Send message |
| `Shift+Enter` | Insert a newline in the text area |

### `@file` mentions

Mention files in a message to attach them — in the TUI and with `aig -p`:

```
explain @internal/agent/agent.go
compare @a.go:10-40 with @"dir with space/b.go"
what's in @internal/tools/ ?
```

- `@path` attaches the file (up to 100 KB; longer files are cut with a note
  telling the model to `read_file` the rest), `@path:10-40` a line range,
  `@dir/` a directory listing. At most 400 KB per message.
- Type `@` and press **Tab** to complete from the workspace's files (fuzzy;
  Tab again cycles). Suggestions show under the input.
- Mentions that don't name a file (`@alice`, e-mail addresses) are left
  alone; a path-like mention that doesn't exist is flagged with ⚠.
- The transcript shows `📎 path (N lines)`; the content goes to the model
  only. The old `/read(path)` macro still works and means `@path`.

---

## Architecture

```
cmd/aig            CLI entry: config, MCP bootstrap, context block, agent + TUI / headless (-p) wiring
internal/agent     the agent loop (UI-independent) → event stream + approval requests
internal/tools     native tools, edit matching, post-edit checks, risk levels
internal/tui       Bubble Tea front-end: renders events, answers approvals
internal/llm       providers (Gemini, OpenAI-compatible DeepSeek / NVIDIA NIM), pruning, <think> splitting
internal/mention   @file mentions: attachment blocks, path completion
internal/prompt    environment block + AGENTS.md / CLAUDE.md instructions
internal/repomap   repository map: Go via go/parser, Python/JS/TS/Rust/shell via line patterns, reference ranking
internal/sandbox   command execution (process groups, output caps, askpass sudo)
internal/mcp       MCP host (stdio servers, namespaced tools)
internal/config    config file, personas, provider endpoints, prompt trust store
internal/session   JSON session persistence
```

### Turn lifecycle

```
user message
   │
   ▼
agent.Run(ctx, history) ──► StreamStartEvent, TokenEvent… ──► TUI live preview (≈30 fps)
   │
   ├─ no tool calls ──► MessageEvent(assistant) ──► DoneEvent
   │
   └─ tool calls ──► for each call:
                       policy: read in workspace → run
                               write → ApprovalEvent (diff)        ─┐
                               run_command / MCP / web_fetch /      │ TUI answers:
                               outside workspace → ApprovalEvent   ─┘ Approve / Deny
                       ToolStartEvent → execute → ToolResultEvent + MessageEvent(tool)
                     loop breakers (3 identical calls / 3 errors) answer the rest and re-plan
                     ──► stream again (up to 50 rounds)

Ctrl+C at any point cancels ctx: remaining calls are answered "aborted", DoneEvent{Aborted}.
```

The TUI mirrors the conversation by appending every `MessageEvent` to its history and saving the session. The agent never touches the UI, so it can also drive a headless mode or subagents. The tests drive both layers with a fake provider.

---

## Development

```bash
make test     # go test -race ./...
make vet      # go vet ./...
make fmt      # gofmt -w .
make tidy     # go mod tidy
```

---

## License

MIT
