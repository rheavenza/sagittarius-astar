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

## Phase 2 — Coding tools ✅ (except project `checks`)  *(branch `feat/agent-context-tools`)*
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
- ⬜ Project `checks` in config (`"test": "go test ./..."`) surfaced to the model as the
  definition of done.

## Phase 3 — Agent core refactor ⬜
Prerequisite for headless mode, subagents and proper tests.

- ⬜ Extract `internal/agent`: the loop, tool routing, loop breakers, approvals as callbacks, and
  an event stream (`Token`, `Reasoning`, `ToolStart`, `ToolResult`, `ApprovalNeeded`, `Done`).
- ⬜ `Tool` interface + risk levels (read / write / exec / network) shared by native and MCP tools.
- ⬜ Remove the legacy "regex on markdown code blocks" execution path.
- ⬜ Fake-provider test harness for end-to-end agent tests.
- ⬜ Fix O(n²) streaming re-render.

## Phase 4 — Safety net ⬜
- ⬜ Git checkpoints before the first agent edit of each turn (hidden refs, no impact on your index).
- ⬜ `/undo`, `/diff` (everything the agent changed this session).
- ⬜ Plan mode: only read tools allowed until you approve the plan.

## Phase 5 — Whole-project understanding ⬜
- ⬜ **Repo map**: symbols and signatures per file (`go/parser` for Go, `ctags`/tree-sitter for
  others), ranked by how often they're referenced, fitted to a token budget. Exposed as a tool,
  and a small version goes into the system prompt.
- ⬜ **`/summarize`**: map-reduce summaries (file → folder → project) on a cheap model, cached
  by content hash in `~/.cache/aig/index/<repo>/`, so re-runs only cost the changed files.
  Produces `PROJECT_SUMMARY.md` plus a `project_overview` tool.
- ⬜ **Exploration subagent**: `explore(question)` runs a separate agent with read-only tools and
  a fresh context, and returns a summary. Keeps the main context small. (Needs Phase 3.)
- ⬜ **Semantic search**: embeddings (NIM / Gemini) in SQLite with a `semantic_search` tool.

## Phase 6 — Context management ⬜
- ⬜ Real token usage from API responses, plus a context-window table per model.
- ⬜ Compaction: summarize old turns at about 80% of the window instead of dropping them.
- ⬜ Large tool outputs are stored and referenced by ID; a tool fetches more on demand.
- ⬜ Prompt-cache-friendly layout: stable prefix, dynamic content only at the end.

## Phase 7 — Workflow & UX ⬜
- ⬜ Headless mode: `aig -p "…"`, stdin piping (`git diff | aig -p "commit message"`).
- ⬜ `@file` mentions with autocomplete (replaces `/read(file)`).
- ⬜ Token/cost meter, session search.
- ⬜ Real isolation for commands (bubblewrap / Landlock / distrobox).
- ⬜ Proper search API (SearXNG / Brave), HTML → markdown, parallel read-only tools.
- ⬜ API keys in KWallet / libsecret; per-folder command allowlists.
