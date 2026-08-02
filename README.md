# SuperCode

[English](README.md) · [简体中文](README_CN.md)

SuperCode is a local-first terminal coding agent inspired by Claude Code and OpenCode. Its provider-neutral agent loop supports OpenAI Chat Completions, OpenAI Responses, Anthropic Messages, and OpenRouter through their official Go SDKs, including streaming text and tool calls across multiple configured endpoints.

The current v1 includes workspace-scoped coding tools, atomic multi-file edits, persistent PTY processes, multimodal image inspection, approved web access, recursive sub-agent collaboration, structured plans and goals, active-turn message steering, crash-recoverable sessions, MCP servers, lifecycle hooks, local plugins, `SKILL.md` activation, bounded long-term memory, slash commands, and an isolated full-screen terminal UI with live Markdown responses.

## Quick start

SuperCode requires Go 1.26 or newer. The first run automatically creates `~/.supercode/config.yaml`. You can also initialize it explicitly and print its path:

```bash
go run . config init
```

Edit the configuration:

```yaml
url: https://api.openai.com/v1
model: gpt-5.6
models: [gpt-5.6]
# model_catalog:
#   gpt-5.6:
#     context_window_tokens: 272000
#     input_modalities: [text, image]
#     tool_calling: true
#     parallel_tool_calls: true
fallback_models: []
token: ""
# token_command: ["secret-tool", "lookup", "service", "supercode"]
stream: true
timeout: 10m
max_retries: 2
approval: on-request
# approval_categories: {shell: true, network: true, mcp: true, permission: true}
max_turns: 0 # unlimited
context_window_tokens: 272000 # nominal model window
auto_compact_tokens: 244800 # 90%; start history compaction
usable_context_tokens: 258400 # 95%; reserve 5% for instructions, tools, and output
tool_output_tokens: 12000
goal_auto_continue: true
alternate_screen: true
# memory_generate: false # enable asynchronous two-phase model processing
# memory_use: true # inject only the short routing summary
# memory_dedicated_tools: true
# instructions: ""
```

Then start the full-screen terminal chat:

```bash
go run . chat
```

The interface is built with Bubble Tea, Bubbles, Lip Gloss, and Glamour. It enters an alternate-screen page by default and restores the previous shell page on exit. Mouse reporting stays disabled so text can be selected and copied normally. Standard terminal alternate-scroll mode maps the mouse wheel to transcript scrolling while the composer is empty; `PgUp`/`PgDn` always scroll the SuperCode conversation deterministically. Use `--no-alt-screen` or `alternate_screen: false` for terminal-native persistent scrollback instead. User messages appear as gray `> message` blocks; assistant responses are rendered as GFM-compatible terminal Markdown during streaming and remain rendered after completion, without role labels. Streaming deltas are coalesced into short display frames so long Markdown and code responses do not re-render once per token. Tool activity uses dedicated tool views: commands render as `Running`, `Ran`, or a live process session; searches include the query and searched path/glob; edits render complete, untruncated line-level `Added`/`Edited`/`Deleted`/`Moved` diffs; and Plan, Web, Goal, image, session, and sub-agent tools show useful fields instead of raw JSON.

- `Enter`: send a message; `Shift+Enter`, `Alt+Enter`, or `Ctrl+J` inserts a newline
- `↑` / `↓`: move within a multiline draft first, then recall up to 100 submitted inputs (including a loaded session's prompts) and restore the current draft when returning to the newest entry
- `Esc`: stop the active model/tool stream, keep partial output, and return focus to the composer
- `Ctrl+G` or `/editor`: edit the current draft in `$VISUAL` or `$EDITOR`
- Type `/` to open the searchable command menu; use `↑`/`↓`, `Tab`, and `Enter`
- `PgUp` / `PgDn`: scroll through messages
- `/new`: start a new session
- `/clear`: clear the viewport without forgetting conversation context
- `/status`: show the active model, workspace, session, tools, and skills
- `/config`: show configuration sources and project trust status
- `/model [id]`: show or change the model for subsequent turns
- `/permissions [on-request|granular|always|never]`: show or change the approval policy
- `/mode [default|plan|execute|pair]`: show or change the collaboration mode
- `/plan [on|off|show|hide]`: switch collaboration mode or toggle the structured plan panel
- `/goal`: inspect the current explicit long-term goal
- `/compact`: compact conversation history immediately
- `/review [focus]`: review the current Git changes
- `/diff [staged]`: display the working-tree or staged diff
- `/mention <path>`: attach a workspace file to the next prompt
- `/copy [assistant|tool|transcript|all]`: copy the latest assistant response, latest raw tool output, the transcript, or all output
- `/raw`: open the complete copy-friendly transcript; `/markdown` toggles rendering
- `/ps` and `/stop <id|all>`: inspect or stop background commands
- `/rename`, `/fork`, `/archive`, and `/delete confirm`: manage the current session
- `/sessions [all]`: open the searchable session picker
- `/resume [id|latest]`: open the picker or restore a saved session
- `/backtrack [turn]`: list user turns or fork before a selected turn
- `/tools`: list built-in tools
- `/skills [reload]`: search discovered skills or reload the catalog
- `/agents [name]`: show the sub-agent tree or one persisted transcript
- `/memory`: show file-backed memory status and its short summary
- `/remember <text>`: add an explicit memory note
- `/forget [text]`: queue a memory deletion or correction for consolidation
- `/queue`: inspect guidance queued during the active turn
- `/help`: show grouped, Markdown-rendered command help
- `/exit`, `/quit`, or `Ctrl+C`: begin a graceful shutdown that stops the active turn, saves the session, and (when `memory_generate: true`) processes the current session into long-term memory. Progress remains visible; press `Ctrl+C` again to force quit.

Mouse reporting is disabled in both full-screen and inline modes, so ordinary drag-to-select remains available. `/copy` and `/raw` provide reliable alternatives when a terminal has unusual clipboard behavior.

Bracketed pastes below 1,000 characters and fewer than nine lines stay editable in the composer. Larger pastes are folded into a `Pasted context · 12,345 chars` row inside the composer; after submission, the conversation shows the original text. Press `Backspace` or `Delete` on an empty composer to remove the most recent paste, use `/detach paste-2` to remove one by number, or use `/detach all` to clear every draft attachment.

When an operation needs approval, SuperCode opens a separate selection card. Use `↑`/`↓` and `Enter`, or press `y` for once, `a` for the same tool during this session, `p` for a safe command prefix during the session, `r` to persist that exact prefix, and `n`/`Esc` to deny. A `request_permissions` card instead labels those scopes as **this turn** and **this session**. The card never consumes or clears a draft in the chat input.

While an agent turn is running, the input remains active. Pressing `Enter` queues the message under **Messages to be submitted after next tool call**. The message is inserted after the current tool-call batch, or as a follow-up when the model finishes without calling a tool. Type `/queue` during a turn to inspect queued messages.

Completed turns are saved under `~/.supercode/sessions/`. `index.json` provides incrementally updated searchable metadata and full-text routing without SQLite. New messages append to a sequenced JSONL write-ahead log, while authoritative JSON snapshots are refreshed adaptively after enough deltas or WAL growth. Resume replays only records newer than the snapshot, repairs an incomplete final record, and reports corruption in the middle instead of silently discarding it. WAL segments covered by a snapshot are compressed as `.jsonl.gz`. This bounds snapshot write amplification while retaining crash recovery and compatibility with legacy full `checkpoint` events. Image inputs are content-addressed under `assets/<session-id>/` so multimodal sessions resume without losing images. Cross-process locks prevent concurrent writers from losing revisions, and the store can rebuild its index from snapshots plus WAL without deleting invalid files. Cross-session memory lives under `~/.supercode/memories/`; a historical `~/.supercode/memory.md` is migrated once into an explicit note. Files use local mode `0600`; containing directories use mode `0700`.

## Coding tools and approvals

The model can call these provider-neutral tools:

| Tool | Purpose | Default policy |
| --- | --- | --- |
| `list_files` | List workspace files | Automatic |
| `search_text` | Search text files | Automatic |
| `read_file` | Read bounded line ranges | Automatic |
| `git_status` | Inspect Git status | Automatic |
| `git_diff` | Inspect staged or unstaged changes | Automatic |
| `apply_patch` | Atomically create, edit, move, delete, or apply unified-diff hunks with optional SHA-256 preconditions; repeated paths in `operations` run sequentially in memory, and omission placeholders are rejected | Automatic workspace-write |
| `exec_command` | Start a command with optional PTY and return a live session ID | Sandboxed reads automatic; others ask |
| `write_stdin` | Write or poll an existing process session | Automatic after command approval |
| `wait` | Wait for process output or terminate a session | Automatic; termination asks |
| `list_processes` / `stop_process` | Inspect or stop background command sessions | List automatic; model-requested stop asks |
| `view_image` | Return a local PNG/JPEG/GIF as model image input | Automatic |
| `web__run` | Search/open/find/click, PDF screenshots, image search, finance, weather, sports, and time | Ask |
| `request_permissions` | Request additional read/write roots or network domains/protocols for this turn or session | Always asks |
| `memories_search` / `memories_read` / `memories_list` | Search, read, or list public long-term Markdown memory | Automatic read-only |
| `memories_add_ad_hoc_note` | Queue an explicitly requested remember/forget/update note | Ask as a write |
| `tool_search` | Search and lazily enable MCP tools | Automatic |
| `mcp__<server>__<tool>` | Invoke a dynamically discovered MCP tool | Always asks by default; remote annotations are not trusted as an approval boundary |
| `update_plan` | Update the structured task plan | Automatic |
| `create_goal` / `get_goal` / `update_goal` | Manage an explicitly requested long-term goal | Automatic |
| `spawn_agent` / `send_message` / `followup_task` | Start or steer bounded sub-agents | Automatic orchestration |
| `interrupt_agent` / `list_agents` / `wait_agent` | Control and observe sub-agents | Automatic orchestration |

File tools reject paths and symbolic links that escape the configured roots. Tool output is bounded, commands have a timeout, and agent runs can use a configurable model-turn limit (`0` means unlimited). Independent tools that explicitly opt into parallel safety—such as workspace reads and typed MCP resource/prompt reads—run concurrently with a process-wide eight-call bound while results remain in model-call order; stateful reads and all write/execute operations stay ordered. Dynamic remote MCP tool calls are ordered and approval-gated regardless of server annotations. `exec_command`, `write_stdin`, and `wait` stream process deltas into the active tool card before completion while retaining a rolling 1 MiB tail as the final bounded observation for the model. Live streaming continues after that retention window fills. `exec_command` keeps long-running processes in a session so later calls can continue them, and cancellation or shutdown terminates its process tree. `view_image` returns image input through the provider-neutral message model. `web__run` blocks private, loopback, link-local, multicast, non-global-unicast, and non-HTTP targets; approval is checked again for the actual endpoint and every redirect, and its page cache is byte-bounded.

On Linux, SuperCode probes Bubblewrap at startup. When available, shell commands see the host filesystem as read-only, configured write roots are reopened for writes, deny roots plus `.git` and `.supercode` remain protected, `/tmp` is ephemeral, and the process runs in separate user and PID namespaces. Additional `read_roots`, `write_roots`, and `deny_roots` can be configured in YAML. Network namespaces are enabled when the host permits them. If the host cannot isolate networking, only a conservative set of non-networking read commands can skip approval; all other commands require approval. `sandbox_permissions: require-escalated` runs outside this boundary only after approval and requires a justification. macOS and Windows explicitly report an approval-only fallback in `/status`; they are never mislabeled as equivalent native sandboxes.

The default `on-request` policy treats validated workspace file edits as workspace-write operations: they do not need a second confirmation. Non-read shell commands, network requests, dynamic remote MCP tool calls, and process termination use the approval card. `request_permissions` can add canonical file-system roots and URL-aware network grants for one turn or the session. A domain grant does not disable shell network isolation; only an explicit `*` protocol plus `*` domain grant can do that. `granular` follows `approval_categories` and fails closed for categories configured as `false`; `always` allows requests and `never` denies them. In non-interactive mode, an unresolved approval is denied; use `--approval always` only in a workspace and with a model endpoint you trust.

PDF page screenshots use the `pdftoppm` executable from Poppler when available; other `web__run` operations do not require it.

## Plans, goals, and collaboration

`update_plan` accepts `pending`, `in_progress`, and `completed` steps and enforces at most one active step. The TUI renders the plan in a dedicated bordered panel with status icons. Plans and goals are saved in the session snapshot and event log.

The Goal tools are intentionally separate: `create_goal` is exposed for an explicitly requested long-term goal, `get_goal` reads it, and `update_goal` can mark it `complete` or `blocked`. Active goals track model turns and input/output/total token usage; `get_goal` also reports elapsed time and the remaining token budget. In the TUI, an active explicit goal can continue automatically between turns, bounded by its token budget and a 20-continuation runtime limit. Ordinary development work should use `update_plan`.

Sub-agent tools are available when the user explicitly requests parallel, delegated, or multi-agent work. Each sub-agent has isolated conversation history, an optional role/model/reasoning hint, and may recursively delegate up to three levels with a shared concurrency limit. Workspace edits stay inside the same sandbox; commands and network calls cannot self-approve. Agent histories, roles, outputs, and statuses are persisted with the session and restored as interrupted after a crash.

## Skills and memory

Skills are discovered from:

```text
~/.supercode/skills/*/SKILL.md
<workspace>/.supercode/skills/*/SKILL.md
```

Project skills override user skills with the same name. A skill uses YAML frontmatter:

```markdown
---
name: review
description: Review a change for correctness and regressions
---

# Review workflow

Inspect the diff, run focused checks, and report concrete findings.
```

The compact skill catalog is injected with each skill's exact local `SKILL.md` path. When the user names a skill (with `$review` or plain text), the task clearly matches its description, or a declared `triggers` entry matches, the model is instructed to read the complete file through the normal read-only file tool before acting. The runtime adds discovered Skill roots to the read-only sandbox, so the model never needs to scan the home directory or inspect `.git` objects to locate Skill content. `/skills` opens a fuzzy picker and `supercode skills list|check|install|remove` manages user skills.

Long-term memory is file-backed; it does not use SQLite, embeddings, or a vector database. Its layout is:

```text
~/.supercode/memories/
  state.json                         private rollout and usage metadata
  MEMORY.md                          consolidated searchable handbook
  memory_summary.md                  short routing summary injected per turn
  raw_memories.md                    selected Phase 2 input
  raw/<rollout-id>.md                secret-redacted Phase 1 extracts
  rollout_summaries/<rollout-id>.md  narrow retrieval targets
  skills/<name>/SKILL.md             generated reusable workflows
  extensions/ad_hoc/notes/*.md       explicit remember/forget/update notes
```

When `memory_generate: true`, startup launches a bounded background pipeline. Phase 1 considers only non-current, non-archived root sessions from the last 10 days that have been idle for at least six hours, processing at most two per run. A graceful TUI exit first commits the current root session, then processes it immediately without the startup idle cutoff; a redacted content hash skips duplicate work when only session metadata changed. It asks the configured extraction model at low reasoning for strict JSON containing detailed memory and a routing summary. Phase 2 ranks up to 256 successful extracts by use count and recency, includes explicit notes, and asks the consolidation model at medium reasoning for `MEMORY.md`, `memory_summary.md`, and optional Skills. Empty model names use the active chat model. Inputs and outputs are secret-redacted, and the private memory directory maintains its own Git baseline for consolidation diffs.

Only `memory_summary.md`, truncated to `memory_max_tokens` (2,500 by default), is injected into a normal turn. The model searches detailed memory on demand with the dedicated read-only tools. Hidden memory citations are removed from visible streamed output and update `usage_count` and `last_usage` in `state.json`, which influences future consolidation priority. `/remember` and `/forget` create append-only notes; they do not directly rewrite generated memory. Automatic model generation is disabled by default to avoid unexpected API cost, while use of already generated memory is enabled by default.

Project instructions are discovered from the Git/project root to the working directory. `AGENTS.override.md` wins over `AGENTS.md` in the same directory, optional fallback filenames are configurable, and a total byte limit prevents unbounded prompt growth. `.supercode/instructions.md` remains a final project-local instruction layer. `/config` shows the files that were actually loaded.

## Prompt architecture

SuperCode uses an embedded, provider-neutral prompt package. The wording is adapted to SuperCode's provider-neutral model boundary and tool contracts.

Stable session instructions contain the coding-agent behavior, communication rules, tool discipline, project instructions, `apply_patch` contract, and multi-agent orchestration policy. Per-turn instructions add the active collaboration mode, approval and sandbox status, model, date, workspace, context budget, selected Skills, Memory, plugins, hooks, MCP servers, and active Goal. The two layers are combined into the portable `Instructions` field; each provider adapter maps it to that API's native instruction or system-message representation.

The `default`, `plan`, `execute`, and `pair` modes are persisted with the session and can be changed with `/mode`. `/plan` remains a shortcut for entering or leaving Plan mode and for showing or hiding the structured plan panel. Review, goal continuation, Apply Patch, multi-agent orchestration, awaiter, Memory extraction, and Memory consolidation prompts are connected to their matching client workflows. Compact remains deterministic and local; Guardian and Realtime templates remain explicit integration points without model-backed runtimes.


## MCP, hooks, and plugins

Trusted configuration can add MCP servers over stdio or Streamable HTTP. Protocol negotiation, transport framing, pagination, and typed content are handled by the official MCP Go SDK. Tools are discovered during startup and named `mcp__<server>__<tool>`. MCP tools stay out of the model request until `tool_search` selects them, reducing context use. Resource and Prompt capabilities are exposed as server-specific tools. HTTP credentials can use environment-expanded headers or an `oauth_token_command` whose stdout supplies a bearer token without storing it in YAML. `supercode mcp status [name]` connects to a server and reports availability; `supercode mcp reload` validates all enabled servers; `supercode mcp login|logout <name> [...]` configures or removes an OAuth token command.

Configured MCP servers connect and discover tools concurrently with an independent startup timeout. A failed server emits a warning while healthy servers remain available; `mcp status` and `mcp reload` report every failure and return an error when validation is incomplete.

```yaml
mcp_servers:
  local:
    transport: stdio
    command: local-mcp-server
    args: ["--stdio"]
  remote:
    transport: streamable-http
    url: https://mcp.example.com/mcp
    oauth_token_command: ["secret-tool", "lookup", "service", "example-mcp"]
```

Lifecycle hooks support `session_start`, `session_end`, `user_prompt_submit`, `pre_tool_use`, `post_tool_use`, `permission_request`, `pre_compact`, `post_compact`, `subagent_start`, and `subagent_stop`. Commands are invoked directly without a shell, receive JSON on stdin, and can return JSON to block an action, rewrite a prompt/tool argument, or inject context. Hook failures stop the affected operation.

```yaml
hooks:
  pre_tool_use:
    - command: ["./scripts/check-tool.sh"]
      matcher: "^(apply_patch|exec_command)$"
      timeout: 5s
```

Local plugins live under `~/.supercode/plugins/<name>/plugin.yaml`; trusted projects may also use `.supercode/plugins/<name>/plugin.yaml`. A plugin can contribute instructions, skills, MCP servers, and hooks. `supercode plugins list|install|enable|disable|remove` manages local bundles. Hooks can be listed, enabled, disabled, and trusted by executable SHA-256 with `supercode hooks ...`; a changed executable no longer matches its trusted digest. This is a local bundle format, not a remote marketplace.

## Configuration and precedence

Values are resolved in this order:

```text
command-line flags > environment variables > trusted project config > user config > built-in defaults
```

The user configuration directory uses mode `0700`, and the file uses mode `0600`. Existing configuration content is never overwritten during normal startup. A project file at `.supercode/config.yaml` is ignored until the workspace is explicitly enabled with `--trust-project`; `/config` or `--config-diagnostics` shows the active sources. Trust is stored by canonical workspace path so a symlink cannot inherit another project's decision.

For the legacy single endpoint, API-key precedence is `OPENAI_API_KEY`, then `token_command`, then YAML `token`. `token_command` executes an argument vector directly, has a five-second timeout, and reads a bounded token from stdout. It can bridge Secret Service, macOS Keychain, a password manager, or another system credential helper. A YAML token remains local plaintext; leaving it blank is safer:

```bash
export OPENAI_API_KEY="your_api_key"
```

Supported environment variables:

| Variable | Purpose |
| --- | --- |
| `OPENAI_API_KEY` | API token; overrides YAML `token` |
| `OPENAI_BASE_URL` | API base URL |
| `OPENAI_MODEL` | Model ID |
| `ANTHROPIC_API_KEY` | Default key for an `anthropic` provider |
| `OPENROUTER_API_KEY` | Default key for an `openrouter` provider |
| `SUPERCODE_STREAM` | Enables or disables streaming |
| `SUPERCODE_TIMEOUT` | Request timeout, such as `30s` or `2m` |
| `SUPERCODE_INSTRUCTIONS` | Developer instructions |
| `SUPERCODE_CONFIG` | Alternative YAML configuration path |

The CLI uses Cobra and follows a Git-style command hierarchy:

```text
supercode [prompt...]                  Run one task, or open the TUI without a prompt
supercode chat [prompt...]             Start interactive multi-turn chat
supercode review [focus...]            Run a non-interactive review of current Git changes
supercode sessions                     List saved workspace sessions
supercode config init                  Create the user configuration
supercode config diagnostics           Show config sources, precedence, and trust
supercode doctor                       Check auth, sandbox, rg, Git, clipboard, MCP, and hooks
supercode diagnostics export <zip>     Write a redacted support archive
supercode policy list|check|remove     Inspect persistent execution rules
supercode mcp list|get|add|remove|status|reload|login|logout
                                       Manage, validate, and authenticate MCP servers
supercode skills list|check|install|remove
                                       Manage local skills
supercode hooks list|trust|enable|disable
                                       Inspect and trust lifecycle hooks
supercode plugins list|install|enable|disable|remove
                                       Install, toggle, or remove local plugins
supercode completion <shell>           Generate bash, zsh, fish, or PowerShell completion
```

Global flags:

```text
--base-url string            OpenAI API base URL
--approval string            Tool policy: on-request, granular, always, or never
--chat                       Start multi-turn chat; TUI in a terminal, line mode in a pipe
--config-diagnostics         Print config sources and project trust status
--context-window-tokens int  Estimated model context window
--auto-compact-tokens int    Automatic compaction threshold
--usable-context-tokens int  Hard request limit after reserved output headroom
--tool-output-tokens int     Maximum retained tokens per tool result
--goal-auto-continue bool    Continue an active explicit goal between turns
--init-config                Initialize the configuration and print its path
--instructions string        Developer instructions
--max-retries int            SDK retries for transient model API failures
--max-turns int              Maximum model turns per task; 0 means unlimited
-i, --image path             Attach an image (repeatable)
-m, --model string           Model ID
--reasoning-effort string    Model reasoning effort
--service-tier string        Provider service tier
--resume string              Resume a session ID, or latest
--sessions                   List sessions for the current workspace
--stream bool                Enable streaming output
--timeout duration           Per-model-request timeout
--alt-screen                 Use the isolated TUI page (default)
--no-alt-screen              Preserve terminal-native scrollback
--trust-project              Trust and enable .supercode/config.yaml
-w, --workspace string       Workspace root (default: current directory)
```

Run `supercode --help` or `supercode <command> --help` for generated help. Legacy single-dash long flags remain accepted for compatibility, but new scripts should use POSIX `--long-flag` syntax.

## Custom endpoint

Configure an OpenAI-compatible endpoint in YAML:

```yaml
url: http://127.0.0.1:8000/v1
model: your-model
token: "your-token"
stream: true
approval: on-request
max_turns: 0
```

You can also override it for a single invocation:

```bash
go run . \
  --base-url http://127.0.0.1:8000/v1 \
  --model your-model \
  "Hello"
```

The configured token is sent to this URL, so only connect to services you trust. A compatible service must implement the OpenAI Chat Completions API and its data-only SSE format when streaming is enabled.

### Multiple providers

Use `providers` to select models across multiple URLs and protocols:

```yaml
model: openai/gpt-4o
providers:
  - name: openai
    provider: openai
    url: https://api.openai.com/v1
    token: ${OPENAI_API_KEY}
    models: [gpt-4o, gpt-4o-mini]
  - name: responses
    provider: openai_responses
    url: https://api.openai.com/v1
    token: ${OPENAI_API_KEY}
    models: [gpt-5-codex]
  - name: anthropic
    provider: anthropic
    url: https://api.anthropic.com
    token: ${ANTHROPIC_API_KEY}
    maxTokens: 8192
    models: [claude-sonnet-4-6, claude-opus-4-6]
  - name: openrouter
    provider: openrouter
    url: https://openrouter.ai/api/v1
    token: ${OPENROUTER_API_KEY}
    models: [openai/gpt-4o, anthropic/claude-sonnet-4]
    headers:
      HTTP-Referer: https://example.com
      X-OpenRouter-Title: SuperCode
```

Every provider has its own `url`, `token`, and optional `token_command`; top-level endpoint credentials are not inherited by provider entries. `openai` uses `/chat/completions`, `openai_responses` uses `/responses`, `anthropic` uses `/v1/messages`, and `openrouter` uses `/chat/completions`. An Anthropic `url` may include or omit a trailing `/v1`. `token: ${NAME}` resolves that exact environment variable; when `token` is omitted, the provider defaults to `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, or `OPENROUTER_API_KEY`.

The stable selector is `provider-name/model-id`, such as `responses/gpt-5-codex`. An unqualified model ID is accepted when it is unique; duplicate model IDs require the qualified selector. The model picker renders the identity as `gpt-5-codex [in responses]`, with the provider suffix dimmed. Legacy `url`, `token`, and `models` configuration remains supported as one OpenAI-compatible Chat Completions endpoint.

## Non-interactive usage

Arguments produce a one-shot request:

```bash
go run . "Explain Go interfaces in one sentence"
```

A prompt can also be read from standard input:

```bash
echo "Explain the goal of this project" | go run .
```

Use line-oriented multi-turn chat in a pipeline:

```bash
printf 'First question\nContinue\n/exit\n' | go run . chat
```

Disable streaming when needed:

```bash
go run . --stream=false "Hello"
```

Automation can consume JSONL events, validate the final answer, or write only the last message:

```bash
supercode --json "Inspect this repository"
supercode --output-schema result.schema.json --output-last-message result.json "Return the requested JSON"
supercode --ephemeral review "Focus on concurrency regressions"
```

`--ephemeral` avoids saving a one-shot session. `review` is a dedicated non-interactive Git review command; unresolved tool approvals are denied unless an explicit non-interactive policy permits them.

Resume the latest session in the current workspace:

```bash
go run . --resume latest
```

List sessions without requiring an API key:

```bash
go run . sessions
```

## Project structure

```text
main.go                        Process entry point, signal handling, and exit status
app/                           Cobra options, startup stages, runtime assembly, and execution modes
internal/config/               Secure YAML creation, loading, and validation
internal/credential/           Environment, command, and static-token credential resolution
internal/agent/                Bounded model/tool orchestration and approvals
internal/permission/           Turn/session file-system and network grants
internal/modelcatalog/         Configurable model limits and capabilities
internal/tool/                 Tool registry and workspace-scoped built-ins
internal/mcp/                  Official MCP Go SDK integration and tool adapters
internal/hook/                 Trusted lifecycle hook runtime
internal/plugin/               Local extension-bundle discovery
internal/collaboration/        Recursive, persisted sub-agent lifecycle
internal/taskstate/            Structured plan and explicit goal state
internal/provider/             Provider-neutral request, response, and stream types
internal/provider/openai/      OpenAI-compatible Chat Completions adapter
internal/provider/openairesponses/ OpenAI Responses adapter
internal/provider/anthropic/   Anthropic Messages adapter
internal/provider/openrouter/  OpenRouter adapter
internal/session/              Indexed JSON snapshots, assets, and compressed event logs
internal/skill/                SKILL.md discovery and exact local-path injection
internal/memory/               File-backed extraction, consolidation, retrieval, and usage feedback
internal/tui/                  Bubble Tea state, routed slash handlers, transcript/session state, and rendering
```

To add a provider, implement `provider.Provider` under `internal/provider/<name>`. Provider SDK types must not leak into the common provider layer, CLI, or TUI.

## Verification

```bash
go test ./...
go vet ./...
go build .
```

Provider tests use an in-memory HTTP transport, and the CLI integration test uses a loopback-only mock server when local sockets are available. Tests do not access a live API or consume credits.
