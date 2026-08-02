# SuperCode

[English](README.md) · [简体中文](README_CN.md)

SuperCode 是一个本地优先的终端编码智能体（coding agent），体验参考 Claude Code 和 OpenCode。它的智能体循环与具体厂商解耦，通过官方 Go SDK 支持 OpenAI Chat Completions、OpenAI Responses、Anthropic Messages 和 OpenRouter，并能跨多个已配置端点使用流式文本与工具调用。

当前 v1 已包含：工作区范围内的编码工具、原子化多文件编辑、持久化 PTY 进程、多模态图片检查、经审批的联网访问、递归子智能体协作、结构化计划与目标、运行中消息引导（steering）、崩溃可恢复的会话、MCP 服务器、生命周期钩子、本地插件、`SKILL.md` 激活、有界长期记忆、斜杠命令，以及带实时 Markdown 渲染的独立全屏终端界面。

## 快速开始

SuperCode 需要 Go 1.26 或更高版本。首次运行会自动创建 `~/.supercode/config.yaml`，也可以显式初始化并打印其路径：

```bash
go run . config init
```

编辑配置：

```yaml
config_version: 1
model: openai/gpt-5.6
providers:
  - name: openai
    provider: openai_responses
    url: https://api.openai.com/v1
    token: ${OPENAI_API_KEY}
    # token_command: ["secret-tool", "lookup", "service", "openai"]
    models: [gpt-5.6]
# model_catalog:
#   gpt-5.6:
#     context_window_tokens: 272000
#     input_modalities: [text, image]
#     tool_calling: true
#     parallel_tool_calls: true
fallback_models: []
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

然后启动全屏终端聊天：

```bash
go run . chat
```

界面基于 Bubble Tea、Bubbles、Lip Gloss 和 Glamour 构建。默认进入备用屏幕（alternate-screen）页面，退出时恢复之前的 shell 页面。全屏模式会显式接收滚轮事件，因此即使正在编辑多行草稿，鼠标滚轮也始终滚动对话内容；如果终端需要，按住 `Shift` 再拖动鼠标即可选中文本。`PgUp`/`PgDn` 也会以确定性的方式滚动 SuperCode 对话。可以使用 `--no-alt-screen` 或 `alternate_screen: false` 改用终端原生的持久回滚和普通拖动选择。用户消息显示为灰色 `> message` 块；助手回复在流式期间以 GFM 兼容的终端 Markdown 渲染，完成后仍保持渲染结果，不带角色标签。流式增量会合并为很短的显示帧，长 Markdown 和代码回复不会再按每个 token 重绘。工具活动使用专用的工具视图：命令显示为 `Running`、`Ran` 或实时进程会话；搜索包含查询词和搜索路径/glob；编辑渲染完整、不截断的行级 `Added`/`Edited`/`Deleted`/`Moved` diff；Plan、Web、Goal、图片、会话和子智能体工具都显示有用的字段，而不是原始 JSON。

- `Enter`：发送消息；`Shift+Enter`、`Alt+Enter` 或 `Ctrl+J` 插入换行
- `↑` / `↓`：优先在多行草稿内移动；到达首行或末行后，可调出最近 100 条已提交输入（包括已加载会话的提示词），回到最新位置时恢复当前草稿
- `Esc`：停止当前模型/工具流，保留部分输出，把焦点还给输入框
- `Ctrl+G` 或 `/editor`：在 `$VISUAL` 或 `$EDITOR` 中编辑当前草稿
- 输入 `/` 打开可搜索的命令菜单；使用 `↑`/`↓`、`Tab` 和 `Enter`
- `PgUp` / `PgDn`：滚动消息
- `/new`：开始新会话
- `/clear`：清空视口但不遗忘对话上下文
- `/status`：显示当前模型、工作区、会话、工具和技能
- `/config`：显示配置来源和项目信任状态
- `/model [id]`：显示或更改后续轮次的模型
- `/permissions [on-request|granular|always|never]`：显示或更改审批策略
- `/mode [default|plan|execute|pair]`：显示或更改协作模式
- `/plan [on|off|show|hide]`：切换协作模式或切换结构化计划面板
- `/goal`：查看当前的显式长期目标
- `/compact`：立即压缩对话历史
- `/review [focus]`：审查当前 Git 变更
- `/diff [staged]`：显示工作区或暂存区 diff
- `/mention <path>`：把工作区文件附加到下一次提示词
- `/copy [assistant|tool|transcript|all]`：复制最新的助手回复、最新原始工具输出、完整转录或全部输出
- `/raw`：打开完整、便于复制的转录；`/markdown` 切换渲染
- `/ps` 和 `/stop <id|all>`：查看或停止后台命令
- `/rename`、`/fork`、`/archive` 和 `/delete confirm`：管理当前会话
- `/sessions [all]`：打开可搜索的会话选择器
- `/resume [id|latest]`：打开选择器或恢复已保存的会话
- `/backtrack [turn]`：列出用户轮次或在某个轮次之前分叉
- `/tools`：列出内置工具
- `/skills [reload]`：搜索发现的技能或重新加载目录
- `/agents [name]`：显示子智能体树或某个持久化转录
- `/memory`：显示文件型记忆状态及其简短摘要
- `/remember <text>`：添加一条显式记忆笔记
- `/forget [text]`：排队一次记忆删除或更正以用于合并
- `/queue`：检查当前轮次期间排队的引导消息
- `/help`：显示分组、Markdown 渲染的命令帮助
- `/exit`、`/quit` 或 `Ctrl+C`：开始优雅退出，先停止当前轮次并保存会话；当 `memory_generate: true` 时，还会把当前会话处理进长期记忆。保存进度会留在界面上；再次按 `Ctrl+C` 可强制退出。

只有全屏模式会启用鼠标上报，避免滚轮被终端转换为 `Up`/`Down` 键后泄漏到输入框。如有需要，可用 `Shift`+拖动进行终端文本选择，或使用 `/copy` 和 `/raw`。内联模式仍完全由终端处理鼠标与回滚。

小于 1000 个字符且少于 9 行的 bracketed paste 保持在输入框中可编辑。更大的粘贴会在输入框内折叠成一行，例如 `Pasted context · 12,345 chars`；提交后对话会显示原始文本。在空输入框按 `Backspace` 或 `Delete` 删除最近的一次粘贴，用 `/detach paste-2` 按编号移除某一项，或用 `/detach all` 清空所有草稿附件。

当操作需要批准时，SuperCode 会打开一个独立的选择卡片。使用 `↑`/`↓` 和 `Enter`，或按 `y` 仅本次允许、`a` 本次会话允许同一工具、`p` 本次会话允许安全命令前缀、`r` 持久化该精确前缀、`n`/`Esc` 拒绝。`request_permissions` 卡片则将这些范围标记为**本轮**和**本会话**。卡片永远不会消费或清空聊天输入框中的草稿。

在智能体轮次运行期间输入仍然可用。按 `Enter` 会把消息排队到 **Messages to be submitted after next tool call**。消息会在当前工具调用批次之后插入，或者在模型未调用工具就结束时作为后续消息提交。轮次期间输入 `/queue` 可检查排队的消息。

已完成的轮次保存在 `~/.supercode/sessions/` 下。`index.json` 提供增量更新的可搜索元数据和全文路由，不依赖 SQLite。新增消息写入带序号的 JSONL 预写日志；当增量数量或 WAL 大小达到自适应阈值时，再刷新权威 JSON 快照。恢复只重放比快照新的记录，能修复末尾未写完的一行，并会明确报告中间损坏，而不是静默丢弃。快照已覆盖的 WAL 段会压缩为 `.jsonl.gz`。这既限制了快照写放大，也保留了崩溃恢复以及对旧式完整 `checkpoint` 事件的兼容。图片输入按内容寻址存储在 `assets/<session-id>/` 下，因此多模态会话恢复时不会丢失图片。跨进程锁避免并发写入丢失修订，索引可以从快照和 WAL 重建而不删除无效文件。跨会话记忆位于 `~/.supercode/memories/`；历史 `~/.supercode/memory.md` 会被一次性迁移为显式笔记。文件使用本地模式 `0600`，所在目录使用 `0700`。

## 编码工具与审批

模型可以调用这些厂商无关的工具：

| 工具 | 用途 | 默认策略 |
| --- | --- | --- |
| `list_files` | 列出工作区文件 | 自动 |
| `search_text` | 搜索文本文件 | 自动 |
| `read_file` | 读取有界行范围 | 自动 |
| `git_status` | 检查 Git 状态 | 自动 |
| `git_diff` | 检查暂存或未暂存变更 | 自动 |
| `apply_patch` | 原子化创建、编辑、移动、删除或应用 unified-diff hunk，支持可选 SHA-256 前置条件；拒绝省略占位符 | 自动 workspace-write |
| `exec_command` | 启动带可选 PTY 的命令并返回实时 session ID | 沙箱只读自动；其他需询问 |
| `write_stdin` | 写入或轮询已有进程会话 | 命令批准后自动 |
| `wait` | 等待进程输出或终止会话 | 自动；终止需询问 |
| `list_processes` / `stop_process` | 查看或停止后台命令会话 | 列出自动；模型请求的停止需询问 |
| `view_image` | 把本地 PNG/JPEG/GIF 作为模型图片输入返回 | 自动 |
| `web__run` | 搜索/打开/查找/点击、PDF 截图、图片搜索、金融、天气、体育和时间 | 询问 |
| `request_permissions` | 为本轮或本会话请求额外读/写根目录或网络域名/协议 | 总是询问 |
| `memories_search` / `memories_read` / `memories_list` | 搜索、读取或列出公开的长期 Markdown 记忆 | 自动只读 |
| `memories_add_ad_hoc_note` | 排队一条显式请求的记住/忘记/更新笔记 | 作为写入询问 |
| `tool_search` | 搜索并延迟启用 MCP 工具 | 自动 |
| `mcp__<server>__<tool>` | 调用动态发现的 MCP 工具 | 默认总是询问；不把远端注解当作审批边界 |
| `update_plan` | 更新结构化任务计划 | 自动 |
| `create_goal` / `get_goal` / `update_goal` | 管理显式请求的长期目标 | 自动 |
| `spawn_agent` / `send_message` / `followup_task` | 启动或引导有界子智能体 | 自动编排 |
| `interrupt_agent` / `list_agents` / `wait_agent` | 控制和观察子智能体 | 自动编排 |

文件工具拒绝逃逸已配置根目录的路径和符号链接。工具输出有界，命令有超时，智能体运行可使用可配置的模型轮次上限（`0` 表示不限）。显式声明并行安全的独立工具——例如工作区读取和类型明确的 MCP Resource/Prompt 读取——会在进程级八路上限内并发运行，同时仍按模型调用顺序提交结果；有状态读取以及全部写入/执行操作保持有序。动态远端 MCP 工具无论服务器注解如何都会保持有序并经过审批。`exec_command`、`write_stdin` 和 `wait` 在完成前把进程增量流式写入活动的工具卡片，同时为模型保留滚动的最新 1 MiB 作为最终受限观察结果；即使超过保留窗口，实时流也不会停止。`exec_command` 把长时间运行的进程保持在会话中，以便后续调用继续；取消或关闭时会终止整棵进程树。`view_image` 通过厂商无关的消息模型返回图片输入。`web__run` 默认阻止私网、loopback、link-local、组播、非全局单播和非 HTTP 目标；执行时会再次校验实际端点及每次重定向，页面缓存也按字节设限。

在 Linux 上，SuperCode 启动时探测 Bubblewrap。可用时，shell 命令看到的主机文件系统是只读的，已配置的写根目录会重新挂载为可写，拒绝根目录以及 `.git` 和 `.supercode` 保持受保护，`/tmp` 是临时的，进程运行在独立的用户和 PID 命名空间中。可以通过 YAML 配置额外的 `read_roots`、`write_roots` 和 `deny_roots`。主机允许时启用网络命名空间。如果主机无法隔离网络，只有一组保守的非联网只读命令可以跳过审批；其他所有命令都需要批准。`sandbox_permissions: require-escalated` 在批准后、且必须提供理由时才在该边界之外运行。macOS 和 Windows 会在 `/status` 中明确报告仅审批的降级模式；它们永远不会被错误标称为等效的原生沙箱。

默认的 `on-request` 策略把已验证的工作区文件编辑当作 workspace-write 操作：不需要第二次确认。非只读 shell 命令、网络请求、动态远端 MCP 工具调用和进程终止使用审批卡片。`request_permissions` 可以为本轮或本会话添加规范的文件系统根目录和 URL 感知的网络授权。域名授权不会禁用 shell 网络隔离；只有显式的 `*` 协议加 `*` 域名授权才能做到。`granular` 遵循 `approval_categories`，对配置为 `false` 的类别默认拒绝；`always` 允许所有请求，`never` 拒绝所有请求。在非交互模式下，未解决的审批会被拒绝；只在可信工作区和模型端点下使用 `--approval always`。

PDF 页面截图在可用时使用 Poppler 的 `pdftoppm` 可执行文件；其他 `web__run` 操作不需要它。

## 计划、目标与协作

`update_plan` 接受 `pending`、`in_progress` 和 `completed` 步骤，并强制最多一个活动步骤。TUI 在专用带边框面板中以状态图标渲染计划。计划和目标保存在会话快照和事件日志中。

Goal 工具是刻意独立的：`create_goal` 用于显式请求的长期目标，`get_goal` 读取它，`update_goal` 可将其标记为 `complete` 或 `blocked`。活动目标跟踪模型轮次和输入/输出/总 token 用量；`get_goal` 还报告已用时间和剩余 token 预算。在 TUI 中，活动的显式目标可以在轮次之间自动继续，受其 token 预算和 20 次续跑运行时限制约束。普通开发工作应使用 `update_plan`。

当用户显式请求并行、委托或多智能体工作时，子智能体工具可用。每个子智能体都有隔离的对话历史、可选的角色/模型/推理提示，并且可以递归委托最多三层，共享并发限制。工作区编辑仍处于同一沙箱内；命令和网络调用不能自行批准。智能体历史、角色、输出和状态随会话持久化，崩溃后恢复为中断状态。

## 技能与记忆

技能从以下位置发现：

```text
~/.supercode/skills/*/SKILL.md
<workspace>/.supercode/skills/*/SKILL.md
```

同名的项目技能覆盖用户技能。技能使用 YAML frontmatter：

```markdown
---
name: review
description: Review a change for correctness and regressions
---

# Review workflow

Inspect the diff, run focused checks, and report concrete findings.
```

紧凑的技能目录随每个技能的精确本地 `SKILL.md` 路径一起注入。当用户点名某个技能（使用 `$review` 或纯文本）、任务与其描述明显匹配、或声明的 `triggers` 条目匹配时，模型会被指示在行动前通过常规只读文件工具读取完整文件。运行时会把发现的技能根目录加入只读沙箱，因此模型永远不需要扫描主目录或检查 `.git` 对象来定位技能内容。`/skills` 打开模糊选择器，`supercode skills list|check|install|remove` 管理用户技能。

长期记忆是文件型的；不使用 SQLite、嵌入或向量数据库。其布局为：

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

当 `memory_generate: true` 时，启动会运行一个有界的后台流水线。阶段 1 只考虑最近 10 天内、空闲至少 6 小时的非当前、非归档根会话，每次运行最多处理两个。TUI 优雅退出会先提交当前根会话，再忽略启动时的空闲阈值立即处理它；经过脱敏的内容哈希会在只有会话元数据变化时跳过重复工作。它让配置的提取模型以低推理强度返回包含详细记忆和路由摘要的严格 JSON。阶段 2 按使用次数和新鲜度对最多 256 个成功提取进行排序，包含显式笔记，并让合并模型以中等推理强度生成 `MEMORY.md`、`memory_summary.md` 和可选的技能。空模型名使用当前聊天模型。输入和输出经过秘密脱敏，私有记忆目录维护自己的 Git 基线用于合并 diff。

只有截断到 `memory_max_tokens`（默认 2500）的 `memory_summary.md` 会注入普通轮次。模型按需使用专用只读工具搜索详细记忆。隐藏的记忆引用会从可见的流式输出中移除，并更新 `state.json` 中的 `usage_count` 和 `last_usage`，影响未来的合并优先级。`/remember` 和 `/forget` 创建追加式笔记；它们不直接重写生成的记忆。自动模型生成默认关闭以避免意外 API 成本，而使用已生成记忆默认开启。

项目指令从 Git/项目根目录发现到工作目录。`AGENTS.override.md` 在同一目录中优先于 `AGENTS.md`，可选的备用文件名可配置，总字节上限防止提示词无界增长。`.supercode/instructions.md` 仍是最终的本地项目指令层。`/config` 显示实际加载的文件。

## 提示词架构

SuperCode 使用嵌入的、厂商无关的提示词包。措辞根据 SuperCode 的厂商无关模型边界和工具契约做了调整。

稳定的会话指令包含编码智能体行为、沟通规则、工具纪律、项目指令、`apply_patch` 契约和多智能体编排策略。每轮指令追加活动协作模式、审批和沙箱状态、模型、日期、工作区、上下文预算、选中的技能、记忆、插件、钩子、MCP 服务器和活动目标。两层合并到可移植的 `Instructions` 字段中，每个 Provider 适配器再把它映射到对应 API 的原生指令或 system message 表示。

`default`、`plan`、`execute` 和 `pair` 模式随会话持久化，可通过 `/mode` 更改。`/plan` 仍是进入或离开 Plan 模式、以及显示或隐藏结构化计划面板的快捷方式。Review、目标续跑、Apply Patch、多智能体编排、awaiter、记忆提取和记忆合并提示词都连接到匹配的客户端工作流。Compact 保持确定性和本地性；Guardian 和 Realtime 模板仍然是显式集成点，没有模型支持的运行时。


## MCP、钩子与插件

可信配置可以通过 stdio 或 Streamable HTTP 添加 MCP 服务器。协议协商、传输成帧、分页和类型化内容由官方 MCP Go SDK 处理。工具在启动时发现，命名为 `mcp__<server>__<tool>`。MCP 工具在 `tool_search` 选中它们之前不会进入模型请求，从而减少上下文使用。Resource 和 Prompt 能力作为服务器特定工具暴露。HTTP 凭据可以使用环境展开的请求头，或使用 `oauth_token_command`（其 stdout 提供 bearer token，而不存储在 YAML 中）。`supercode mcp status [name]` 连接服务器并报告可用性；`supercode mcp reload` 校验所有已启用的服务器；`supercode mcp login|logout <name> [...]` 配置或移除 OAuth token 命令。

配置的 MCP 服务器会并发连接和发现工具，并分别应用启动超时。单个服务器失败时会显示警告，健康服务器仍可使用；`mcp status` 和 `mcp reload` 会报告全部失败项，并在校验不完整时返回错误。

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

生命周期钩子支持 `session_start`、`session_end`、`user_prompt_submit`、`pre_tool_use`、`post_tool_use`、`permission_request`、`pre_compact`、`post_compact`、`subagent_start` 和 `subagent_stop`。命令不经过 shell 直接调用，通过 stdin 接收 JSON，并可以返回 JSON 来阻止某个操作、改写提示词/工具参数或注入上下文。钩子失败会停止受影响的操作。

```yaml
hooks:
  pre_tool_use:
    - command: ["./scripts/check-tool.sh"]
      matcher: "^(apply_patch|exec_command)$"
      timeout: 5s
```

本地插件位于 `~/.supercode/plugins/<name>/plugin.yaml`；可信项目也可以使用 `.supercode/plugins/<name>/plugin.yaml`。插件可以贡献指令、技能、MCP 服务器和钩子。`supercode plugins list|install|enable|disable|remove` 管理本地包。钩子可以通过 `supercode hooks ...` 列出、启用、禁用，并按可执行文件 SHA-256 信任；可执行文件一旦改变就不再匹配其信任摘要。这是本地包格式，不是远程市场。

## 配置与优先级

匹配的运行时标量值按以下顺序解析：

```text
命令行标志 > 环境变量 > 可信项目配置 > 用户配置 > 内置默认值
```

用户配置目录使用模式 `0700`，文件使用 `0600`。正常启动期间永远不会覆盖现有配置内容。项目文件 `.supercode/config.yaml` 在显式启用 `--trust-project` 之前会被忽略；`/config` 或 `--config-diagnostics` 显示活动来源。信任按规范工作区路径存储，因此符号链接不能继承另一个项目的决策。Provider 条目是自包含配置块；CLI `--base-url` 和顶层端点字段只适用于旧式单端点模式。

对于 Provider 条目，`token: ${NAME}` 会强制读取该环境变量。未显式指定环境变量时，凭据优先级为 Provider 默认环境变量、该条目的 `token_command`、静态 YAML `token`。对于旧式单端点配置，优先级为 `OPENAI_API_KEY`、顶层 `token_command`、顶层 `token`。`token_command` 直接执行参数向量，有五秒超时，并从 stdout 读取有界 token。它可以桥接 Secret Service、macOS Keychain、密码管理器或其他系统凭据助手。静态 YAML token 会保持为本地明文，因此环境变量或凭据命令更安全：

```bash
export OPENAI_API_KEY="your_api_key"
```

支持的环境变量：

| 变量 | 用途 |
| --- | --- |
| `OPENAI_API_KEY` | 默认 OpenAI Provider token；覆盖旧式顶层 `token` |
| `OPENAI_BASE_URL` | 旧式单端点/CLI 基础 URL |
| `OPENAI_MODEL` | 默认模型 ID 或已配置的 `provider/model` 选择值 |
| `ANTHROPIC_API_KEY` | `anthropic` Provider 的默认密钥 |
| `OPENROUTER_API_KEY` | `openrouter` Provider 的默认密钥 |
| `SUPERCODE_STREAM` | 启用或禁用流式 |
| `SUPERCODE_TIMEOUT` | 请求超时，例如 `30s` 或 `2m` |
| `SUPERCODE_INSTRUCTIONS` | 开发者指令 |
| `SUPERCODE_CONFIG` | 备选 YAML 配置路径 |

CLI 使用 Cobra，遵循 Git 风格命令层级：

```text
supercode [prompt...]                  运行单个任务，或无提示词时打开 TUI
supercode chat [prompt...]             启动交互式多轮聊天
supercode review [focus...]            对当前 Git 变更运行非交互审查
supercode sessions                     列出已保存的工作区会话
supercode config init                  创建用户配置
supercode config diagnostics           显示配置来源、优先级和信任
supercode doctor                       检查认证、沙箱、rg、Git、剪贴板、MCP 和钩子
supercode diagnostics export <zip>     写入脱敏的支持归档
supercode policy list|check|remove     检查持久执行规则
supercode mcp list|get|add|remove|status|reload|login|logout
                                       管理、校验和认证 MCP 服务器
supercode skills list|check|install|remove
                                       管理本地技能
supercode hooks list|trust|enable|disable
                                       检查和信任生命周期钩子
supercode plugins list|install|enable|disable|remove
                                       安装、切换或移除本地插件
supercode completion <shell>           生成 bash、zsh、fish 或 PowerShell 补全
```

全局标志：

```text
--base-url string            旧式单端点 OpenAI API 基础 URL
--approval string            工具策略：on-request、granular、always 或 never
--chat                       启动多轮聊天；终端用 TUI，管道用行模式
--config-diagnostics         打印配置来源和项目信任状态
--context-window-tokens int  估算的模型上下文窗口
--auto-compact-tokens int    自动压缩阈值
--usable-context-tokens int  预留输出空间后的硬性请求上限
--tool-output-tokens int     每个工具结果保留的最大 token 数
--goal-auto-continue bool    在轮次之间继续活动的显式目标
--init-config                初始化配置并打印其路径
--instructions string        开发者指令
--max-retries int            模型 API 瞬时失败的 SDK 重试次数
--max-turns int              每个任务的最大模型轮次；0 表示不限
-i, --image path             附加图片（可重复）
-m, --model string           模型 ID 或 provider/model 选择值
--reasoning-effort string    模型推理强度
--service-tier string        提供商服务层级
--resume string              恢复会话 ID，或 latest
--sessions                   列出当前工作区的会话
--stream bool                启用流式输出
--timeout duration           每个模型请求的超时
--alt-screen                 使用独立 TUI 页面（默认）
--no-alt-screen              保留终端原生回滚
--trust-project              信任并启用 .supercode/config.yaml
-w, --workspace string       工作区根目录（默认：当前目录）
```

运行 `supercode --help` 或 `supercode <command> --help` 查看生成的帮助。为兼容性仍接受旧的单横线长标志，但新脚本应使用 POSIX `--long-flag` 语法。

## Provider 配置

当前的主配置格式使用具名的 `providers` 列表。OpenAI 兼容的 Chat Completions 端点可以这样配置：

```yaml
model: local/your-model
providers:
  - name: local
    provider: openai
    url: http://127.0.0.1:8000/v1
    token: ${LOCAL_MODEL_TOKEN}
    models: [your-model]
stream: true
approval: on-request
max_turns: 0
```

每个模型通过 `provider-name/model-id` 定位。配置的 token 只会发送到该 Provider 的 URL，因此只连接你信任的服务。`openai` Provider 在启用流式时必须实现 OpenAI Chat Completions API 及其纯数据 SSE 格式。

旧式顶层 `url`、`token` 和 `models` 字段仍受支持，便于现有单端点配置继续工作。这种模式的 URL 和模型也可以在单次调用中覆盖：

```bash
go run . \
  --base-url http://127.0.0.1:8000/v1 \
  --model your-model \
  "Hello"
```

### Provider 类型与多端点

添加更多条目，即可在多个 URL 和协议之间选择模型：

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

每个 Provider 都可以配置自己的 `url`、`token` 和可选的 `token_command`；Provider 不会继承顶层旧式端点凭据。`openai` 使用 `/chat/completions`，`openai_responses` 使用 `/responses`，`anthropic` 使用 `/v1/messages`，`openrouter` 使用 `/chat/completions`。Anthropic 的 `url` 末尾可以有或没有 `/v1`。`token: ${NAME}` 会解析指定环境变量；省略 `token` 时，各 Provider 默认读取 `OPENAI_API_KEY`、`ANTHROPIC_API_KEY` 或 `OPENROUTER_API_KEY`。如需使用凭据帮助程序，请省略 `token`，并在对应 Provider 条目上配置 `token_command`。

稳定的模型选择值是 `provider-name/model-id`，例如 `responses/gpt-5-codex`。模型 ID 唯一时也可使用不带 Provider 的写法；同名模型必须使用完整选择值。模型选择器显示为 `gpt-5-codex [in responses]`，Provider 后缀使用弱化颜色。

## 非交互用法

参数产生一次性请求：

```bash
go run . "Explain Go interfaces in one sentence"
```

也可以从标准输入读取提示词：

```bash
echo "Explain the goal of this project" | go run .
```

在管道中使用逐行多轮聊天：

```bash
printf 'First question\nContinue\n/exit\n' | go run . chat
```

需要时禁用流式：

```bash
go run . --stream=false "Hello"
```

自动化可以消费 JSONL 事件、校验最终答案，或只写最后一条消息：

```bash
supercode --json "Inspect this repository"
supercode --output-schema result.schema.json --output-last-message result.json "Return the requested JSON"
supercode --ephemeral review "Focus on concurrency regressions"
```

`--ephemeral` 避免保存一次性会话。`review` 是专用的非交互 Git 审查命令；除非显式的非交互策略允许，否则未解决的工具审批会被拒绝。

恢复当前工作区的最新会话：

```bash
go run . --resume latest
```

列出会话而不需要 API 密钥：

```bash
go run . sessions
```

## 项目结构

```text
main.go                        进程入口、信号处理和退出状态
app/                           Cobra 选项、启动阶段、运行时组装和执行模式
internal/config/              安全的 YAML 创建、加载和校验
internal/credential/          环境、命令和静态 token 凭据解析
internal/agent/               有界的模型/工具编排与审批
internal/permission/          轮次/会话文件系统和网络授权
internal/modelcatalog/        可配置的模型限制和能力
internal/tool/                工具注册表和工作区范围内的内置工具
internal/mcp/                 官方 MCP Go SDK 集成与工具适配器
internal/hook/                可信生命周期钩子运行时
internal/plugin/              本地扩展包发现
internal/collaboration/       递归、持久化的子智能体生命周期
internal/taskstate/           结构化计划和显式目标状态
internal/provider/            厂商无关的请求、响应和流类型
internal/provider/openai/     OpenAI 兼容的 Chat Completions 适配器
internal/provider/openairesponses/ OpenAI Responses 适配器
internal/provider/anthropic/  Anthropic Messages 适配器
internal/provider/openrouter/ OpenRouter 适配器
internal/session/             索引 JSON 快照、资产和压缩事件日志
internal/skill/               SKILL.md 发现与精确本地路径注入
internal/memory/              文件型提取、合并、检索和使用反馈
internal/tui/                 Bubble Tea 状态、斜杠路由、转录/会话状态和渲染
```

要添加供应商，在 `internal/provider/<name>` 下实现 `provider.Provider`。供应商 SDK 类型不得泄漏到公共 provider 层、CLI 或 TUI。

## 验证

```bash
go test ./...
go vet ./...
go build .
```

Provider 测试使用内存 HTTP 传输，CLI 集成测试在本地 socket 可用时使用仅 loopback 的 mock 服务器。测试不访问真实 API，也不消耗额度。
