# Architecture

## Trust boundary

LLM、Prompt、Skill、远程输出和 MCP Client 都不属于可信计算基。唯一能够执行 SSH 的入口是 `service.Service`，它固定执行以下顺序：

1. 从 SQLite 按 `host_id` 解析目标与认证方式，忽略模型提供的任何连接凭据。
2. 规范化并校验请求，绑定原始载荷与连接配置的 SHA-256。
3. 应用审批模式：Manual 交给用户，Auto 将当前用户请求和精确操作交给审批 Agent，Full access 直接执行；审批 Agent 要求人工判断、不可用或返回无效结果时回退用户审批。
4. 仅在实际执行前解密所需 SSH/sudo 密码，获取并发令牌，通过绑定的内置 SSH Transport 执行。
5. 加密原始请求和输出，生成脱敏视图并追加审计事件。

Eino Tool、MCP Tool、HTTP 和 CLI 都是这个 Service 的适配器。Eino 与 MCP 复用 `internal/agenttool` 中的输入契约、Schema 和执行适配器，并通过 `internal/toolresult` 共享 Service 错误到模型结果的映射；Eino 的 interrupt、checkpoint 和审批恢复只保留在 `internal/agent`。模型侧执行结果只保留状态、有效输出和必要标识；预期失败额外返回 `code/message/retryable` 与可用的结构化校验信息。只有上下文取消或内部持久化损坏会成为 ToolNode fatal error。

这里的 MCP Tool 分为两个方向。`opsnerva mcp` 通过 stdio、设置中的 MCP Server Mode 通过同一 HTTP 服务上的 `/mcp`，把受控 SSH Service 暴露给 MCP Client，因此完整复用输入校验、审批模式和审计。只读工具通过 MCP annotations 明确标记；`ssh_shell` 使用独立 MCP surface，`ssh_tunnel` 复用已有转发状态，`ssh_history` 只能读取当前 MCP 会话创建的运行。HTTP 使用有状态 Streamable HTTP，由服务端为每次 initialize 分配高熵 `mcp_sess_*`；stdio 进程拥有一个独立会话。客户端上报的 name/version 仅作展示，不作为身份认证，共享 Bearer Token 仍是 HTTP 访问边界。设置开关按请求即时生效，Token 仅在生成时返回，SQLite 只保存 SHA-256 摘要。管理员配置的外部 MCP Server 则属于独立信任域：它的工具在远端/子进程自身权限下执行，不自动继承 OpsNerva 的审批控制。Web 会明确提示该边界，只有启用状态为 ready 且未被 func 管理单独关闭的外部工具才进入主 Agent，审批 Agent 仍保持无 Tool。

App 控制面通过 loopback HTTP API 连接本地 Sidecar。`auth.password` 非空时，除登录状态、登录和退出接口外，普通 `/api/v1` 端点都要求进程内随机会话 Cookie；Cookie 为 `HttpOnly`、`SameSite=Lax`，TLS 下同时设置 `Secure`，会话只保存令牌 SHA-256 与过期时间，重启后失效。配置密码使用常量时间比较，登录失败按来源地址限速。未配置密码时保持本机无登录模式。MCP HTTP 使用独立 Bearer Token，不接受控制面 Cookie；MCP stdio 与 CLI 仍属于本机进程边界。

模型提供商、SSH 主机和代理使用带版本号的统一 JSON 迁移契约。Store 在只读事务中生成一致快照，Service 通过专用 DTO 明确允许导出的字段，不序列化运行时 Host Key、派生状态或数据库密文。Service 使用本机 Master Key 解密凭据并写入迁移 JSON，不依赖控制面登录，也不要求单独的迁移密码。导入先完整解析并校验 ID、名称、模式、凭据和跨资源引用，再在单个 SQLite 事务中合并；任何错误整体回滚，未包含的现有资源不会删除。目标库用自己的 Master Key 重新加密凭据，因此迁移包不携带、也不依赖源机器 `master.key`。导入和导出只属于 Web 控制面，不暴露给 LLM 或 MCP Tool。

人工审批说明使用原有 `ApprovalAgent`；Auto 决策使用新增的 `AutoApprovalAgent`。两者都是 `MaxIterations=1`、无 Tool 的独立 Eino `ChatModelAgent`，各自拥有 Runner、Prompt、Service 接口、并发槽和可用状态，互不复用。`ApprovalAgent` 仅异步生成人工审批页的操作与风险说明，不参与自动决定。`AutoApprovalAgent` 接收由 Go context 绑定的当前用户请求、精确操作、目标能力、当前任务和请求摘要；Tool reason 与任务不能扩大用户授权。它结构化返回 `allow/reject/manual`。Auto 仅在完整 `allow` 时执行，明确 `reject` 时终止，`manual`、缺少当前用户请求、不可用、超时或格式无效时回退用户审批。

## Packages

- `internal/sshx`：进程内 SSH 认证、严格 host key、SFTP、SOCKS5/HTTP 代理、ProxyJump、输出上限和连接探测。
- `internal/websearch`：独立的 Tavily Search/Extract Client，负责请求校验、代理、重试、并发限制、请求合并、响应裁剪和外部内容脱敏。
- `internal/service/websearch.go`：Web Search 配置、凭据解密、Client 调用与审计持久化；不接入审批。
- `internal/fileedit`：SSH 与 Workspace 共用的文本块规范化、展示 diff、审批载荷一致性检查和字节保留替换算法；不执行 I/O。
- `internal/workspacefs`：按 Workspace 根目录解析路径、读取/搜索/枚举文件、生成预览、打开下载流、保存普通文本、暂存/提交编辑及同步文件；不依赖 Service、Store 或 SSH。
- `internal/service`：审批状态机、摘要绑定、执行并发、任务、审计事务，以及外部 MCP Client Session 与动态工具生命周期。
- `internal/store`：SQLite hosts、runs、approvals、events、chat、加密模型/MCP 配置与 Eino checkpoints。
- `internal/agenttool`：Eino 与 MCP 共用的 Tool 输入契约、Schema、结果投影和 SSH/Workspace/Web/History 执行适配器。
- `internal/toolresult`：Service、Store 和 Skill 错误到模型/MCP 结构化结果的共享映射。
- `internal/agent`：Eino ChatModelAgent、Tool 装配、HITL checkpoint、消息历史、事件流与并发安全的 Runner 热切换。
- `internal/mcpserver`：官方 MCP Go SDK stdio 与 Streamable HTTP 适配器。
- `internal/httpapi`：本地 HTTP API、SSE、应用状态/交互终端 WebSocket 和嵌入 Go 二进制的 React 静态资源。
- `internal/observability`：`slog` 多路 Handler、字段脱敏、JSONL 文件轮转与 Web 内存日志缓冲。
- `internal/skills`：可上传、永久删除和启停的无权限运维方法论注册表。

## Service organization

`internal/service` 的第一阶段拆分只调整代码与测试的归属，不改变公开接口、审批行为、SQL、事件协议或资源生命周期。`service.go` 只保留共享依赖与状态、构造函数、Store 访问和关闭入口。

| 文件 | 职责 |
| --- | --- |
| `hosts.go`、`ssh.go` | 主机配置与 Host Key 管理；SSH 连接解析与凭据解密 |
| `chat_sessions.go` | 会话、消息、附件读取及会话管理 |
| `model_providers.go`、`models.go` | 提供商配置与凭据管理；模型发现与连接测试配置 |
| `system_settings.go` | 系统配置与 MCP HTTP 访问令牌 |
| `execution_request.go`、`command_validation.go` | 请求规范化、摘要与参数校验；命令契约校验 |
| `execution.go` | 提交、审批分流、执行与并发限制 |
| `history.go`、`audit_history.go` | 运行历史查询与原文读取；审计查询、删除与追加 |
| `recovery.go` | 启动时恢复中断的任务、运行和工具记录 |
| `websearch.go` | 动态解析 Web Search 配置与凭据、调用独立 Client、写入调用审计 |
| `workspace.go` | Workspace 注册管理、配置快照与能力目录 |
| `workspace_files.go`、`workspace_edit.go` | Agent 文件调用与输出协议；编辑审批入口、校验器执行与文件事务编排 |
| `workspace_browser.go` | 管理端文件 DTO、操作入口和文件监听 |
| `workspace_transfers.go`、`workspace_upload.go` | SSH 与 Workspace 传输编排；上传流与原子落盘 |
| `workspace_shell.go` | Workspace Shell 后端选择、命令构造及执行编排，生命周期仍复用现有 Shell 实现 |

文件列表、任务状态判断等逻辑归回已有的 `files.go`、`tasks.go`；共享凭据字符校验位于 `input_validation.go`。测试按相同职责归档，`service_test.go` 仅保留共享 Transport fake 与测试服务构造器；审批测试区分决策、说明生成和批准后执行。

第二阶段已完成 Web Search 的组件解耦：`Service` 持有一个 `websearch.Client`，并发槽与请求合并状态归 Client 所有。每次调用传入当前配置的明文快照，不向 Client 传递 `Service`、Store、审批接口或持久化回调。Client 返回结果、结构化错误与不含原始查询/URL 的 `CallInfo`；配置或输入校验失败时不生成调用审计，已验证调用的成功、失败与取消由 Service 通过原审计通道记录。Search/Extract 不走人工或自动审批，关闭配置阻止新调用，不改变既有在途请求的取消与超时语义。

请求协议、输入校验、输出预算和并发测试直接构造 Client，无需数据库；配置动态生效、凭据加密、代理、审计与审批隔离由 Service 集成测试覆盖。HTTP 和工具结果映射直接使用 `websearch.ProviderError`，不保留旧 Service 错误类型的兼容别名。

第三阶段已完成 Workspace 基础文件层解耦。`workspacefs.FS` 只持有根目录，不持有 Service、配置注册表、审批或审计回调；调用方每次按当前 Workspace 配置构造文件操作对象。读取返回内容、文件信息、偏移和完整 SHA256；工具输出标记与 HTTP DTO 由 Service 组装。路径输入错误在 Service 边界映射为原有 `InputValidationError`，文件不存在等 I/O 错误保持分类。

底层沿用现有路径/符号链接检查和原子文本保存流程，不将其视为操作系统级沙箱。读写权限与审批前后校验仍归 Service；文件监听频率、传输进度、Shell 生命周期和 Agent 编辑校验器未改变。目录同步实现移至文件模块，上传和 Agent 编辑共用该实现，不保留旧副本。文件模块单测不需要数据库，Service 与 HTTP 测试验证原有协议、审批和权限边界。

第四阶段已完成文件编辑解耦。`fileedit` 承接原本放在 Service 的共享文本契约，SSH 与 Workspace 直接调用同一套规范化、diff 和一致性检查，不保留旧函数的兼容包装。纯算法测试迁入该模块，远端脚本生成和执行仍由原 Service 路径负责。

Workspace 编辑采用 `workspacefs.FS.PrepareEdit → Service 校验器 → Edit.Commit`：文件模块持有暂存文件、原始摘要和提交结果，Service 负责当前写权限、审批载荷校验、校验器选择/执行、工具退出码与输出标记。调用方始终 `defer Edit.Close()`；拒绝、取消或冲突只清理暂存文件，创建仍使用排他硬链接，替换仍在校验后复核完整内容摘要。文件模块不接收 Service、Store 或审批回调，事务也不跨调用持久化；不把摘要复核视为原子 CAS 或系统级文件锁。

此阶段补齐准备与提交前的取消检查；目录同步失败发生在提交之后，返回明确的提交后错误，不再冒充退出码 74 的校验失败。仍然只由校验器失败产生 74，编辑冲突产生 75；不需要脱敏的错误保留原有路径输入/取消分类。测试覆盖字节与权限保留、新建不覆盖、校验期间目标变化、审批载荷篡改、拒绝/取消清理以及工具输出与持久化结果。校验器集成测试使用 Go 测试进程，不依赖 Bash。

其余编排与运行时状态仍由 `Service` 持有。后续按依赖顺序逐阶段推进，每阶段独立验证：

1. 继续解耦 Workspace 上传/删除落盘及注册状态；保留审批与审计的编排边界，不把整个 `*Service` 传给新组件。
2. 分别收拢 Shell、Tunnel、外部 MCP Client 的运行时状态和关闭职责，保留用户交互与 Agent/MCP 审计的边界。
3. 最后梳理 Execution、Approval、Task 的交叉调用，统一完成与取消路径，并分离业务状态更新和事件发布，再调整调用方装配。

## Dynamic extensions

Skill Registry 位于控制面数据目录，每个 Skill 目录必须包含 `SKILL.md`，启用状态写入独立 `skill.json`。管理员列表包含全部 Skill；主 Eino Agent 的通用 `skill` 可用于任意任务领域，不传 `name` 时列出启用项，传入精确 `name` 时加载完整内容。Skill 只提供指导，不扩大权限或覆盖系统规则。OpsNerva 自身的 MCP Server 不暴露 Skill，删除是不可恢复的物理删除。

主 Agent 的 func 启用状态保存在 `agent_tool_settings`。未写入状态的 func 默认启用；管理员可在 Loaded functions 中逐项关闭或重新启用。每次修改都会写入审计并重建 Eino runner，关闭项仍保留在管理目录中，但不会传给 ChatModel，也不会注册到 ToolNode。

外部 MCP 配置保存在 `mcp_servers`。command、args、cwd、URL 和秘密键名是可管理元数据；环境变量、HTTP Header、OAuth 动态客户端凭据及 Token 整体使用 AES-256-GCM 加密。Streamable HTTP OAuth 使用授权服务器发现、动态客户端注册、PKCE 和 refresh token；回调 state 与待完成流程只存在内存，修改 Endpoint 时删除原 OAuth 会话。启动时 Service 尝试连接所有 enabled 配置；单个服务器失败只记录 `error` 状态和结构化日志，不阻止控制面启动。

stdio 通过 `exec.Command(command,args...)` 启动，不解析 Shell；Streamable HTTP 使用官方 MCP Go SDK transport。连接成功后分页执行 `tools/list`，把服务器 JSON Schema 转为 Eino ToolInfo，并生成不超过模型限制的稳定名称 `mcp__<server-id-hash>__<sanitized-tool-name>`。动态 wrapper 每次调用都重新检查服务器的 ready Session，因此 Disable/Delete/重连失败会立即阻止旧 Runner 中的残留句柄；随后 Runtime 热重载会从模型函数 Schema 中移除它。调用结果限制为 128 KiB，并记录不含参数或输出的 `mcp_tool_called` 审计事件。

## Command execution

`ssh_exec` 接收 program 与 args，服务对每个参数进行 POSIX 单引号编码，并通过 `golang.org/x/crypto/ssh` 在进程内建立连接，不调用本地 SSH 程序或 shell。同步 Tool 结果默认返回完整 stdout/stderr；调用方可设置 `max_output_bytes` 和 `output_view=head|tail|head_tail` 仅精炼模型视图，返回值同时携带每个流的总字节数、省略字节数和 `output_limited`，因此不存在静默截断。

`ssh_exec` 只接受单个非交互可执行文件及分离的 argv；Shell 语法和多步骤操作使用 `ssh_run_script`，提示或终端 UI 使用 `ssh_shell`。`ssh_run_script` 将脚本通过 stdin 传给远端 Shell：已安装 Bash 时使用 Bash，否则使用 POSIX `/bin/sh`。服务端使用 Shell AST 检查并拒绝脚本内直接调用 sudo；提权只能使用结构化的 `elevated` 参数。后台执行返回 task ID；未显式指定 `timeout_seconds` 时，后台命令使用 `max_timeout_seconds`，同步命令使用 `sync_timeout_seconds`。`ssh_task status` 可在 Service 内阻塞等待终态或指定字节偏移后的新输出，单次最长 60 秒，并可只返回 stdout/stderr 增量；等待截止只返回仍在运行的任务和 `wait_deadline_reached=true`，不会终止或改写任务。

`ssh_tunnel` 的 `start` 进入同一套 Run、审批模式和加密审计状态机；`list` 与 `stop` 直接操作进程内 Tunnel Registry。本地转发由控制面在指定 IP 建立 TCP Listener，再以 `direct-tcpip` channel 连接主机侧目标；反向转发通过 `tcpip-forward` 请求 SSH 服务端监听指定 IP，并接收 `forwarded-tcpip` channel 后回拨控制面侧目标。Registry 将用户创建的长期隧道状态与一次 SSH 连接对应的 Listener、Client 和连接集合分离；连接异常结束时关闭该次运行时，保留隧道 ID、已分配端口和累计流量，以 1–30 秒指数退避重建运行时。每次重连重新从 Store 解析主机、代理、ProxyJump、认证和 Host Key 配置，不持有旧凭据，也不生成重复执行 Run。`running`、`retrying`、`failed`、`stopping` 和 `stopped` 生命周期通过应用 WebSocket 主动发送连接 delta，累计流量仅每 5 秒采样一次。`stop` 与 Service Shutdown 共用生命周期取消，能够终止退避等待、连接建立、Listener、SSH Client 和全部活动连接并等待 worker 退出；不把隧道恢复为跨重启持久状态。

无 PTY 的交互式 Shell、编辑器与 `systemctl edit` 会在 Service 层拒绝；apt/dnf/yum/pacman 的变更操作必须显式提供对应非交互参数。脚本、argv、环境和路径还有独立大小与格式上限，检测到秘密的环境变量不会进入执行请求。

## Transactional files and Workspace

Workspace 与 SSH 文件读取共享 `tail_lines` 语义。Agent 侧 Workspace 生命周期包括审批控制的 `workspace_file_delete`、Workspace→SSH 的 `workspace_file_upload` 和 SSH→Workspace 的 `workspace_file_download`；两个传输方向与主机间传输共用 `{transferred,total}` 字节进度 reporter 和现有文件传输卡片事件。下载绑定远端 SHA256，拒绝符号链接、超过 100 MiB 的源和已存在的本地目标，并在同目录临时文件校验后原子提交。文件编辑先规范化 UTF-8 BOM 与 CRLF，再唯一匹配模型提交的原文块并由 Service 生成 diff；UTF-16 明确拒绝。`workspace_shell` 省略 cwd 时在请求中固定绑定 `.`，执行环境统一声明 UTF-8，Windows PowerShell 脚本继续通过系统临时目录中的 BOM 文件启动，但进程工作目录始终是 Workspace 根。

`ssh_file_read` 在同一次受审计操作中返回有界内容、mode/owner/mtime 与 SHA256，`workspace_file_read` 使用相同的范围语义。普通读取默认限制为 128 KiB；未到文件末尾时通过 `has_more/next_offset` 显式分页，`full_content=true` 才取消默认页限制。`offset_bytes` 非负时是从文件开头计算的零基偏移，负数表示读取文件末尾对应字节数，返回元数据记录解析后的实际非负偏移。两者都以可选 `pattern` 切换到字面量搜索模式，并支持上下文与结果行数参数；搜索和范围参数互斥，独立的 `ssh_file_search`、`workspace_file_search` 不再注册到 Agent 或 MCP。内部仍以不同执行模式保留参数校验和审计语义。现有文件由 `ssh_file_edit` 或 `workspace_file_edit` 以 `old_text/new_text` 编辑；不提供专用的新建文件 Tool。Service 在审批前规范化文本、生成 diff 并计算新增、删除行数，`ExecRequest.change` 是审批、审计和 Web 展示的变更来源。Tool 参数 `validator_id` 只能引用启动配置中的 scope 对应 ID，配置项以固定 program/args 执行并拒绝 Tool 提供的 Shell 命令。远程 Shell 事务脚本在批准后才生成：同目录写入并同步临时文件、确认原文唯一、应用后运行白名单 validator，再原子提交。编辑链路不校验旧文件 SHA、不创建持久备份、不写 `file_operations`，也不提供恢复 Tool。

`ssh_file_transfer` 由控制端分别建立源、目标两条内部 SSH/SFTP 连接并用 `io.Copy` 中继，不要求远端主机互通，不调用本地或远端 `scp`，也不在控制端落盘。请求以目标主机作为 Run 主机，同时绑定源主机 ID、源路径及 SHA256、目标路径和两端 `ssh_connection_digest`。未提供目标 SHA256 时只允许创建新文件；提供后只允许替换该精确版本，并在写入前后复核。Transport 拒绝符号链接和非普通文件，先写目标同目录的随机独占临时文件，流式计算源 SHA256，通过后使用 SFTP rename 提交；进度按字节事件发送，冲突、取消和超时会清理临时文件。一次传输只占一个全局执行槽，并按稳定顺序同时占用两台主机的并发槽，避免反向传输死锁。

Workspace 在 `workspace_dir` 下按 ID 托管；SQLite 只登记 ID、权限和时间戳，`chat_sessions` 持久化当前绑定。目录固定派生为 `<workspace_dir>/<id>`，API、审计和模型上下文均不返回真实根路径。`workspace_list` 不存在，模型侧 Workspace Tool schema 不含 `workspace_id`，只从可信会话上下文解析绑定；没有会话语义的 MCP Server 不注册这些 Tool。上传与下载限制为 100 MiB，拒绝敏感路径、符号链接和覆盖，通过同目录临时文件、`fsync`、SHA256 校验与原子 hard-link 提交。`workspace_file_upload` 绑定本地源版本后发送到 SSH，`workspace_file_download` 绑定远端源版本后写入 Workspace；绝对本地路径不会序列化。`workspace_file_delete` 拒绝根目录，非空目录要求 `recursive=true`。Web 文件面板通过独立附件接口流式下载普通文件，响应使用原文件名、`no-store` 与 `nosniff`；文件列表和预览窗口共享该入口。Web 文件面板与 Agent、Shell、外部程序共享 SSE 文件事件刷新链路。每个 Workspace 使用隐藏的受管目标复用 Run/Approval/Audit 状态机。

`workspace_shell` 是唯一开放给模型的本地 Shell，支持一次性 `run` 以及 `start/input/output/list/interrupt/close` 交互式 PTY。`input/output` 的 `wait_seconds` 是读取前的可取消延迟，范围 0–600 秒、默认 5 秒；定时期间的输出事件只实时推送到 Web，不会唤醒工具，定时结束后按调用开始时确定的序列游标读取一页。管理员在 SQLite 持久化的 System 设置中明确选择 `sandbox`、`host` 或 `disabled`，Linux 默认 `sandbox`，Windows 默认 `host`。启动或运行时解析出的实际后端写入 `ExecRequest.workspace_shell_backend`，和 Workspace ID、相对 cwd、环境及脚本一起进入加密审批摘要；执行前再次读取设置，后端不一致即拒绝。交互会话复用 SSH 终端的事件序列、ANSI 输出、尺寸变更、Ctrl+C 与持久化状态，但以 `kind=workspace` 记录 Workspace 和后端；没有 TTL。Web 以 WebSocket JSON 控制帧发送输入、实际尺寸和中断，服务端以带序列号的二进制帧发送脱敏后的原始 PTY 字节，重连通过 `after` 游标续传；相邻输出分片在单个 SQLite 事务中批量提交，较大的事件载荷以独立 Zstandard 帧存储并在读取时透明解压，旧 TEXT 记录保持兼容。Bubblewrap 交互模式复用外层专用 PTY 的 session/controlling terminal，不再创建第二个 session，因此 Bash job control和全屏程序可用；原始 ANSI 事件保留给 Web 终端，Agent 适配器使用跨块状态机移除控制序列。启动和一次性脚本都遵循当前审批模式，不再进行等级分类。

App 中由用户直接新建的 SSH/Workspace Terminal 与上述 Agent Tool Shell 只共享 PTY、尺寸、序列和实时订阅组件。App Terminal 不创建 Run、Approval、Audit、`ssh_shell_sessions` 或 `ssh_shell_events`，最近 2 MiB 输出仅保存在进程内；浏览器 WebSocket 断开后以原 Shell ID 和事件序列游标重新附着，SSH 连接或本地 Workspace PTY 异常结束时也保留同一逻辑 Shell，手动重连只替换其底层运行代次，不创建第二个 Shell。普通 SSH 无法在连接死亡后恢复原远端进程，因此底层 PTY 会重建，但卡片、Shell ID、输出历史和事件序列保持不变；正常退出、主动关闭或服务重启后不提供伪恢复。用户按键不会形成输入事件，也不经过面向模型的凭据检测、密码提示阻断或输出脱敏。Agent/MCP Shell 继续使用 SQLite 历史、响应游标、凭据隔离和执行审计；用户接管这类 Shell 输入密码时仍使用不保存明文的私密通道。

`web_search` 和 `web_extract` 共用管理员保存在 `web_search_settings` 中的 Tavily 配置，但可由 func 管理分别启停。Tavily 设置只保存共享 `proxy_id`，运行时从 `proxies` 解析 HTTP、HTTPS、SOCKS5 或 SOCKS5H 地址及加密凭据；请求禁用环境代理，选中的代理失败时不会回退直连。查询、域名过滤条件和待提取 URL 会离开本机。管理员结果数是上限，模型省略结果数时默认取 5。搜索支持 topic、depth、相对/绝对日期范围和高级分片；提取一次接受最多五个公开 HTTP/HTTPS URL，并支持 query、depth 和相关分片。URL 输入与提供方返回值均会规范化、去重并拒绝凭据、localhost、私网和链路本地地址。

提供方原始响应限制为 2 MiB，错误正文限制为 4 KiB；模型可见的搜索和提取结果分别限制为约 32 KiB 与 48 KiB，并携带逐项及总量裁剪元数据。Eino Tool Reduction 在 Summarization 之前运行，历史 Web 结果使用结构化 reducer 保留查询、标题、URL、日期、失败项和请求元数据，只压缩正文；持久化事件保持原样。请求最多进行一次临时网络、短期 429 或 5xx 重试，受四路并发上限保护，相同在途请求会合并，但不缓存完成结果。全部外部内容都会执行当前凭据精确脱敏并标记为不可信。审计保存查询或规范化 URL 列表的 SHA256、请求 ID、credits、HTTP 状态、重试与字节统计，不保存正文、凭据或完整 URL。

Sandbox 后端仅在 Linux 使用配置的 Bubblewrap；不存在或 namespace 创建失败时关闭失败，绝不回退到 Host Shell。沙箱新建 user/mount/PID/network namespace、丢弃 capabilities、禁用嵌套 user namespace 和网络，只读挂载 `/usr` 与动态链接库目录，创建独立 `/proc`、`/dev`、`/tmp`，并按 Workspace access 只读或读写挂载到 `/workspace`。预存的 `.env*`、`.ssh`、`.opsnerva-*`、`data`、`master.key` 与 credential 命名路径，以及 socket、FIFO 和 device 等特殊文件，在 mount namespace 内被遮蔽。

Host 后端直接以服务账户执行，拥有宿主机文件系统与网络权限；Unix 选择 Bash，Windows 依次查找 `pwsh.exe` 与 `powershell.exe`。Host 仅允许 `read_write` Workspace，并遵循当前审批模式。两种后端都使用清理后的环境、有界输出、统一脱敏并隐藏 Workspace 宿主根路径。配置中固定的 Workspace validator 仍使用 argv 和固定环境单独执行，不经过 Shell。

目标主机和最多四级跳板链的非秘密连接字段及更新时间组成 `ssh_connection_digest`，与命令一起进入请求摘要；人工批准后修改地址、用户、认证方式、known_hosts、网络代理或跳板链会导致执行失败。主机间文件传输对源端和目标端分别计算并校验该摘要。

内置实现使用 `golang.org/x/crypto/ssh`、`knownhosts` 和 `github.com/pkg/sftp`。密码只作为进程内 AuthMethod；Keyboard Interactive 只回答一次无回显的密码提示。Unix Agent 连接 `SSH_AUTH_SOCK`，Windows Agent 通过 named pipe 连接系统 OpenSSH Agent。Web/CLI 上传的未加密 OpenSSH 格式私钥限制为 1 MiB，使用 AES-256-GCM 写入 `private_key_cipher`，对外只返回 `has_private_key` 并只在内存解析；不接受或保存宿主机私钥路径。主机只保存共享 `proxy_id`，连接时解析 SOCKS5、SOCKS5H 或 HTTP CONNECT 参数；代理密码只在内存解密。ProxyJump 只能引用注册主机，逐跳验证 host key、检测环路并限制最大深度；与网络代理组合时，代理只负责连接第一台跳板机。

内置实现通过未认证握手扫描协商出的 host key，信任时重新扫描并精确比较 SHA256 指纹，再以 `0600` 追加和同步 known_hosts。未知 key 与 key mismatch 均关闭失败。命令和主机间文件传输建立独立连接；操作端 SFTP 文件浏览器按连接配置池化完整的 SSH 连接及 SFTP 会话，每组最多 2 个，空闲 2 分钟后回收。每次操作独占一个池单元，满池等待响应请求取消；归还后解除旧请求的取消绑定。取消或连接故障先关闭该单元最外层 SSH 连接，解除子系统初始化与文件操作阻塞，不影响另一单元；失效单元不再复用，不自动重放文件操作。15 秒 keepalive 连续超时会断开。

文件浏览器目录读取在切换目录、离开页面或窗口隐藏时取消，重新激活后核对目录。删除成功直接移除本地列表条目，不额外等待全量目录刷新；失败或取消后重新读取相关目录以核对部分删除。操作端删除通过原 DELETE 接口返回 202 任务，服务端独立持有生命周期，切页或 HTTP 断开不取消任务；取消使用 POST `/api/v1/sftp-deletions/{id}/cancel`。同一主机只允许一个活动删除任务，全局最多 8 个；内存仅保留最近 64 台主机的最后结果，不持久化、不进入 Agent 会话或审计。递归扫描逐目录发现文件，以 8 个 worker 有界并发删除，目录按层级从深到浅删除；子项失败时跳过祖先目录，保留已确认删除、失败、跳过计数和首个错误。`sftp_deletions` 通过现有应用 WebSocket 初始/重连快照和带 revision 的 delta 推送，过程更新最多每 200ms 一次，无轮询；前端列表只订阅活动状态转换，小卡片单独订阅进度。

SFTP 与 Workspace 文件列表共享固定行高的虚拟列表，仅挂载视口及上下各 6 行，并额外保留当前焦点行；支持方向键、Home/End、PageUp/PageDown 与跨窗口 Tab 导航。SFTP 只格式化可见行时间，复用 `Intl.DateTimeFormat`，虚拟行不播放入场动画。上传/下载状态仓库按主机或 Workspace 原地更新记录，开始、结束与上传完成版本独立发布；字节进度只通知进度组件，每 200ms 合并一次，最后一个可见订阅离开即清除待刷新定时器，任务本身继续。Workspace 目录监听不依赖预览状态，读取中到达的失效事件合并为一次后续读取；页面、窗口隐藏或侧栏折叠时取消目录/预览读取并关闭监听，恢复可见后重新核对。

下载封装保留 `io.WriterTo`，上传显式使用并发写入，单文件请求并发设为 32；上传先完整写入临时文件再提交，失败后释放原租约，用独立的 5 秒清理上下文删除临时文件，避免满池清理死锁。`component=sftp` 的请求诊断记录池等待、连接获取、SFTP 初始化、目录读取、实际删除与租约归还的分段耗时，通过 `request_id` 关联 HTTP 总耗时，不写入审计；成功删除记录为 INFO，慢操作（至少 2 秒）或失败为 WARN，快速读取及请求取消为 DEBUG。

双后端到内置单后端的升级是显式破坏性迁移。检测到旧 `transport_backend`、`config_alias`、自由格式 `proxy_jump` 或 `identity_file` 列时，Store 会清理旧主机及依赖的 runs、approvals、tasks，再删除这些列，不保留运行时兼容分支。废弃的 `file_operations` 表会在启动迁移时直接删除。

提权是 `ExecRequest.elevated` 的结构化属性，不是任意命令字符串。通过当前审批模式后 Transport 才按主机配置包装为 `sudo -n -- /bin/sh -c ...` 或 `sudo -S -p '' -- /bin/sh -c ...`。sudo 密码只拼接到远端 stdin，不进入请求摘要、审计 JSON 或模型工具参数。

## Approval state machine

```text
Agent / MCP request
   ├── Manual ── approval_required ── approved ── running ── completed / failed
   │                              └── rejected / expired
   ├── Auto ── AutoApprovalAgent allow ── running ── completed / failed
   │          ├── reject ── rejected
   │          └── manual/unavailable/invalid ── approval_required
   └── Full access ── running ── completed / failed
```

人工审批原因可选。系统不保存会话级授权。审批写入后，服务再次解密原始载荷并重新计算摘要，避免 TOCTOU 或载荷替换。

Eino Agent 的人工审批使用框架原生 HITL。Tool 中间件发现 `approval_required` 后调用 `StatefulInterrupt`；Runner 先把精确 Tool 状态写入独立 checkpoint，Runtime 再以单个事务公开该 checkpoint 的全部审批并暂停当前对话队列。同组审批按创建顺序逐个展示，批准或拒绝只持久化决定；整组全部决定后，`ResumeWithParams` 才按全部 interrupt ID 一次性恢复原 Tool，等待中的 Tool 不会被转换为失败结果。批准载荷由原子 claim 保证只执行一次，拒绝说明以 `operator_instruction` 返回模型。CLI、MCP、后台任务和直接 HTTP 执行保持非阻塞的 `approval_required` 契约。后台任务通过执行生命周期事件更新状态，不轮询 approval/run 表。

HTTP Chat Handler 使用保留 request logger/value、但移除浏览器取消信号的后台 context。浏览器断开后只停止 SSE 写入，Runner 和对话队列继续运行；Runtime 按 session ID 拒绝同会话并发运行。Web 刷新后通过 `GET /api/v1/chat/{id}/state` 和带事件游标的 SSE 重连恢复。等待审批的用户消息与 Tool 卡片分别持久化为 `waiting_for_approval` 和 `approval_required`。进程重启后，全部决定但未恢复的审批组会从 checkpoint 继续；部分决定的组继续等待剩余审批，尚未获得 interrupt ID 的不完整审批会明确中断并清理。失去进程内执行所有者的 created/running Run 会标记为 `interrupted`，避免恢复时重复执行。活动会话不能删除。独立 SSH Task 无法重新附着旧 SSH 进程，重启时仍明确标记为 `interrupted`。

主 Agent 已产生 Tool 结果但终止正文为空时，不允许重跑带 Tool 的 Runner。Runtime 改用独立的 `FinalAnswerAgent`，仅将当前用户请求、本轮已持久化的脱敏 Tool 结果和最新任务状态作为不可信 JSON 数据交给同一模型。该 Agent 为 `MaxIterations=1`、无 Tool、无 checkpoint，只能补生成最终用户回复；结果成功后按正常 Assistant 消息持久化，失败则保留原轮次和 Tool 结果并返回明确错误。

## Agent tasks

复杂工作使用项目自有的顺序计划：`ops_plan_create` 创建目标和 2–8 个步骤，`ops_plan_step_update` 完成或跳过当前步骤并自动推进下一步，`ops_plan_revise` 替换未完成部分。已完成和已跳过步骤保留，全部完成后计划仍可查询，直到新计划替换。计划不包含依赖图、负责人或阻塞状态。

`internal/domain/plan.go` 仅定义共享的计划数据类型。`internal/plan` 集中维护计划输入规则、当前步骤查询、顺序推进、历史保留和校验错误，只依赖 `domain`；转换返回独立快照，可脱离数据库与 Agent 框架测试。`internal/service/plans.go` 负责可信会话绑定、审计和计划校验错误到应用错误的转换，不依赖工具层。`internal/store/plans.go` 负责事务、持久化和提交后的事件发布，状态校验仍在取得写锁后执行，避免并发更新使用旧状态。会话 HTTP 接口和 Chat state 组装集中于 `internal/httpapi/chat_sessions.go`。Web 的 `SessionPlanPanel` 自己持有计划状态，通过现有 WebSocket 订阅 `plan` 字段，计划更新不再写入 `ChatPage` 状态。Runtime 注入计划作为权威状态与不可信文本，审批解释和自动审批读取同一个当前步骤。前端沿用历史 `SessionPlan` 的目标、当前步骤和进度展示，默认收起，展开内容在文档流内；Agent 停止时显示暂停并停止动画。启动迁移把现有 `agent_task_files` 的标题、说明和完成进度转为顺序计划，完成项保留在前，未完成项按原 ID 顺序推进，随后移除旧存储表和工具开关。SSH 后台任务机制独立，不受影响。

## Audit storage

`ssh_history` 始终限制在当前会话，可按主机、Tool、状态和 RFC3339 开始时间过滤，并通过稳定游标分页。文本检索默认使用字面量，也支持经过 POSIX 编译校验的 `regex`，并可通过 `query_scope=all|request|output` 限定匹配范围。搜索只返回运行摘要；指定 `run_id` 返回有界的 Tool 参数、规范化请求和脱敏输出，`run_id + query` 返回有界匹配片段，`limit` 在该模式下限制每个输出流的匹配数。

每个 MCP `tools/call` 在执行前写入 `mcp_client_sessions` 与 `mcp_tool_calls`，参数经过统一脱敏并限制大小；完成时记录状态及关联的 Run、Approval、Task、Shell 或 Tunnel ID，同时写入 `mcp_tool_call_started/completed/failed/interrupted` 审计事件。高频 stdout、stderr 和传输进度只作为实时事件发送，不逐块写入 MCP 活动表。服务重启时仍处于 running 的调用明确转为 interrupted。

`runs.request_json`、stdout 和 stderr 的可检索字段均为脱敏视图；对应原文采用 AES-256-GCM 写入 cipher 字段。MCP/Eino 历史工具永远不会返回 cipher 或解密内容。只有本地审批和显式 `audit show --raw` 会解密。

每次运行还会产生独立事件：`command_started`、`approval_requested`、`approval_granted/rejected`、`command_completed/denied`、`task_cancelled` 等。

## Server observability

服务端日志与执行审计是两条独立链路：Audit 是 SQLite 中不可替代的安全证据，Server Logs 用于排查控制面运行状态。应用统一调用标准库 `log/slog`，初始化时通过 MultiHandler 分发到终端、JSONL 轮转文件和进程内环形缓冲区。成功的普通 GET、HEAD 和 OPTIONS 不写访问日志；超过 2 秒的只读请求记录为 Warn，写请求记录为 Info，4xx/5xx 分别记录为 Warn/Error。Web 通过单一应用 WebSocket 订阅连接、审批、会话、活动 Tool 状态、MCP 活动、健康和日志主题。Approval、Audit、Session 与 Chat state 在订阅时发送一次快照，之后由 Store 的成功写入或对话队列状态变更触发失效快照；Agent Runtime 直接写 Store 的路径也使用同一通知链路。连接生命周期发送 delta，流量每 5 秒采样；Agent 活动按当前 session ID 按需订阅，MCP 活动使用 REST 快照恢复并直接推送增量事件。日志当前首次发送筛选后的快照，随后每秒检查新增条目；重连或事件溢出时恢复权威快照。终端仍使用独立 WebSocket，避免 PTY 流量阻塞控制面状态。

HTTP Middleware 始终为请求生成 `request_id` 并通过 context 传递给 Agent、Approval 与 SSH 层；需要记录访问事件时附带 method、path、status、耗时、响应字节和来源 IP，因此一次请求的跨层事件可以关联检索。模型输入、reasoning token、HTTP body、命令参数、脚本和远端输出均不进入服务日志，只记录长度、计数、ID 与最终状态。统一脱敏 Handler 会处理结构化敏感字段，并清理消息、错误文本和嵌套对象中的 Authorization、Bearer/Basic、密码、Token、API Key、私钥与常见云凭据格式。Debug 日志默认启用，可通过配置或 `OPSNERVA_LOG_LEVEL=info` 降低详细程度。

Web 导出接口返回诊断 ZIP：`diagnostics.json` 仅包含版本、Go/OS/架构、启动时间、非敏感日志配置、Agent/模型状态及资源数量；其余条目为当前 JSONL 文件和轮转备份，文件日志关闭时回退为内存日志 JSONL。归档阶段会再次解析并脱敏已有结构化日志，避免旧文件中的常见凭据格式直接进入诊断包。诊断包不包含系统 Prompt、主机地址、Workspace 路径、数据库、审计原文或浏览器控制台日志。

## Conversation persistence

每个新对话由后端生成 session ID，用户消息、最终 Assistant 文本和带 `tool_name` 的脱敏工具结果写入 `chat_messages`；每个用户回合使用独立 Eino checkpoint ID，审批记录持久化 checkpoint 与 interrupt 的精确关联。运行中的会话接受最多 20 条内存消息，并区分 `followup` 与 `steering`：followup 保持 FIFO，在当前 Runner 轮次完整结束后消费；steering 保持自身顺序并排在未开始的 followup 前，通过 Eino `WithCancel` 在 ChatModel 或当前一组 Tool Calls 完成后的首个安全点收束当前轮次，再立即开始新输入。审批暂停期间队列保留但不前进。steering 不复用停止接口，不直接取消正在执行的 Tool、清空队列或拒绝审批；被引导的用户轮次以完成态保留为下一轮上下文。连续轮次通过 `turn_done` 或 `turn_steered` 保持 SSE，仅在队列耗尽时发送最终 `done`。停止、失败或进程退出会丢弃未消费队列。用户图片保存到 `chat_attachments`，普通历史 API 只返回元数据，鉴权附件接口返回原始内容，删除消息时通过外键级联删除。聊天上传使用 multipart，允许格式取自 `system_settings.chat_image_allowed_types_json`；不设置图片张数、大小或图片上下文预算。选入模型上下文的 turn 会把全部图片编码为 Eino `UserInputMultiContent`，再由 OpenAI-compatible adapter 生成 `image_url` data URL。Web 恢复历史时重建工具结果卡片和图片缩略图。下一轮模型输入按用户消息划分完整 turn；历史工具结果不会伪造成缺少 ToolCall ID 的协议级 Tool Message，而是作为明确标记、仍按不可信数据处理的 Assistant 历史证据。失败或中断 turn 只要已经执行过工具也会恢复，没有任何活动的失败 turn 则排除。查询最多读取最近 500 条模型相关记录，再按最近完整 turn、单条工具结果、单个 turn 和 256 KiB 文本总字节预算逐层裁剪；reasoning 不回放。每轮只记录消息数、图片数、图片字节数、工具证据数、文本字节数和截断状态，不记录上下文正文。会话索引按最后事件时间排序，标题取第一条用户消息，纯图片会话使用 `Image`；删除会话会在同一 SQLite 事务中删除消息、附件和审批引用的 checkpoint，执行证据仍保留在独立的 runs 与 audit_events 中。

Runner 在调用工具前通过 Go context 绑定当前 session ID，Service 创建 Run 时只从可信 context 读取该值，模型工具参数不能伪造会话归属。异步 Task 会把该值复制到脱离 HTTP 请求生命周期的后台 context。Audit 的运行记录按 `runs.session_id` 分组，MCP Run 标记为 MCP Server 调用；MCP 活动视图按独立客户端会话展示调用过程。CLI、HTTP 直调和升级前的历史记录显示在 Direct / Legacy 分组。

### Audit history pagination

审计 Web 界面使用两级分页接口：`GET /api/v1/audit/groups` 默认每页 20 个会话分组，`GET /api/v1/audit/runs?session_id=...` 默认每页 50 条组内命令，`limit` 范围为 1–200。组内查询必须显式提供 `session_id`；空值仅代表 Direct / Legacy，不能表示全部。分组直接关联聊天会话标题或 MCP 客户端名称，不依赖聊天侧栏的最近 50 个会话；已删除会话的执行记录仍可分页读取。

首次读取返回 `snapshot_at` 时间上界，后续外层和组内请求沿用此值及相同的 `q`、`host_id`、`started_after`、`started_before`。起止时间是包含边界的 RFC3339 时间，服务端统一按 UTC 比较并拒绝反向范围。`q` 是对请求文本、命令参数和脚本的字面子串搜索，两级查询使用相同匹配范围；不搜索或读取 stdout/stderr 正文。分组的数量、待审批数和最近运行时间均针对这个搜索、主机与时间范围。分组按最近匹配运行时间及 session ID 降序，命令按开始时间及 run ID 降序。存在下一页时返回 `next_cursor: {started_at,id}`；调用方将其作为 `cursor_started_at`、`cursor_id`，连同快照和筛选条件传回。Direct 分组的 cursor ID 可以为空，命令 cursor ID 不可为空。

这个上界用于隔开新运行和历史翻页，不是跨请求冻结的数据库快照：状态更新、删除和回填到上界之前的记录仍然可见，前端必须处理重新校准与过期请求。时间排序在审计查询键中补齐 RFC3339Nano 小数精度，不改写历史时间戳。`idx_runs_audit_session_time_id` 支持组内范围查询，`idx_runs_audit_time_session_id` 与 `idx_runs_audit_host_time_session_id` 分别支持时间、主机加时间的分组范围查询；旧数据库打开时只补建索引。分组计数仍需聚合符合范围的记录，不引入汇总表或全文索引。现有 `/api/v1/run-summaries`、Agent/MCP 历史查询及其可信 context 会话隔离不变；新的审计 Service 查询同样尊重 context 会话约束。

组内深页分别查询“相同时间且 ID 更小”和“更早时间”两个不重叠索引范围，各自最多读取 `limit+1` 条，合并排序最多 `2*(limit+1)` 条。不会因大量记录时间相同而扫描同一时间戳下的全部前序记录；游标已验证不晚于时间上界，因此这两个范围不再重复添加更宽的上界条件。

分页状态层由 `AuditHistoryPage` 管理完整筛选条件、连续的已加载时间范围、固定上界、请求取消及原子提交；`AuditHistoryStore` 管理分组与各会话的独立订阅、展开选择、筛选切换和审计事件批处理。任何筛选变化都会取消旧请求并清除其快照、边界和游标；重新校准会读到原先的最早游标，不按旧页数截断。只刷新已展开会话的命令，隐藏视图停止分页订阅、请求与事件定时器。250 ms 定时器仅合并已收到的事件，不用于轮询。

`useAuditHistory` 在 App 生命周期内持有状态，不订阅分页更新；`AuditRunsView` 订阅会话列表和展开选择，`AuditHistoryGroupCard` 分别订阅组内记录。底部“加载更早会话”和组内“加载更早记录”使用独立游标；搜索经 250 ms 防抖，主机和本地起止时间在有效时直接转换为 UTC，全部由服务端查询而不是筛选已加载记录。完整筛选结果不超过 20 条运行时默认展开，手动收起的选择在筛选、刷新、删除和切页期间保留，加载更多不强制展开。保留原有 details 结构与收缩样式，完整运行详情仍按需读取。

并发翻页共享组内补载；切换筛选、隐藏或删除使旧补载代次失效。分页错误保留失败操作，重试旧页时继续使用原游标，而不是重读首页。`AuditRunDetailResource` 仅在首次展开详情后创建；摘要变化时失效缓存，收起记录、收起会话、隐藏或卸载时取消详情读取，迟到响应不得覆盖新结果。详情错误只允许显式重试，不自动循环请求。

旧的单层审计 Hook、前端分组/过滤、最近 50 个会话标题拼接和跨页面 `runs` 属性传递已移除。聊天工具卡片直接使用持久化工具结果的 `_display` 请求信息、生命周期状态及输出；截断结果仍通过聊天消息详情接口按需补全，不依赖用户是否打开过审计页。MCP 活动视图及 Agent/MCP 历史查询链路不变。

删除成功时，HTTP 结果携带 `audit_event_id`，对应同一事务发布的 WebSocket 审计事件 `id`。客户端以此去重这两条到达顺序不定的通知，并立即使受影响的旧读取失效；快照中的历史删除事件仅触发重新校准，不再次执行删除过滤。删除范围及活动记录保留规则不变。

会话标题写入及会话删除成功后也发布审计视图失效通知，以更新关联标题和会话存在状态；这些通知不写入 `audit_events`，不会将用户改名操作计入审计历史。四阶段验收范围与性能数据见 [审计历史验收记录](audit-history-validation.md)。

## Model provider routing

模型提供商按 kind 映射为 Eino ChatModel：Anthropic 走 Claude 组件（原生 Anthropic API，`x-api-key` 认证），其余（OpenAI、DeepSeek、Ollama 和自定义兼容端点）走 OpenAI-compatible 组件。每条提供商记录可选配置 User-Agent 改写，对该提供商的聊天、连接测试和模型发现请求统一生效，用于兼容按 UA 过滤请求的网关（SDK 默认 UA 形如 `Anthropic/Go x.y.z`，可能被部分中转站拒绝）。API Key 在进入 SQLite 前使用与审计数据相同的 AES-256-GCM 主密钥加密，对外只返回 `has_api_key`。提供商只保存共享 `proxy_id`；模型发现、配置测试、主 Agent 和选择该记录的 subagent 在每次构建配置时解析同一个显式代理。修改代理会触发 Runtime 重载，代理失败不会静默回退直连。

SQLite 使用部分唯一索引保证最多只有一个 active provider。切换时服务更新 active route，构建新的 ChatModelAgent 与 Runner，再通过互斥锁原子替换运行时指针；已经取得旧 Runner 的请求可以正常结束，新请求使用新配置。没有 active provider 时才回退到 `OPENAI_*` 环境变量。

人工审批说明 Agent 和 Auto Approval Agent 默认各自继承 active provider。前者可通过 `system_settings.subagent_model_provider_id` 指定模型，后者通过 `automatic_approval_settings.model_provider_id` 独立指定；一方的选择不影响另一方。显式选择的 provider 不会静默回退，且在解除引用前禁止删除。两者的业务截止时间共用 `subagent_timeout_seconds`，允许 5–120 秒、默认 30 秒。

模型发现统一请求配置 Base URL 下的 `GET /models`，兼容 OpenAI 标准的 `data[].id`，同时容忍部分实现的 `models[]` 包装。请求最长 15 秒、响应最大 2 MiB，并禁止 HTTP 重定向，避免 Authorization Header 被转发到其他地址；上游错误在返回 Web 前会经过密钥替换和通用脱敏。

所有保存、发现与测试流程复用同一 Base URL 规范化函数：无协议的 loopback、私网 IP、`.local` 与单标签主机补全为 HTTP，公网域名补全为 HTTPS，并移除末尾误填的 `/models` 或 `/chat/completions`。包含凭据、查询参数或 fragment 的 URL 会被拒绝。

模型测试可以使用已保存配置，也可以使用尚未落库的表单配置。后端复用加密 Key 或请求中的临时 Key，通过对应 ChatModel 发送 `Hello`；HTTP 调用成功且 Assistant Content 去除空白后非空才返回 Healthy，空响应与协议错误均视为失败。

## Runtime settings

Web 配置中心把模型提供商、SSH 主机、代理和系统设置收敛到同一入口。`proxies` 保存可复用的名称、规范化 URL、用户名和加密密码；模型、Tavily 和主机只保存 `proxy_id`。被引用的代理禁止删除，HTTPS 代理禁止分配给 SSH 主机。旧的三套内嵌代理字段会在一次性迁移中复制到独立代理记录后直接删除，不保留双轨运行逻辑。`system_settings` 单行表保存完整 System Prompt、Agent 最大模型迭代数、命令解释开关、独立 provider、请求超时、聊天图片格式和 Workspace Shell 模式；每次修改都会写入 `system_settings_updated` 审计事件。未显式保存 Prompt 时读取内置模板，显式空字符串则保持为空。保存后的 Prompt 不拼接内置内容；Runtime 仅附加不可编辑的服务宿主机 `GOOS/GOARCH` 上下文，并明确该平台不代表 SSH 目标机。Runtime 构建新的 ChatModelAgent/Runner 并原子替换指针，因此所有会话的新请求立即使用新的 Prompt、循环预算和解释模型路由，已经取得旧 Runner 的执行不会被中断。
