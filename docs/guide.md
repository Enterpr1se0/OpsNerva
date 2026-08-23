# OpsNerva 使用手册

本文是 OpsNerva 的完整使用与配置手册，覆盖安装、模型与主机配置、Workspace、审批与审计等全部功能。项目概览与快速上手见仓库根目录的 [README](../README.md)，实现边界与安全设计见[架构文档](architecture.md)。

## 功能总览

- 支持多个 OpenAI 兼容模型提供商、共享代理配置、连接测试和运行时切换。
- 内置跨平台 SSH，支持 `ssh-agent`、上传私钥、密码、网络代理、ProxyJump、sudo 和严格 Host Key 校验。
- Agent 可建立本地或反向 SSH 端口转发，Web 全局显示活动链路、连接数和流量。
- Manual、Auto 或 Full access 模式统一决定 Agent 请求的审批路径。
- 会话、工具结果、任务和审批状态持久化，刷新页面不会中断正在运行的 Agent。
- 支持在会话中选择或粘贴图片，并把文字和图片一起发送给支持视觉输入的模型。
- Workspace 支持文件管理、补丁和 Shell；Linux 可使用 Bubblewrap 沙箱。
- 内置 Tavily 网络搜索与网页提取，并支持动态加载 Skill 和 MCP 工具。
- 命令、输出和凭据使用 AES-256-GCM 加密保存，模型只接收脱敏后的历史信息。
- React 前端嵌入 Go 二进制；服务端可用 Docker 部署，Windows 和 Linux 也可打包为 Tauri 桌面 App。

```mermaid
flowchart LR
    UI[React / CLI] --> API[Go API + SSE + WebSocket]
    API --> Eino[Eino ChatModelAgent]
    MCP[MCP Client] --> Tools[Typed SSH Tools]
    Eino --> Tools
    Tools --> Validate[Validation + Binding]
    Validate --> Mode[Manual / Auto / Full access]
    Mode --> Approval[User or Approval Agent]
    Mode --> SSH[Built-in SSH]
    Approval --> SSH
    SSH --> Host[Linux Hosts]
    Tools --> Audit[(Encrypted SQLite Audit)]
```

## 快速开始

### 桌面 App

桌面版适用于 Windows 和 Linux。Tauri 会启动内置 Go sidecar，等待本地服务就绪后再显示主界面；再次启动只会聚焦已有窗口。后端仅监听随机的 `127.0.0.1` 端口。启用 MCP Server Mode 后，关闭窗口会隐藏到托盘，“轻量模式”会销毁窗口和 WebView，sidecar 与 MCP Endpoint 继续运行；通过托盘图标或菜单重新创建窗口，选择“退出”才会结束服务。未启用 MCP Server Mode 时关闭窗口会直接退出。

首次启动会在安装目录创建 `config.yaml`、`data/` 和 `workspace/`，然后直接进入 App。

从源码构建需要 Go 1.26+、Node.js 22.13+ 和 Rust stable。Windows 生成 NSIS 安装包：

```powershell
npm --prefix web install
npm --prefix web run desktop:build
```

Linux 还需要 Tauri 的 WebKitGTK 系统依赖。Ubuntu 22.04 可使用：

```bash
sudo apt-get update
sudo apt-get install -y libwebkit2gtk-4.1-dev libayatana-appindicator3-dev librsvg2-dev libxdo-dev libssl-dev patchelf
npm --prefix web install
npm --prefix web run desktop:build
```

产物分别位于 `web/src-tauri/target/release/bundle/nsis/` 和 `web/src-tauri/target/release/bundle/{appimage,deb}/`。推送 `v*` 标签会构建 Windows x64 与 Linux x64 安装包并自动发布到 GitHub Release；手动运行 `Desktop packages` 工作流也会生成相同安装包（仅上传 workflow artifacts）。

桌面开发模式：

```bash
npm --prefix web run desktop:dev
```

### 服务端 / Docker

Docker 保持独立的 Web 服务部署方式，不包含 Tauri 或 Rust 运行时：

```bash
docker build -t opsnerva .
docker run --rm -e OPSNERVA_LISTEN=0.0.0.0 -p 127.0.0.1:8080:8080 \
  -v opsnerva-data:/app/data \
  -v opsnerva-workspace:/app/workspace \
  opsnerva
```

浏览器打开 [http://127.0.0.1:8080](http://127.0.0.1:8080)。默认未启用控制面登录，不应映射到非本机地址。

### 便携二进制

准备以下环境：

- Git
- Go 1.26+
- Node.js 22.13+（包含 npm）
- 一个支持 Tool Calling 的 OpenAI 兼容模型

Linux / macOS 的快捷构建命令还需要 `make`。内置 SSH 不依赖系统中的 `ssh` 命令。Bubblewrap 仅用于 Linux 上的 Workspace Shell 沙箱，不影响服务启动和 SSH 功能。

### Linux / macOS

```bash
git clone https://github.com/Enterpr1se0/opsnerva.git
cd opsnerva
make build
./bin/opsnerva
```

无参数启动会在可执行文件同目录创建 `config.yaml` 并直接启动 Web 服务，已有配置不会被覆盖。

### Windows PowerShell

Windows 不需要安装 `make`：

```powershell
git clone https://github.com/Enterpr1se0/opsnerva.git
Set-Location opsnerva
npm --prefix web install
npm --prefix web run build
New-Item -ItemType Directory -Force bin | Out-Null
go build -buildvcs=false -trimpath -ldflags="-s -w" -o bin/opsnerva.exe ./cmd/opsnerva
.\bin\opsnerva.exe
```

也可以直接双击 `opsnerva.exe`。首次运行会在 EXE 旁创建 `config.yaml`、启动服务并打开浏览器。构建会把 Web 前端嵌入可执行文件，运行时不需要单独复制 `web/dist`。

### 首次启动

1. 启动 OpsNerva App。
2. 打开 **配置 → 模型提供商**，添加模型的 Base URL、Model ID 和 API Key。
3. 先点击 **测试**，保存后点击 **使用此模型**。
4. 如需管理远程主机，打开 **配置 → SSH 主机** 添加主机，然后扫描并核对 Host Key 指纹。
5. 回到 **Agent**，新建会话即可开始使用。

快捷启动生成的 `config.yaml` 修改后需重启生效。数据、加密主密钥、日志和 SQLite 数据库默认写入安装目录的 `data/`，Workspace 文件写入同目录的 `workspace/`。配置中的相对路径均以 `config.yaml` 所在目录为基准。

自动启动之外仍可显式指定配置：

```bash
cp configs/config.example.yaml bin/config.local.yaml
./bin/opsnerva --config bin/config.local.yaml serve
```

### 修改监听地址

默认监听 `127.0.0.1:8080`。快捷启动可以修改 EXE 同目录的 `config.yaml`，也可以在启动时覆盖：

```bash
OPSNERVA_LISTEN='127.0.0.1:9090' ./bin/opsnerva --config bin/config.local.yaml serve
```

PowerShell 使用：

```powershell
$env:OPSNERVA_LISTEN = '127.0.0.1:9090'
.\bin\opsnerva.exe --config bin/config.local.yaml serve
```

### 控制面登录

登录默认关闭。为 `config.yaml` 添加以下配置并重启服务即可启用：

```yaml
auth:
  username: admin
  password: "replace-with-a-long-password"
  session_ttl_hours: 24
```

也可以使用环境变量，环境变量优先于 YAML：

```bash
export OPSNERVA_AUTH_USERNAME=admin
export OPSNERVA_AUTH_PASSWORD='replace-with-a-long-password'
export OPSNERVA_AUTH_SESSION_TTL_HOURS=24
```

密码至少 8 个字符，会保留在启动配置或进程环境中，不写入数据库、日志或健康信息。登录成功后服务端签发进程内随机会话，浏览器仅保存 `HttpOnly`、`SameSite=Lax` Cookie；重启服务会使已有会话失效。登录不会替代 HTTPS：非本机部署仍应使用 HTTPS 反向代理，不要把 HTTP 控制面直接暴露到局域网或公网。`/mcp` 继续使用 MCP Server Mode 自己的 Bearer Token，不接受控制面 Cookie。

### 配置迁移

配置中心可以一次导入或导出模型提供商、SSH 主机和代理，并保留代理及 ProxyJump 引用。导入采用原子合并：相同 ID 或名称更新，未出现在配置包中的现有条目保留；名称冲突、无效引用或无效凭据会让整包回滚。

- 导出的 JSON 始终包含 API Key、SSH 私钥、SSH/root 密码和代理密码，不依赖控制面登录配置，也不需要单独的迁移密码。
- 目标服务直接导入配置，并使用自己的主密钥重新加密凭据后落库。迁移文件包含明文凭据，应通过可信通道传输并在导入后妥善处理。
- Host Key 信任状态和 Known Hosts 文件内容不会迁移，导入后仍需重新核对指纹。

### 使用环境变量配置模型

也可以跳过 Web 配置，通过环境变量提供默认模型：

```bash
export OPENAI_API_KEY="your-key"
export OPENAI_BASE_URL="https://your-openai-compatible-endpoint/v1"
export OPENAI_MODEL="your-tool-calling-model"
```

Web 保存的 API Key 使用本机主密钥加密，不会在列表、健康检查或审计中回显。配置中心的“代理”分组可以保存多个 HTTP、HTTPS、SOCKS5 或 SOCKS5H 代理，模型提供商只需选择“直连”或一个已保存代理。保存或启用提供商后，新请求会立即使用新配置，无需重启服务。

Base URL 可以填写完整 URL，也可以省略协议，例如 `127.0.0.1:11434/v1` 或 `api.example.com/v1`。本机、私网 IP 和单标签主机自动使用 HTTP，公网域名自动使用 HTTPS；误粘贴以 `/models` 或 `/chat/completions` 结尾的完整接口地址也会自动还原为 Base URL。

## Web 本地开发

```bash
make dev-api
make dev-web
```

在两个终端中分别运行以上命令。Web 开发服务器监听 `0.0.0.0:5173`，访问 [http://127.0.0.1:5173](http://127.0.0.1:5173)，`/api` 请求会自动代理到 `8080` 端口。

## System Prompt

系统设置会展示当前可编辑的 System Prompt，可直接编辑、保存为空或恢复内置模板。用户保存的文本会完整覆盖内置 Prompt，不进行去空白或空值回退；Runtime 会另行附加当前服务宿主机的 `OS/架构`，并明确它只适用于本地服务与 Workspace，不代表 SSH 目标机。保存后 Runtime 原子替换 Agent Runner；已经开始的请求继续执行，所有会话之后发起的请求统一使用新 Prompt。

## Tavily Web

系统设置中的 Tavily Web 可配置 API 地址、API Key、超时和搜索结果上限，并选择“直连”或配置中心已有的代理。管理员的结果数是调用上限；模型省略 `max_results` 时默认返回 5 条。`web_search` 支持主题、检索深度、相对或绝对日期范围和域名过滤；`web_extract` 从最多五个指定公开 URL 提取 Markdown，并可用 `query` 返回相关片段。推荐先搜索 3–5 个来源，再按需提取并引用 URL。高级深度只应在需要更多相关片段时使用，因为会消耗更多 credits。

搜索正文单条最多约 2 KiB，完整搜索结果最多约 32 KiB；提取正文单页最多约 16 KiB，完整提取结果最多约 48 KiB。截断结果会返回原始字节数、实际字节数和省略数量；历史工具结果进入模型前还会由 Eino Reduction 保留来源结构并进一步压缩正文。两个 func 可在 Loaded functions 中分别启停。API Key 与代理密码使用 AES-256-GCM 分别加密保存在 Tavily 设置和共享代理记录中，只对外返回是否已配置。查询和 URL 会发送给 Tavily，返回内容按不可信外部证据交给模型。审计保存查询或 URL 列表的 SHA256、耗时、数量、请求 ID、credits、重试与裁剪统计，不保存正文或凭据。

## 会话与上下文

Agent 页面右侧的 Conversations 会列出最近会话，标题取首条用户消息。刷新页面会恢复上次选择的会话并自动定位到最新消息；向上查看旧内容后，新增内容不会强制抢走滚动位置。可以新建、切换或删除历史会话。连接、审批、会话、活动 Tool 和健康状态通过一个应用 WebSocket 按变化推送，不再由页面短周期轮询；活动 Tool 状态只订阅当前会话，断线后自动重连并重新取得快照。用户消息、最终 Assistant 回复、模型提供商实际返回的 reasoning 和工具结果卡片都保存在 SQLite；reasoning 卡片默认折叠并只显示最新一行，展开后查看该次模型调用的完整思考过程。不支持 reasoning 的模型不会显示伪造内容。reasoning 仅用于界面历史，不会作为新消息重复发送给模型。跨轮模型上下文按完整用户轮次恢复：脱敏工具结果会作为明确标记的不可信历史证据回放，失败或中断轮次只要已经执行过工具也会保留；较长会话按最近完整轮次和 256 KiB 总预算裁剪。执行真实性仍以审计 Run 为权威记录。工具真正开始执行时，当前 SSE 会立即显示带实际参数的“执行中”卡片；待审批、完成或失败都按同一个 Tool Call ID 原位更新，不会等待最终结果后才出现，也不会生成重复卡片。命令类 Tool 卡片会直接展示服务端标准化后的完整 program/argv 或完整 Bash 脚本，以及目标主机、工作目录、环境、提权、状态、退出码和分离的 stdout/stderr；`ssh_exec` 与 `ssh_run_script` 默认返回完整输出，也可用 `max_output_bytes` 配合 `output_view=head|tail|head_tail` 精炼交给模型的每个输出流，结果会明确返回总字节数和省略字节数，前端不会把精炼结果伪装成完整输出。原始 JSON 只作为折叠的排错信息。受控操作的审批不会再堆在独立页面中，而是只在发起它的当前会话上方弹出，并同样直接显示 LLM 请求执行的完整命令或脚本。

聊天框支持多选和粘贴图片，也允许只发送图片。Agent 运行时仍可输入并发送，新消息进入当前会话的有界队列，在当前轮完成后按顺序自动开始下一轮；停止 Agent 会同时清空待处理队列。管理员可在 Agent 设置中选择 PNG、JPEG、WebP 和 GIF 格式；服务端不设置图片张数、单张大小或模型上下文图片预算。图片原始数据随消息保存在 SQLite，历史页面通过鉴权接口读取；被文本上下文规则选中的历史轮次会携带该轮全部图片重新发送给模型。活动模型必须兼容 OpenAI 风格的 `image_url` 内容块，否则提供商会返回不支持多模态的错误。

## Workspace

服务在 `workspace_dir`（默认启动目录下的 `workspace/`）中托管全部 Workspace。首次初始化会创建 `default/read_write`，之后可在系统设置中按名称新增、修改权限或移除；每个 Workspace 固定使用 `<workspace_dir>/<名称>/`，无需填写或查看宿主机绝对路径。在系统设置中删除 Workspace 会先解除登记（Agent 立即失去访问权），再永久删除对应目录及其中全部文件，无法恢复；审计事件会记录目录路径与删除结果。每个 Agent 会话持久化绑定一个 Workspace；对话左侧的选择器负责首次绑定和后续切换，运行中的 Agent 禁止切换。模型没有 Workspace 列表工具，所有 `workspace_*` Tool 都由服务端读取当前会话绑定，Tool schema 不接受 `workspace_id`。文件面板可进入子目录、点击上传或拖入多个不超过 100 MiB 的文件、预览文本，并从文件列表或预览窗口把原文件下载到浏览器；上传和下载显示实时字节进度并可取消。服务端通过操作系统文件事件监听当前打开的目录，再以 SSE 通知 Web 静默刷新，因此 Web 上传、Agent Tool、Workspace Shell 和外部编辑器产生的变化使用同一条刷新链路；页面隐藏时暂停该监听，重新显示时自动同步。Web 删除会直接永久删除宿主机文件或目录，确认后无法恢复。这些操作不会自动改写提示词或触发 LLM。文本预览上限为 1 MiB，二进制文件只显示元数据和 SHA256，但仍可直接下载。Web 上传使用防路径穿越、敏感文件名拒绝、禁止覆盖、同目录临时文件、`fsync` 和原子落盘。

`workspace_shell` 用于解压、构建、测试、打包和交互式调试。`action=run` 执行一次性脚本并一次返回完整输出；`start/input/output/list/interrupt/close` 管理持续 PTY。`input` 先发送输入，`input/output` 再按 `wait_seconds` 延迟 0–600 秒后读取一次输出，未填写时延迟 5 秒；期间产生的输出仍实时推送到 Web，不会提前结束工具等待。Agent 创建的 Workspace Shell 会进入右上角统一 Shell 列表并可直接打开观察；Workspace 文件栏中用户手动新建的终端只保留在当前 Workspace，不重复展示。Web 终端通过同一条 WebSocket 发送输入、尺寸和中断，并以带序列号的二进制帧接收原始 PTY 输出；断线按最后序列恢复。交互 Sandbox 保留专用 PTY 的控制终端，因此 Bash 作业控制及 `vim`、`top` 等全屏程序可用。初次渲染会立即同步实际终端尺寸，xterm 滚动缓冲限制为 10,000 行。Agent Tool 结果仍使用跨输出块清理后的可读文本。系统设置提供 `Sandbox`、`Host Shell`、`Disabled` 三种模式；Linux 默认 Sandbox，Windows 默认 Host Shell，设置变化不会让已审批请求切换执行边界。Sandbox 仅支持 Linux，通过 `workspace_sandbox_path`（默认 `bwrap`，也可用 `OPSNERVA_WORKSPACE_SANDBOX`）启动隔离的 user/mount/PID/network namespace，只挂载只读系统运行目录、独立 `/tmp` 和目标 Workspace，并禁用网络与嵌套 user namespace；缺少 Bubblewrap 或 namespace 权限时直接失败，不会降级执行。Workspace 的 `read_only/read_write` 决定沙箱挂载权限，`.env*`、`.ssh` 和系统隐藏文件等敏感路径会被遮蔽。交互会话持续到主动关闭、进程退出或服务停止，不设置 TTL。

Host Shell 直接拥有当前服务账户可用的宿主机文件系统与网络权限：Unix 使用 Bash，Windows 优先使用 PowerShell 7 (`pwsh.exe`) 并回退 Windows PowerShell。它仅允许 `read_write` Workspace，并遵循当前审批模式。省略 `cwd` 时固定使用 Workspace 根目录；Bash、PowerShell、Python 等子进程统一声明 UTF-8 环境。实际后端、Workspace、脚本、相对工作目录、环境与超时全部进入加密请求摘要；模式或请求内容变化不会修改已经开始的执行。`Disabled` 会在审批前拒绝调用。

Agent 向远端发送 Workspace 文件使用 `workspace_file_upload`；从远端取回 Workspace 使用 `workspace_file_download`。两个方向都绑定读取所得 SHA256、主机和两端路径，并在批准执行时再次校验版本；下载只创建新文件，不覆盖现有 Workspace 文件。`workspace_file_delete` 可删除文件或目录，非空目录必须显式设置 `recursive=true`，Workspace 根目录不可删除。托管根目录仅在服务内部使用，不写入数据库、API、审计或模型上下文。可通过 `workspace_dir` 或 `OPSNERVA_WORKSPACE_DIR` 修改统一根目录。

`workspace_file_read` 和 `ssh_file_read` 默认读取 128 KiB；内容未读完时返回 `file.has_more=true` 与下一页 `file.next_offset`。只有文件大小合理且确实需要完整内容时才设置 `full_content=true`。两者都支持 `tail_lines`。显式设置 `offset_bytes` 时，非负值表示从文件开头计算的零基偏移，负值表示读取末尾对应字节数，例如 `-12000` 读取最后 12,000 字节。设置 `pattern` 会切换为搜索模式，并且必须同时设置 `match_mode`：`literal` 匹配完整字面量，`regex` 使用 POSIX 正则表达式；可选的 `context_lines` 返回上下文，搜索结果不会截断。未找到匹配项是成功结果并返回 `search.found=false`；搜索参数与内容范围参数互斥，不再提供独立的 file search Tool。

SSH 主机间迁移单个普通文件使用 `ssh_file_transfer`。OpsNerva 分别连接源主机和目标主机，通过内置 SFTP 在内存中中继数据，因此两台主机无需彼此可达，也不会调用远端 `scp`。调用前先用 `ssh_file_read(metadata_only=true)` 获取源文件 SHA256。目标不存在时省略 `expected_destination_sha256` 即可创建；目标存在时提供其当前 SHA256，服务只替换该精确版本。两端连接配置、路径、版本和超时进入同一个受控请求。目标端先写同目录临时文件，源 SHA256 匹配后再原子改名；版本冲突、取消或超时不会留下半文件。传输中的字节进度会实时显示在 Tool 卡片。

文件编辑 Tool 只要求模型提交从最新读取结果复制的连续完整行 `old_text` 及其替换内容 `new_text`。Service 规范化 UTF-8 BOM 与 CRLF，要求原文只匹配一次，并生成最小 unified diff 供审批和结果展示；找不到或匹配多处时拒绝写入。UTF-16 文件需先转换为 UTF-8。`ssh_file_edit` 与 `workspace_file_edit` 只修改现有文件，不提供专用的新建文件 Tool。远程事务脚本只在批准后由执行层生成，不进入审批内容。编辑不绑定 SHA256、不保存备份，也不提供自动恢复 Tool。可选的 `validator_id` 只能填写 `validators` 配置中对应 scope 的 ID，不能填写命令行；Agent Tool 描述和当前 Workspace 上下文会列出可用 ID，没有可用 ID 时必须省略。validator 仅对临时文件执行，失败时目标文件不会被修改。执行类 Tool 结果只返回状态、有效输出和必要标识；错误额外返回 `code/message/retryable` 与可用的结构化校验信息，不再重复审计、耗时、风险和通用下一步字段。不存在资源、参数错误、超时和远端失败不会用普通运行错误中断 Eino ToolNode。

## 日志

服务端统一使用标准库 `log/slog`。终端按 `logging.format` 输出，轮转文件始终使用便于检索的 JSONL；Web 的 **Logs** 页面显示当前进程最近的结构化日志，支持级别、组件、关键字筛选、WebSocket 增量更新和诊断包导出。切换筛选条件或断线重连时先取得快照，之后只传输新增条目。默认采集 `debug` 及以上级别；生产环境不需要详细生命周期日志时可将级别调为 `info`：

```bash
OPSNERVA_LOG_LEVEL=info ./bin/opsnerva serve
tail -f data/opsnerva.log | jq
```

日志默认保存在 `data/opsnerva.log`，单文件 20 MiB、保留 3 个备份，可通过配置文件的 `logging` 段或 `OPSNERVA_LOG_*` 环境变量调整。Web 诊断包包含 `diagnostics.json`、当前日志和现存轮转备份；未启用文件日志时则包含当前进程的内存日志 JSONL。诊断清单只提供版本、平台、日志配置、Agent 状态以及主机/模型/MCP/Workspace/Skill 数量，不包含系统 Prompt、主机地址、目录路径或凭据。Web 缓冲区不跨重启，轮转文件会保留。成功的普通 GET/HEAD/OPTIONS 不写访问日志，超过 2 秒的只读请求记录为 Warn；写请求记录为 Info，4xx/5xx 分别记录为 Warn/Error。内置日志不记录 HTTP 正文、API Key、SSH/sudo 密码、模型 reasoning 正文、完整参数或 stdout/stderr；结构化敏感字段、消息和错误文本中的常见凭据格式会统一替换为 `[REDACTED]`，导出时还会重新清理已有日志。

## 注册第一个主机

OpsNerva 默认不接受未知 host key。先注册、扫描并人工核对指纹：

```bash
./bin/opsnerva host add \
  --name demo \
  --address 192.0.2.10 \
  --port 22 \
  --user ops

./bin/opsnerva host list
./bin/opsnerva host scan-key HOST_ID
./bin/opsnerva host trust HOST_ID SHA256:THE_VERIFIED_FINGERPRINT
./bin/opsnerva host probe HOST_ID
```

主机可选择当前 `ssh-agent`、上传未加密 OpenSSH 格式私钥或账号密码；Windows Agent 使用系统 OpenSSH Agent named pipe。上传私钥限制为 1 MiB，与 SSH、sudo 和代理密码一样使用 AES-256-GCM 加密保存，API 只返回是否已配置，不返回内容或宿主机路径。执行时只在内存中解密和解析，密钥和密码都不会发送给模型。SSH 主机可选择共享代理中的 SOCKS5、SOCKS5H 或 HTTP CONNECT 代理；HTTPS 代理不会出现在 SSH 选择器中，也会被服务端拒绝。ProxyJump 必须引用另一个已注册且已信任 host key 的主机，每一级都会独立认证并校验 host key，最多四级且拒绝环路。两者同时配置时，代理用于连接第一台跳板机。

Eino Agent 的 `ssh_tunnel` 支持 `start`、`list` 和 `stop`。`direction=local`（默认）对应 `-L`：OpsNerva 监听 `local_host:local_port`，经 SSH 转发到主机侧可访问的 `remote_host:remote_port`；`direction=reverse` 对应 `-R`：SSH 服务端监听 `remote_host:remote_port`，经 SSH 回连 OpsNerva 可访问的 `local_host:local_port`。两个地址默认都是 `127.0.0.1`，监听端口可设为 `0` 自动分配。建立链路遵循当前审批模式，并复用主机已有的网络代理、ProxyJump、认证和 Host Key 校验。连接异常中断后，Service 保留同一隧道 ID 和端口，按 1–30 秒指数退避自动重连，并在每次尝试时重新读取当前主机连接配置；手动停止会立即取消等待或正在进行的重连。反向转发及非回环远端监听还受 SSH 服务端的 TCP 转发与 GatewayPorts 策略约束。Web 顶栏显示当前链路、方向、重连次数、连接数、流量和故障，可直接编辑或停止。隧道只存在于当前服务进程中，停止服务或关闭桌面 App 会立即关闭监听和活动连接。

从双后端版本升级时会执行一次破坏性 SSH schema 迁移：旧主机及其关联的运行、审批、任务和文件操作记录会被删除，同时移除 System OpenSSH、`ssh_config` 别名和自由格式 ProxyJump 字段。聊天记录、模型设置和 Workspace 文件不受影响。

主机可选择三种 sudo 策略：禁用、目标机已配置最小权限 `NOPASSWD`，或由 OpsNerva 托管 sudo 密码。LLM 不调用 `sudo`，只在 `ssh_exec`、`ssh_run_script` 或 `ssh_file_edit` 中设置 `elevated: true`。服务端再按主机配置生成 `sudo -n` 或 `sudo -S` 调用，并遵循当前审批模式。

## 审批模式

- `Manual`：主 Agent 和 MCP Client 的执行请求暂停并等待用户允许或拒绝。
- `Auto`：无 Tool 的独立审批 Agent 对照当前用户请求审查精确操作。允许时立即执行，明确拒绝时终止；要求人工判断、缺少用户请求、模型不可用、超时或响应无效时回退人工审批。
- `Full access`：主 Agent 和 MCP Client 的请求直接执行。

三种模式都保留参数校验、主机与 Workspace 授权、SSH 认证与 Host Key、文件版本绑定、凭据脱敏、系统权限和审计。人工审批说明 Agent 与 Auto Approval Agent 分别使用独立 Runner、Prompt、Service 接口、并发槽和模型选择，仅共用超时设置。Auto Approval Agent 只接收由控制面绑定的当前用户请求、标准化操作、目标主机能力、当前任务和请求摘要；没有 Tool，也不调用批准 API。Tool reason 和任务不能扩大用户授权。Auto 审查结果随 Run 持久化。Manual 下开启“人工审批说明”时，原有说明 Agent 在后台生成操作与风险说明，建议不代替用户决定。MCP Client 不提供用户原始请求，因此 Auto 会回退人工审批。

人工审批绑定主机、目录、命令/脚本、参数、环境和文件内容的 SHA-256。审批后任何修改都会使摘要失效。Web 审批框只提供“允许本次”和“拒绝并说明”，不保存会话级授权。主 Agent 的原始 Tool 调用会在 Service 层真正暂停；刷新页面或临时断网不会取消 Agent Loop。批准并执行完成后，真实结果返回同一个 Tool Call。拒绝时填写的内容会作为 `operator_instruction` 返回被暂停的 Tool。

如果主 Agent 已产生 Tool 结果却以空正文结束，Runtime 不会重跑原 Agent Loop。它会把本轮已持久化的脱敏 Tool 结果和最新任务状态交给一个 `MaxIterations=1`、无 Tool、无 checkpoint 的独立总结 Agent，仅补生成最终回复；该路径不能再次执行操作。总结仍失败时会返回明确错误，并保留原 Tool 结果供下一轮继续。

部署、修复、迁移和多组件诊断等复杂工作使用 Eino `plantask` 中间件提供的 `TaskCreate`、`TaskGet`、`TaskUpdate` 和 `TaskList`。任务支持 `pending`、`in_progress`、`completed` 状态、依赖关系、负责人和元数据；状态由 SQLite Backend 按可信会话 context 隔离。Chat state 与 Web 展示当前任务列表，刷新、断网或达到 Agent 迭代上限后可继续；全部任务完成时框架会清空本轮列表。

CLI 审批示例：

```bash
./bin/opsnerva approval list
./bin/opsnerva approval approve APPROVAL_ID --reason "reviewed command"
```

## MCP 使用

`opsnerva mcp` 使用官方 MCP Go SDK 启动 stdio Server。以支持 MCP 的客户端为例：

```json
{
  "mcpServers": {
    "opsnerva": {
      "command": "/absolute/path/to/bin/opsnerva",
      "args": ["--config", "/absolute/path/to/bin/config.yaml", "mcp"]
    }
  }
}
```

MCP 与 Eino 复用同一个 Service、审批模式和 Audit Store；不存在权限更宽的旁路。每个 stdio 进程或 HTTP initialize 都会获得独立会话，`ssh_history` 只能读取该会话创建的运行。

也可以在 **配置 / 系统设置 / MCP Server Mode** 启动 Streamable HTTP 服务。启动后复制界面显示的 Endpoint 和访问令牌；令牌只在启动或重新生成时显示，服务端仅保存 SHA-256 摘要。其他 Agent 的 MCP 配置示例：

```json
{
  "mcpServers": {
    "opsnerva": {
      "url": "http://HOST:8080/mcp",
      "headers": {
        "Authorization": "Bearer opsnerva_mcp_REPLACE_ME"
      }
    }
  }
}
```

停止 MCP Server Mode 后 `/mcp` 立即不可用；重新启动或重新生成令牌会使旧令牌失效。远程使用时应通过 HTTPS 反向代理暴露该 Endpoint。其他 Agent 的工具调用可在 **审计 / MCP 活动** 实时查看；页面按客户端会话分组，并可展开关联的运行审计。

Web 的 **Extensions / MCP Servers** 还支持反向角色：让 OpsNerva 作为 MCP Client 连接外部工具服务。支持两种标准传输：

- `stdio`：使用 command + 独立 args 数组直接启动子进程，不经过宿主 Shell；可配置绝对工作目录和加密环境变量。
- `streamable_http`：连接绝对 HTTP(S) MCP endpoint，支持加密 HTTP Header 和 OAuth 2.1。

需要 OAuth 的服务在连接失败后点击“授权”。OpsNerva 使用授权服务器发现、动态客户端注册和 PKCE，系统浏览器回调成功后自动连接；客户端凭据、访问令牌和刷新令牌使用 AES-256-GCM 保存，过期访问令牌自动刷新。重新授权会替换旧会话，修改 Endpoint 会清除原会话。

保存后的服务器可以 Test、Retry、Enable、Disable、Edit 或永久 Delete。启用时 OpsNerva 连接服务器、分页发现 `tools/list`，再以 `mcp__<server-hash>__<tool>` 名称注入主 Eino Agent；禁用会关闭 MCP Session，旧 Tool 句柄立即失效，并热重建模型函数列表。环境变量和 Header 同样加密，Web 只显示键名，不回显值。当前仅导入 MCP Tools，不导入 Resources、Prompts 或 Sampling。

外部 MCP Tool 拥有对应 MCP Server 自身的执行权限，不会自动经过 OpsNerva 的审批模式。因此只应启用管理员信任的服务器；这与 OpsNerva 自己作为 MCP Server 时复用受控 SSH Service 的安全语义不同。

主要工具：

- `ssh_host_list` / `ssh_host_inspect`
- `ssh_exec` / `ssh_run_script`（可选 `background: true` 启动后台任务，默认同步执行）
- `ssh_tunnel`（`action=start|list|stop`；`direction=local|reverse`；监听 IP 可配置）
- `ssh_shell`（`action=start|input|output|list|interrupt|close`；MCP 终端与 Web、会话终端隔离）
- `ssh_task`（`action=status|cancel`；status 可用 `wait_seconds`、`block_until=terminal|output` 阻塞等待，并用输出字节偏移只取增量；单次等待截止返回 `wait_deadline_reached=true`，不会改变任务状态。后台命令未填写 `timeout_seconds` 时使用 `max_timeout_seconds`）
- `ssh_file_read`（可选 `metadata_only=true` 或 `pattern` 搜索模式）/ `ssh_file_list`
- `ssh_file_edit` / `ssh_file_transfer`
- `workspace_file_list` / `workspace_file_read`（可选 `tail_lines` 或 `pattern` 搜索模式）/ `workspace_file_edit` / `workspace_file_delete` / `workspace_file_upload` / `workspace_file_download` / `workspace_shell`。这些工具只在 Eino Agent 中提供，Workspace 由 Web 会话绑定，模型不能列出或自行选择其他 Workspace；无会话语义的 MCP Server 不暴露这组工具。
- `ssh_history` 可按请求/输出文本、主机、工具、状态和 RFC3339 时间范围组合检索，并支持 `literal` / POSIX `regex` 和游标分页。检索返回结构化摘要；指定 `run_id` 后返回有界的结构化请求和脱敏输出，也可同时传 `query` 获取匹配片段，此时 `limit` 限制每个输出流的匹配数。主 Agent 与 MCP Client 都只能读取各自可信会话中的运行；通用 `skill` 不通过自身 MCP Server 暴露。

## 数据安全

- `data/master.key` 首次运行生成，权限为 `0600`；生产演示可通过 `OPSNERVA_MASTER_KEY` 注入 Base64 编码的 32 字节密钥。
- Web 模型提供商的 API Key 同样采用 AES-256-GCM 加密保存，HTTP API 只暴露是否已配置密钥。
- 主机 SSH/sudo 密码采用 AES-256-GCM 加密保存；HTTP 和 LLM 工具只暴露 `has_password`、`has_sudo_password` 能力标记。
- 原始请求和 stdout/stderr 加密保存；数据库只额外保存脱敏视图用于检索和模型上下文。
- App API 默认只监听 `127.0.0.1:8080`。控制面登录为可选配置；即使启用，也应通过 HTTPS 反向代理访问非本机部署。
- 远程输出被标记为不可信数据，不能改变系统提示词、输入校验或审批结果。
- 密码认证仍保持非交互、超时和单次提示限制；优先推荐 SSH 证书/密钥与最小化 `sudo -n`，托管密码用于兼容无法立即改造的目标机。

更详细的实现边界见 [架构文档](architecture.md)，可复现演示见 [演示脚本](demo.md)。

## 常用命令

```bash
make test       # Go 单元测试
make test-web   # TypeScript + Vite 构建
make build      # 构建 Web 与单二进制后端
make check      # 测试并构建全部组件

./bin/opsnerva chat
./bin/opsnerva exec --host HOST_ID --program uname --arg -a --reason diagnosis
./bin/opsnerva audit search "systemctl"
./bin/opsnerva audit show RUN_ID
./bin/opsnerva audit show RUN_ID --raw

```

## 当前边界

当前是本地单用户 App，不包含多租户 RBAC、Vault/SSH CA 或 Kubernetes 原生 API。长任务与有限输出保存在 SQLite；重启时无法重新附着到旧 SSH 进程的任务会明确标记为 `interrupted`，而不是消失。

## License

MIT
