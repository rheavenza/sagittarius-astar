# 🚀 Sagittarius A* (`aig`) — Roadmap

**Where things stand (Nov 2026):** aig is a streaming Bubble Tea agent with Gemini, DeepSeek and
NVIDIA NIM providers, native tool calling, an MCP host, sessions, personas and approval gates.
The bug-fix and hardening pass is documented in [`FIXES.md`](FIXES.md).

The goal of the next phases is to make aig a **strong coding agent**: one that understands a
whole codebase, edits precisely, checks its own work, and can be undone safely.

Legend: ✅ done · 🚧 in progress · ⬜ planned

---

## Phase 1 — Context & system prompt ✅  *(branch `feat/agent-context-tools`)*
Cheap, high-impact: the model should know where it is and what the project expects.

- ✅ **Environment block** computed once per session and appended to the system prompt on every
  request. It holds the working dir, OS/distro (Bazzite / ostree detection), date, git branch and
  detected stack. It is stable within a session so provider prompt caching keeps working.
- ✅ **Project instruction files** are loaded automatically: `AGENTS.md`, `CLAUDE.md`,
  `.aig/instructions.md` from the working dir and the git root, plus a global
  `~/.config/aig/AGENTS.md`. Repo files need one-time consent per content hash (same trust store
  as `aig.md`).
- ✅ **New default `coder` prompt**: concrete workflow rules (explore → plan → minimal edit →
  verify), tool etiquette, and treating injected content as untrusted data.
- ✅ **Removed the forced Plan-Action-Reflection directive** and the `<think>` instruction.
  Thinking models already reason; forcing it again duplicated reasoning and padded answers.

## Phase 2 — Coding tools ✅  *(branch `feat/agent-context-tools`)*
Most edit failures come from the tools, not from the model.

- ✅ `read_file` output has line numbers (`N<TAB>line`).
- ✅ `find_files` glob tool (`**/*.go`, `{a,b}`), `.gitignore`-aware via `git ls-files`.
- ✅ `search_regex`: uses ripgrep when installed, is `.gitignore`-aware, and has a
  case-insensitive option.
- ✅ `multi_edit`: several replacements in one file, with one diff and one approval.
  `edit_file` gains `replace_all`.
- ✅ Edit matching tolerates whitespace and indentation differences, and strips line-number
  prefixes the model copied by accident.
- ✅ **Checks after each edit**: syntax/compile checks run after a confirmed write (`go build`,
  `gofmt -e`, `python -m py_compile`, `bash -n`, JSON validation; extensible via config), and
  errors go back into the tool result.
- ✅ **Project checks** in `.aig/config.json` (`"test": "go test ./..."`), run in order by a
  `run_checks` tool and shown to the model as the definition of done. The file needs consent
  per content hash, all checks share one approval, and destructive checks still ask in auto
  mode.

## Phase 3 — Agent core refactor ✅  *(branch `refactor/agent-core`)*
Prerequisite for headless mode, subagents and proper tests.

- ✅ `internal/agent` runs the loop: tool routing, loop breakers, approvals, cancellation and an
  event stream (`StreamStart`, `Token`, `Message`, `ToolStart`, `ToolResult`, `Approval`,
  `Notice`, `Done`). The TUI only renders events and answers approval requests
  (`model.go`: 2,900 → ~1,500 lines).
- ✅ `internal/tools` holds the native tools. Each has a `Risk` level (read / network / write /
  exec / external), and the approval policy for native and MCP tools is based on it.
- ✅ Removed the legacy "regex on markdown code blocks" execution path.
- ✅ Fake-provider test harnesses: agent end-to-end tests, plus TUI tests that drive `Update`
  with key presses.
- ✅ Streaming re-render is no longer O(n²): an incremental `<think>` splitter, a cached
  transcript prefix, and renders throttled to about 30 fps.
- ✅ Fixed: `run_command` could hang after the command finished, because exec waited on the
  open stdin pipe.

## Phase 4 — Safety net ✅  *(branch `feat/safety-net`)*
- ✅ **Checkpoints** before the agent's first change in each turn (a write, command or MCP call):
  - In git repos, a snapshot commit of the work tree (tracked plus untracked files that aren't
    ignored), built with a temporary index and stored as `refs/aig/checkpoints/<session>/<n>`.
    Your index, HEAD and branches are never touched, and checkpoints survive restarts.
    About 30 ms on this repo.
  - Outside git, a journal of the original contents of files written by aig's edit tools.
- ✅ `/undo` (takes a redo point first, so nothing is lost), `/redo`, `/diff` (all changes
  since the session's first checkpoint), `/checkpoints`. The model is told about undo/redo so
  it re-reads files.
- ✅ **Permission modes**, cycled with Shift+Tab or set with `/mode`:
  - **ask**: approve every write, command and sensitive tool.
  - **accept edits**: edits inside the workspace apply without asking.
  - **auto**: everything runs without asking, except writes outside the workspace and
    destructive commands (`rm -rf`, force-push, `mkfs`, `rpm-ostree rebase`…).
  - **plan**.
  Set the startup mode with `permission_mode` in config.
- ✅ TUI: the frame is measured so it never exceeds the terminal (this fixes the duplicated
  status bar and input box); the status bar never wraps; conversation scrollbar; the view
  stays put while you read scrolled-up history; Ctrl+Home/End.
- ✅ CLI: `aig version`, unknown arguments are rejected, and aig refuses to start the TUI
  without a terminal (it used to hang when an agent ran it).
- ✅ Plan mode: `/plan [task]` offers the model only read-only tools, blocks everything else,
  and asks for a numbered plan. `/go` approves it and starts the implementation.

## Phase 5 — Whole-project understanding ✅
- ✅ **Repo map** (`internal/repomap`): symbols and signatures per file, fitted to a token
  budget. Exposed as the `repo_map` tool (`dir`, `focus`, `max_tokens`); a version of about 1,000
  tokens goes into the system prompt in git repos.
  - Go is parsed with `go/parser`. References are scoped: `pkg.Name` from other packages, bare
    names inside the package, `.Method` for methods.
  - Python, JS/TS, Rust and shell use line patterns. They're ranked by how many files mention a
    name, and short generic names are discounted.
  - Generated files, vendored folders, test files and minified bundles rank low or are skipped.
  - The parse cache is keyed by mtime and size. The Go standard library (7k files) maps in about
    0.4 s cold and 20 ms cached.
  - Possible later improvement: tree-sitter for exact non-Go parsing.
- ✅ **`/summarize`** (`internal/summary`): summaries per file, then per folder, then the
  project, made with `summary_model` (falls back to the session's model).
  - `/summarize` previews the cost; `go` runs it in the background with 4 requests at a time;
    `stop` cancels; `show` prints the overview.
  - Cached by content hash in `~/.cache/aig/index/<repo>-<hash>/`. A folder's cache key covers
    its children's summaries, so an unchanged summary stops the re-run from going further up.
  - It stops after 5 failed requests (bad key, rate limit) instead of failing every file, and
    saves progress every 20 files.
  - Produces `PROJECT_SUMMARY.md` and the `project_overview` tool (offered once an index exists;
    changed files are flagged). The overview is added to the system prompt at session start.
- ✅ **Exploration subagent**: `explore(question, hints?)` runs a nested agent turn with a fresh
  context (its own prompt, plus the environment, instructions and repo map) and returns its answer.
  - Only local read-only tools: no writes, commands, web or MCP, and it can't call `explore`
    again. It never asks for approval; anything that would need it is refused.
  - Its tool calls show in the TUI as `↳ explore: read_file …`. Esc/Ctrl+C cancels it along with
    the turn.
  - It gets 20 rounds; after that it must answer with no tools offered.
  - Allowed in plan mode.
  - Later: a separate cheaper model for subagents, and running several explore calls in parallel.
- ⬜ **Semantic search** (postponed): embeddings (NIM / Gemini) in SQLite with a
  `semantic_search` tool. Revisit after Phase 6, once there's real usage data for
  `repo_map` + `explore` + `project_overview`.

## Phase 6 — Context management ✅
- ✅ **Real token usage**: providers report it through `llm.WithUsage(ctx, &u)` (OpenAI-compatible
  via `stream_options.include_usage`, Gemini via `UsageMetadata`), so the `Provider` interface
  didn't change. There's a context-window table per model, with `context_windows` overrides in
  config. Token estimates are calibrated against the real counts.
- ✅ **Status bar**: shows `ctx 34% (45k/131k)`, with `~` when it's an estimate and yellow from 70%.
- ✅ **Compaction**: at 80% of the window, older turns are summarized and replaced by a summary
  message plus an ack; the most recent ~35% of the window is kept word for word.
  - The TUI adopts the new history (`HistoryEvent`) and saves the session.
  - When a single long turn fills the context, its old tool outputs are shortened instead,
    oldest first.
  - If the summary request fails, aig warns and falls back to dropping the oldest turns.
  - `/compact` does the same on demand.
- ✅ **Large tool outputs** (>16 KB) are stored per session. The history keeps the first and
  last 3 KB plus an ID, and `read_output(id, start_line/end_line | pattern)` fetches the rest.
- ✅ **Prompt-cache-friendly layout**: the system prompt and context block are fixed for the
  session. The plan-mode instructions are now sent as a note at the end of each request, so
  toggling `/plan` doesn't change the prompt prefix. Compaction at 80% means the old
  per-request pruning, which changed the prefix every time, rarely kicks in.

## Phase 7 — Workflow & UX ⬜
- ⬜ Headless mode: `aig -p "…"`, stdin piping (`git diff | aig -p "commit message"`).
- ⬜ `@file` mentions with autocomplete (replaces `/read(file)`).
- ⬜ Token/cost meter, session search.
- ⬜ Real isolation for commands (bubblewrap / Landlock / distrobox).
- ⬜ Proper search API (SearXNG / Brave), HTML → markdown, parallel read-only tools.
- ⬜ API keys in KWallet / libsecret; per-folder command allowlists.
