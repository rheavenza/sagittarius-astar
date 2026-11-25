# AI Handover Context & Project Status
**Project Name:** `aig` (sagittarius-a*)
**Type:** Terminal-based AI Assistant (TUI)
**Stack:** Go, Bubble Tea, Lipgloss, Glamour, x/sys/windows

## 🎯 Purpose of this file
This file serves as a compressed "brain dump" for future AI agents working on this project. Read this before starting a task to understand the architecture, recent changes, and known quirks, thereby minimizing token usage on redundant codebase discovery.

---

Architecture, rules and quirks for AI agents now live in [`AGENTS.md`](AGENTS.md), which aig and other agents load automatically.


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

3. **Loop Breaker Circuit**
   - **Consecutive Same-Call Breaker**: Intercepts tool execution if the exact same tool and argument hash are called 3 times consecutively. Halts the execution loop and injects a warning context to force model re-evaluation.
   - **Consecutive Error Breaker**: Intercepts and aborts tool loops after 3 consecutive failures to prevent cascading resource waste.

4. **Dynamic API Key Reloading & Windows Selection**
   - **API Key Resolving**: Resolves keys dynamically on each generation run rather than caching on start.
   - **Quick Edit Mode**: Restores Quick Edit Mode on Windows Terminal to enable native selection without breaking the TUI.

5. **Review bug-fix pass (`fix/review-bugfixes`)** — see `FIXES.md` for the full list (protocol correctness, process-group kills, approvals, MCP hardening, tests).

---
