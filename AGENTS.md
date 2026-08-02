# SuperCode Agent Guide

## Project overview

SuperCode is a local-first terminal coding agent written in Go. It supports
OpenAI Chat Completions, OpenAI Responses, Anthropic Messages, and OpenRouter
behind a provider-neutral router, plus a full-screen Bubble Tea UI, sandboxed
tools, resumable sessions, MCP servers, plugins, hooks, skills, memory, and
bounded multi-agent collaboration.

Keep the provider boundary vendor-neutral. Features outside
`internal/provider/<adapter>` must use the types in `internal/provider` rather
than importing a vendor SDK directly.

## Repository map

- `app/`: CLI startup, configuration assembly, lifecycle, and command routing.
- `internal/agent/`: model loop, context budgeting, approvals, and tool batches.
- `internal/tool/`: built-in tools, workspace confinement, sandbox, and PTYs.
- `internal/tui/`: Bubble Tea state, rendering, commands, and interaction flows.
- `internal/session/`: snapshots, event logs, indexes, assets, and recovery.
- `internal/mcp/`: MCP transports and conversion to provider-neutral tools.
- `internal/memory/`: local long-term memory and its optional model pipeline.
- `internal/prompts/templates/`: embedded session, mode, and special prompts.

## Working rules

1. Inspect the relevant implementation and tests before editing.
2. Preserve unrelated user changes; never use destructive Git commands to
   clean a working tree.
3. Use `gofmt` on changed Go files. Avoid adding dependencies when the standard
   library is sufficient.
4. Keep `README.md` and `README_CN.md` aligned when user-facing behavior,
   configuration, commands, or requirements change.
5. Add focused regression tests for every bug fix and behavioral change.
6. Prefer small state-machine helpers over adding more branches to the central
   agent or TUI loops.

## Safety and compatibility invariants

- Workspace tools must reject path traversal, symlink escapes, and access
  outside configured roots. Do not weaken these checks for convenience.
- Approval risk is a security boundary. A tool marked `RiskRead` must not gain
  new external side effects. Stateful read-risk tools must not opt into
  parallel execution.
- Parallel tool execution requires the explicit `tool.Parallelizable` opt-in.
  Writes, shell/process control, permissions, and other stateful operations
  remain ordered unless their semantics are redesigned and tested.
- An assistant tool-call batch must produce exactly one tool result per call,
  in original call order, with stable non-empty call IDs.
- Session changes must load older snapshots and legacy `checkpoint` events.
  Completed turns must be durable before the UI reports them as saved.
- Graceful TUI shutdown must cancel and join active work, commit the current
  session, then run optional memory generation before quitting. Keep the
  visible progress state and the second-`Ctrl+C` force-exit escape hatch.
- Snapshot and policy writes remain atomic and use restrictive permissions.
- TUI update functions must not block on network, model, tool, or filesystem
  work. Use commands/events, coalesce high-frequency updates, and preserve
  stale-event protection when cancellation starts a new turn.
- MCP failures may degrade individual servers, but must remain visible to the
  user and must not silently disable all integrations.
- Secrets, authorization headers, API keys, and credential-command output must
  never appear in logs, tests, fixtures, or error messages.

## Validation

Run the smallest relevant tests while iterating, then before handoff run:

```bash
gofmt -w <changed-go-files>
go test ./...
go vet ./...
go test -race ./...
```

For performance-sensitive changes, also run the relevant benchmarks, such as:

```bash
go test ./internal/tui ./internal/session -run '^$' -bench . -benchmem
```

Tests that require Bubblewrap, PTYs, Git, or local sockets may skip when the
host lacks those capabilities. Keep unit coverage for the surrounding logic so
a skipped integration test does not leave the behavior completely untested.
