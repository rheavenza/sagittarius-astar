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

### Project context & instructions
On startup aig builds a context block that is appended to the system prompt of every request (never stored in the session):
- **Environment**: working dir, git root and branch, OS (with Bazzite/ostree hints), date, detected stack (`go.mod`, `package.json`, …), and useful CLI tools on PATH.
- **Instruction files**: `AGENTS.md`, `CLAUDE.md`, `.aig/instructions.md` in the working directory and the git root (each needs one-time consent, re-asked when the file changes), plus your global `~/.config/aig/AGENTS.md` (always trusted).

### Coding tools
| Tool | What it does |
|---|---|
| `read_file` | Line-numbered output (`N<TAB>line`), line ranges, cut off at 50 KB on a line boundary |
| `find_files` | Glob search (`*.go`, `internal/**/*_test.go`, `*.{ts,tsx}`), `.gitignore`-aware |
| `search_regex` | ripgrep when installed, Go fallback; `.gitignore`-aware, skips binaries, `case_insensitive` |
| `edit_file` | Exact replace, `replace_all`, tolerant of whitespace/indentation differences and copied line numbers |
| `multi_edit` | Several edits to one file, applied atomically with one diff and one confirmation |

After each confirmed write aig runs a quick check and returns the result to the model: `go vet` on the package (or `gofmt -e`), `python3 -m py_compile`, `bash -n`, `node --check`, JSON validation. Override or extend per extension:
```json
"post_edit_checks": { ".ts": "npx tsc --noEmit -p {dir}", ".py": "off" }
```

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
aig -p deepseek -m deepseek-v4-flash

# Use a custom system prompt persona
aig --persona sysadmin
aig -s coder

# Resume the most recent session
aig -r
aig --resume

# Resume a specific conversation by ID
aig -r 20260521-170000
```

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
| `/provider` | List available providers |
| `/provider <name>` | Switch the active provider on-the-fly (keeps conversation history) |
| `/persona` | List available personas configured |
| `/persona <name>` | Switch the system prompt/persona on-the-fly (takes effect on next message) |
| `/approve-tools on\|off` | Toggle auto-approve for commands and sensitive tools (shows `⚡auto` badge in status bar when active) |
| `/map [dir]` | Generates and injects a tree-like map of `[dir]` into the conversation history |
| `/exit` | Exit the application |
| `Ctrl+C` | Cancel a running command / token stream |
| `Enter` | Send message |
| `Shift+Enter` | Insert a newline in the text area |

### Inline Macros

- **`/read(path)`**: Attach the content of a file located at `path` directly to the next message context. You can include multiple `/read(...)` macros in a single input message. E.g., `Refactor /read(internal/tui/styles.go) to use Catppuccin theme.`

---

## Architecture

```
cmd/aig            CLI entry: config, MCP bootstrap, context block, agent + TUI wiring
internal/agent     the agent loop (UI-independent) → event stream + approval requests
internal/tools     native tools, edit matching, post-edit checks, risk levels
internal/tui       Bubble Tea front-end: renders events, answers approvals
internal/llm       providers (Gemini, OpenAI-compatible DeepSeek / NVIDIA NIM), pruning, <think> splitting
internal/prompt    environment block + AGENTS.md / CLAUDE.md instructions
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
