# Architecture

## Trust boundary

LLM、Prompt、Skill、远程输出和 MCP Client 都不属于可信计算基。唯一能够执行 SSH 的入口是 `service.Service`，它固定执行以下顺序：

1. 从 SQLite 按 `host_id` 解析目标与认证方式，忽略模型提供的任何连接凭据。
2. 规范化并校验请求，绑定原始载荷与连接配置的 SHA-256。
3. 应用审批模式：Manual 交给用户，Auto 将当前用户请求和精确操作交给审批 Agent，Full access 直接执行；审批 Agent 要求人工判断、不可用或返回无效结果时回退用户审批。
4. 仅在实际执行前解密所需 SSH/sudo 密码，获取并发令牌，通过绑定的内置 SSH Transport 执行。
5. 加密原始请求和输出，生成脱敏视图并追加审计事件。

Eino Tool、MCP Tool、HTTP 和 CLI 都是这个 Service 的适配器。模型侧执行结果只保留状态、有效输出和必要标识；预期失败额外返回 `code/message/retryable` 与可用的结构化校验信息。只有上下文取消或内部持久化损坏会成为 ToolNode fatal error。

这里的 MCP Tool 分为两个方向。`opsnerva mcp` 通过 stdio、设置中的 MCP Server Mode 通过同一 HTTP 服务上的 `/mcp`，把受控 SSH Service 暴露给 MCP Client，因此完整复用输入校验、审批模式和审计。只读工具通过 MCP annotations 明确标记；`ssh_shell` 使用独立 MCP surface，`ssh_tunnel` 复用已有转发状态，`ssh_history` 只能读取当前 MCP 会话创建的运行。HTTP 使用有状态 Streamable HTTP，由服务端为每次 initialize 分配高熵 `mcp_sess_*`；stdio 进程拥有一个独立会话。客户端上报的 name/version 仅作展示，不作为身份认证，共享 Bearer Token 仍是 HTTP 访问边界。设置开关按请求即时生效，Token 仅在生成时返回，SQLite 只保存 SHA-256 摘要。管理员配置的外部 MCP Server 则属于独立信任域：它的工具在远端/子进程自身权限下执行，不自动继承 OpsNerva 的审批控制。Web 会明确提示该边界，只有启用状态为 ready 且未被 func 管理单独关闭的外部工具才进入主 Agent，审批 Agent 仍保持无 Tool。

App 控制面通过 loopback HTTP API 连接本地 Sidecar。`auth.password` 非空时，除登录状态、登录和退出接口外，普通 `/api/v1` 端点都要求进程内随机会话 Cookie；Cookie 为 `HttpOnly`、`SameSite=Lax`，TLS 下同时设置 `Secure`，会话只保存令牌 SHA-256 与过期时间，重启后失效。配置密码使用常量时间比较，登录失败按来源地址限速。未配置密码时保持本机无登录模式。MCP HTTP 使用独立 Bearer Token，不接受控制面 Cookie；MCP stdio 与 CLI 仍属于本机进程边界。

模型提供商、SSH 主机和代理使用带版本号的统一 JSON 迁移契约。Store 在只读事务中生成一致快照，Service 通过专用 DTO 明确允许导出的字段，不序列化运行时 Host Key、派生状态或数据库密文。Service 使用本机 Master Key 解密凭据并写入迁移 JSON，不依赖控制面登录，也不要求单独的迁移密码。导入先完整解析并校验 ID、名称、模式、凭据和跨资源引用，再在单个 SQLite 事务中合并；任何错误整体回滚，未包含的现有资源不会删除。目标库用自己的 Master Key 重新加密凭据，因此迁移包不携带、也不依赖源机器 `master.key`。导入和导出只属于 Web 控制面，不暴露给 LLM 或 MCP Tool。

人工审批说明使用原有 `ApprovalAgent`；Auto 决策使用新增的 `AutoApprovalAgent`。两者都是 `MaxIterations=1`、无 Tool 的独立 Eino `ChatModelAgent`，各自拥有 Runner、Prompt、Service 接口、并发槽和可用状态，互不复用。`ApprovalAgent` 仅异步生成人工审批页的操作与风险说明，不参与自动决定。`AutoApprovalAgent` 接收由 Go context 绑定的当前用户请求、精确操作、目标能力、当前任务和请求摘要；Tool reason 与任务不能扩大用户授权。它结构化返回 `allow/reject/manual`。Auto 仅在完整 `allow` 时执行，明确 `reject` 时终止，`manual`、缺少当前用户请求、不可用、超时或格式无效时回退用户审批。

## Packages

- `internal/sshx`：进程内 SSH 认证、严格 host key、SFTP、SOCKS5/HTTP 代理、ProxyJump、输出上限和连接探测。
- `internal/service/websearch.go`：Tavily 请求、HTTP/HTTPS/SOCKS5 代理、凭据解密、响应限额与外部内容脱敏。
- `internal/service`：审批状态机、摘要绑定、执行并发、任务、审计事务，以及外部 MCP Client Session 与动态工具生命周期。
- `internal/store`：SQLite hosts、runs、approvals、events、chat、加密模型/MCP 配置与 Eino checkpoints。
- `internal/agent`：Eino ChatModelAgent、强类型 Tools、消息历史、事件流与并发安全的 Runner 热切换。
- `internal/mcpserver`：官方 MCP Go SDK stdio 与 Streamable HTTP 适配器。
- `internal/httpapi`：本地 HTTP API、SSE、应用状态/交互终端 WebSocket 和嵌入 Go 二进制的 React 静态资源。
- `internal/observability`：`slog` 多路 Handler、字段脱敏、JSONL 文件轮转与 Web 内存日志缓冲。
- `internal/skills`：可上传、永久删除和启停的无权限运维方法论注册表。

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

App 中由用户直接新建的 SSH/Workspace Terminal 与上述 Agent Tool Shell 只共享 PTY、尺寸、序列和实时订阅组件。App Terminal 不创建 Run、Approval、Audit、`ssh_shell_sessions` 或 `ssh_shell_events`，最近 2 MiB 输出仅在进程内用于 WebSocket 重连，关闭或服务重启后即丢弃；用户按键不会形成输入事件，也不经过面向模型的凭据检测、密码提示阻断或输出脱敏。Agent/MCP Shell 继续使用 SQLite 历史、响应游标、凭据隔离和执行审计；用户接管这类 Shell 输入密码时仍使用不保存明文的私密通道。

`web_search` 和 `web_extract` 共用管理员保存在 `web_search_settings` 中的 Tavily 配置，但可由 func 管理分别启停。Tavily 设置只保存共享 `proxy_id`，运行时从 `proxies` 解析 HTTP、HTTPS、SOCKS5 或 SOCKS5H 地址及加密凭据；请求禁用环境代理，选中的代理失败时不会回退直连。查询、域名过滤条件和待提取 URL 会离开本机。管理员结果数是上限，模型省略结果数时默认取 5。搜索支持 topic、depth、相对/绝对日期范围和高级分片；提取一次接受最多五个公开 HTTP/HTTPS URL，并支持 query、depth 和相关分片。URL 输入与提供方返回值均会规范化、去重并拒绝凭据、localhost、私网和链路本地地址。

提供方原始响应限制为 2 MiB，错误正文限制为 4 KiB；模型可见的搜索和提取结果分别限制为约 32 KiB 与 48 KiB，并携带逐项及总量裁剪元数据。Eino Tool Reduction 在 Summarization 之前运行，历史 Web 结果使用结构化 reducer 保留查询、标题、URL、日期、失败项和请求元数据，只压缩正文；持久化事件保持原样。请求最多进行一次临时网络、短期 429 或 5xx 重试，受四路并发上限保护，相同在途请求会合并，但不缓存完成结果。全部外部内容都会执行当前凭据精确脱敏并标记为不可信。审计保存查询或规范化 URL 列表的 SHA256、请求 ID、credits、HTTP 状态、重试与字节统计，不保存正文、凭据或完整 URL。

Sandbox 后端仅在 Linux 使用配置的 Bubblewrap；不存在或 namespace 创建失败时关闭失败，绝不回退到 Host Shell。沙箱新建 user/mount/PID/network namespace、丢弃 capabilities、禁用嵌套 user namespace 和网络，只读挂载 `/usr` 与动态链接库目录，创建独立 `/proc`、`/dev`、`/tmp`，并按 Workspace access 只读或读写挂载到 `/workspace`。预存的 `.env*`、`.ssh`、`.opsnerva-*`、`data`、`master.key` 与 credential 命名路径，以及 socket、FIFO 和 device 等特殊文件，在 mount namespace 内被遮蔽。

Host 后端直接以服务账户执行，拥有宿主机文件系统与网络权限；Unix 选择 Bash，Windows 依次查找 `pwsh.exe` 与 `powershell.exe`。Host 仅允许 `read_write` Workspace，并遵循当前审批模式。两种后端都使用清理后的环境、有界输出、统一脱敏并隐藏 Workspace 宿主根路径。配置中固定的 Workspace validator 仍使用 argv 和固定环境单独执行，不经过 Shell。

目标主机和最多四级跳板链的非秘密连接字段及更新时间组成 `ssh_connection_digest`，与命令一起进入请求摘要；人工批准后修改地址、用户、认证方式、known_hosts、网络代理或跳板链会导致执行失败。主机间文件传输对源端和目标端分别计算并校验该摘要。

内置实现使用 `golang.org/x/crypto/ssh`、`knownhosts` 和 `github.com/pkg/sftp`。密码只作为进程内 AuthMethod；Keyboard Interactive 只回答一次无回显的密码提示。Unix Agent 连接 `SSH_AUTH_SOCK`，Windows Agent 通过 named pipe 连接系统 OpenSSH Agent。Web/CLI 上传的未加密 OpenSSH 格式私钥限制为 1 MiB，使用 AES-256-GCM 写入 `private_key_cipher`，对外只返回 `has_private_key` 并只在内存解析；不接受或保存宿主机私钥路径。主机只保存共享 `proxy_id`，连接时解析 SOCKS5、SOCKS5H 或 HTTP CONNECT 参数；代理密码只在内存解密。ProxyJump 只能引用注册主机，逐跳验证 host key、检测环路并限制最大深度；与网络代理组合时，代理只负责连接第一台跳板机。

内置实现通过未认证握手扫描协商出的 host key，信任时重新扫描并精确比较 SHA256 指纹，再以 `0600` 追加和同步 known_hosts。未知 key 与 key mismatch 均关闭失败。命令和 SFTP 每次建立独立连接，连接/命令取消会关闭完整跳板链；15 秒 keepalive 连续超时会断开。

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

复杂工作直接使用 `github.com/cloudwego/eino/adk/middlewares/plantask`。中间件在 `BeforeAgent` 注入 `TaskCreate`、`TaskGet`、`TaskUpdate` 和 `TaskList`，框架负责字段 schema、任务依赖、环检测、状态更新和全部完成后的清理。

项目实现 `plantask.Backend`，将框架任务文件写入 `agent_task_files`。Backend 只从可信 Go context 读取 session ID，模型参数不能跨会话访问任务。Chat state 返回当前任务列表，Web 展示进度、当前任务和依赖；Runtime 在后续模型请求前注入现存任务状态，并把它作为不可信文本处理。任务状态不扩大权限，所有 SSH Tool 仍独立通过输入校验、审批模式和加密审计。

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

## Model provider routing

模型提供商按 kind 映射为 Eino ChatModel：Anthropic 走 Claude 组件（原生 Anthropic API，`x-api-key` 认证），其余（OpenAI、DeepSeek、Ollama 和自定义兼容端点）走 OpenAI-compatible 组件。每条提供商记录可选配置 User-Agent 改写，对该提供商的聊天、连接测试和模型发现请求统一生效，用于兼容按 UA 过滤请求的网关（SDK 默认 UA 形如 `Anthropic/Go x.y.z`，可能被部分中转站拒绝）。API Key 在进入 SQLite 前使用与审计数据相同的 AES-256-GCM 主密钥加密，对外只返回 `has_api_key`。提供商只保存共享 `proxy_id`；模型发现、配置测试、主 Agent 和选择该记录的 subagent 在每次构建配置时解析同一个显式代理。修改代理会触发 Runtime 重载，代理失败不会静默回退直连。

SQLite 使用部分唯一索引保证最多只有一个 active provider。切换时服务更新 active route，构建新的 ChatModelAgent 与 Runner，再通过互斥锁原子替换运行时指针；已经取得旧 Runner 的请求可以正常结束，新请求使用新配置。没有 active provider 时才回退到 `OPENAI_*` 环境变量。

人工审批说明 Agent 和 Auto Approval Agent 默认各自继承 active provider。前者可通过 `system_settings.subagent_model_provider_id` 指定模型，后者通过 `automatic_approval_settings.model_provider_id` 独立指定；一方的选择不影响另一方。显式选择的 provider 不会静默回退，且在解除引用前禁止删除。两者的业务截止时间共用 `subagent_timeout_seconds`，允许 5–120 秒、默认 30 秒。

模型发现统一请求配置 Base URL 下的 `GET /models`，兼容 OpenAI 标准的 `data[].id`，同时容忍部分实现的 `models[]` 包装。请求最长 15 秒、响应最大 2 MiB，并禁止 HTTP 重定向，避免 Authorization Header 被转发到其他地址；上游错误在返回 Web 前会经过密钥替换和通用脱敏。

所有保存、发现与测试流程复用同一 Base URL 规范化函数：无协议的 loopback、私网 IP、`.local` 与单标签主机补全为 HTTP，公网域名补全为 HTTPS，并移除末尾误填的 `/models` 或 `/chat/completions`。包含凭据、查询参数或 fragment 的 URL 会被拒绝。

模型测试可以使用已保存配置，也可以使用尚未落库的表单配置。后端复用加密 Key 或请求中的临时 Key，通过对应 ChatModel 发送 `Hello`；HTTP 调用成功且 Assistant Content 去除空白后非空才返回 Healthy，空响应与协议错误均视为失败。

## Runtime settings

Web 配置中心把模型提供商、SSH 主机、代理和系统设置收敛到同一入口。`proxies` 保存可复用的名称、规范化 URL、用户名和加密密码；模型、Tavily 和主机只保存 `proxy_id`。被引用的代理禁止删除，HTTPS 代理禁止分配给 SSH 主机。旧的三套内嵌代理字段会在一次性迁移中复制到独立代理记录后直接删除，不保留双轨运行逻辑。`system_settings` 单行表保存完整 System Prompt、Agent 最大模型迭代数、命令解释开关、独立 provider、请求超时、聊天图片格式和 Workspace Shell 模式；每次修改都会写入 `system_settings_updated` 审计事件。未显式保存 Prompt 时读取内置模板，显式空字符串则保持为空。保存后的 Prompt 不拼接内置内容；Runtime 仅附加不可编辑的服务宿主机 `GOOS/GOARCH` 上下文，并明确该平台不代表 SSH 目标机。Runtime 构建新的 ChatModelAgent/Runner 并原子替换指针，因此所有会话的新请求立即使用新的 Prompt、循环预算和解释模型路由，已经取得旧 Runner 的执行不会被中断。
