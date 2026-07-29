export type Risk = 'read_only' | 'change' | 'critical' | 'forbidden'

export type HostAuthType = 'agent' | 'key' | 'password'
export type HostSudoMode = 'none' | 'nopasswd' | 'password'

export interface HostKey {
  fingerprint: string
  algorithm?: string
  trusted: boolean
}

export interface Host {
  id: string
  name: string
  address: string
  port: number
  user: string
  agent_enabled: boolean
  auth_type: HostAuthType
  has_private_key: boolean
  known_hosts_file?: string
  proxy_jump_host_id?: string
  proxy_id?: string
  host_key?: HostKey
  has_password: boolean
  sudo_mode: HostSudoMode
  has_sudo_password: boolean
  created_at: string
  updated_at: string
}

export interface HostInput {
  id?: string
  name: string
  address: string
  port: number
  user: string
  agent_enabled: boolean
  auth_type: HostAuthType
  private_key: string
  known_hosts_file: string
  proxy_jump_host_id: string
  proxy_id: string
  password: string
  sudo_mode: HostSudoMode
  sudo_password: string
}

export interface Approval {
  id: string
  run_id: string
  session_id?: string
  host_id: string
  request_json: string
  request_digest: string
  risk: Risk
  status: string
  reason?: string
  ai_review?: CommandReview
  created_at: string
  expires_at: string
}

export interface ApprovalExecutionResult {
  run_id: string
  status: string
  risk: Risk
  operator_instruction?: string
  exit_code?: number
  stdout?: string
  stderr?: string
  shell?: SSHShell
  shell_usage?: SSHShellUsage
}

export interface Run {
  id: string
  session_id?: string
  host_id: string
  tool_name?: string
  tool_arguments_json?: string
  request_json: string
  risk: Risk
  status: string
  exit_code: number
  stdout_redacted?: string
  stderr_redacted?: string
  error?: string
  ai_review?: CommandReview
  started_at: string
  completed_at?: string
}

export interface SSHTunnel {
  id: string
  host_id: string
  host_name: string
  local_host: string
  local_port: number
  remote_host: string
  remote_port: number
  status: 'running' | 'stopping' | 'stopped' | 'failed'
  proxy_used: boolean
  active_connections: number
  total_connections: number
  bytes_sent: number
  bytes_received: number
  error?: string
  started_at: string
}

export interface SSHTunnelList {
  tunnels: SSHTunnel[]
  count: number
}

export interface SSHTunnelStartInput {
  host_id: string
  remote_host: string
  remote_port: number
  local_port: number
}

export interface SSHShell {
  id: string
  run_id: string
  session_id: string
  surface: 'agent' | 'quick' | 'workspace'
  host_id: string
  host_name: string
  user: string
  elevated: boolean
  cwd?: string
  status: 'starting' | 'running' | 'stopping' | 'completed' | 'closed' | 'interrupted' | 'failed'
  cols: number
  rows: number
  last_sequence: number
  exit_code?: number
  termination_reason?: 'requested_close' | 'service_stopped' | 'remote_exit' | 'remote_signal' | 'connection_lost' | 'start_failed'
  error?: string
  started_at: string
  ended_at?: string
}

export interface SSHShellEvent {
  shell_id: string
  first_sequence?: number
  sequence: number
  stream: 'stdout' | 'stderr' | 'input' | 'control' | 'status'
  source?: 'agent' | 'operator'
  content?: string
  sensitive?: boolean
  input_bytes?: number
  status?: string
  created_at: string
}

export interface SSHShellSnapshot {
  shell: SSHShell
  events: SSHShellEvent[]
  recent_output?: string
  next_sequence: number
}

export interface SSHShellList {
  shells: SSHShell[]
  count: number
}

export interface SSHShellStartInput {
  host_id: string
  surface?: 'quick' | 'workspace'
}

export interface SSHShellUsage {
  input: string
  status: string
  close: string
}

export interface SFTPFileEntry {
  name: string
  path: string
  type: 'file' | 'directory' | 'symlink'
  size?: number
  mode: string
  modified_at: string
}

export interface SFTPFileList {
  host_id: string
  path: string
  entries: SFTPFileEntry[]
}

export interface SFTPMutationResult {
  host_id: string
  entry: SFTPFileEntry
}

export interface CommandExplanation {
  summary: string
  mechanism: string
  risks: string[]
}

export interface CommandReview {
  status: 'pending' | 'completed' | 'degraded' | 'unavailable'
  model?: string
  decision?: 'allow' | 'reject'
  reason?: string
  deterministic_risk: Risk
  explanation?: CommandExplanation
  errors?: string[]
  reviewed_at: string
}

export interface ServerLogEntry {
  time: string
  level: string
  message: string
  component?: string
  fields?: Record<string, unknown>
}

export interface ServerLogResponse {
  entries: ServerLogEntry[]
  components: string[]
  minimum_level: string
  file?: string
}

export interface AgentEvent {
  type: string
  role?: string
  tool_name?: string
  tool_call_id?: string
  content?: string
  segment_id?: string
  session_id?: string
  run_id?: string
  stream?: 'stdout' | 'stderr'
  sequence?: number
  error?: string
  approval_id?: string
  status?: string
  risk?: Risk
  retry_attempt?: number
  retry_max?: number
  retry_delay_ms?: number
}

export interface ChatSession {
  id: string
  title: string
	workspace_id: string
  message_count: number
  updated_at: string
  active: boolean
}

export interface ChatMessage {
	id: string
  role: 'user' | 'assistant' | 'tool' | 'reasoning'
  content: string
  tool_name?: string
  status: 'pending' | 'completed' | 'failed'
	attachments?: ChatAttachment[]
  created_at: string
}

export interface ChatAttachment {
	id: string
	name: string
	mime_type: string
	size_bytes: number
}

export interface AgentPlanStep {
  number: number
  title: string
  status: 'pending' | 'in_progress' | 'completed' | 'blocked'
  evidence?: string
  updated_at: string
}

export interface AgentPlan {
  session_id: string
  goal: string
  status: 'active' | 'completed' | 'blocked'
  steps: AgentPlanStep[]
  created_at: string
  updated_at: string
}

export interface ChatState {
  active: boolean
	workspace_id: string
  messages: ChatMessage[]
  plan?: AgentPlan | null
}

export type ModelProviderKind = 'openai' | 'deepseek' | 'anthropic' | 'openai_compatible' | 'ollama'

export interface Proxy {
	id: string
	name: string
	url: string
	username?: string
	has_password: boolean
	ssh_compatible: boolean
	created_at: string
	updated_at: string
}

export interface ProxyInput {
	id?: string
	name: string
	url: string
	username: string
	password: string
	clear_password?: boolean
}

export interface ProxyTestResult {
	ok: boolean
	status_code?: number
	latency_ms: number
	target: string
}

export interface ModelProvider {
  id: string
  name: string
  kind: ModelProviderKind
  base_url?: string
	model: string
	has_api_key: boolean
	proxy_id?: string
	user_agent?: string
  active: boolean
  created_at: string
  updated_at: string
}

export interface ModelProviderInput {
  id?: string
  name: string
  kind: ModelProviderKind
  base_url: string
	model: string
	api_key: string
	proxy_id: string
	user_agent: string
}

export interface ModelDiscoveryInput {
  id?: string
  kind: ModelProviderKind
	base_url: string
	api_key: string
	proxy_id: string
	user_agent?: string
}

export interface ModelTestInput extends ModelDiscoveryInput {
  model: string
}

export interface ModelCatalog {
  models: string[]
  count: number
}

export interface ModelStatus {
  available: boolean
  approval_agent_available: boolean
  approval_provider_id?: string
  approval_provider_name?: string
  approval_model?: string
  approval_timeout_seconds?: number
  approval_error?: string
  source: 'database' | 'environment' | 'none'
  provider_id?: string
  name?: string
  model?: string
  error?: string
}

export interface ModelTestResult {
  provider_id?: string
  name?: string
  model: string
  response: string
  latency_ms: number
}

export interface Health {
  status: string
  agent_available: boolean
  model: ModelStatus
  time: string
}

export interface SystemSettings {
  agent_max_iterations: number
  system_prompt: string
  default_system_prompt: string
  approval_mode: ApprovalMode
  approval_explanations_enabled: boolean
  subagent_model_provider_id: string
  subagent_timeout_seconds: number
	chat_image_allowed_types: string[]
  workspace_shell_mode: WorkspaceShellMode
  workspace_shell_platform: string
  workspace_shell_backend?: 'sandbox' | 'host'
  workspace_shell_name?: string
  workspace_sandbox_available: boolean
  workspace_host_shell_available: boolean
  mcp_http_enabled: boolean
  mcp_http_token_configured: boolean
  mcp_http_token?: string
  updated_at: string
}

export type WorkspaceShellMode = 'sandbox' | 'host' | 'disabled'
export type ApprovalMode = 'manual' | 'auto' | 'full_access'

export interface SystemSettingsInput {
  agent_max_iterations: number
  system_prompt?: string
  approval_mode?: ApprovalMode
  approval_explanations_enabled?: boolean
  subagent_model_provider_id?: string
  subagent_timeout_seconds?: number
	chat_image_allowed_types?: string[]
  workspace_shell_mode?: WorkspaceShellMode
  mcp_http_enabled?: boolean
  rotate_mcp_http_token?: boolean
}

export interface WebSearchSettings {
  enabled: boolean
  provider: 'tavily'
  base_url: string
  has_api_key: boolean
  proxy_id?: string
  timeout_seconds: number
  max_results: number
  updated_at?: string
}

export interface WebSearchSettingsInput {
  enabled: boolean
  base_url: string
  api_key?: string
  clear_api_key?: boolean
  proxy_id?: string
  timeout_seconds: number
  max_results: number
}

export interface WebSearchResponse {
  ok?: boolean
  query: string
  provider: string
  results: Array<{title:string;url:string;content:string;score?:number;published_date?:string}>
  response_time?: number
  content_is_untrusted: boolean
}

export interface AuthSession {
  authenticated: boolean
  csrf_token: string
  expires_at: string
}

export interface FileMetadata {
  path: string
  size?: number
  mode?: string
  owner?: string
  group?: string
  modified_unix?: number
  sha256?: string
  validator?: string
  validation_ok?: boolean
  sensitive?: boolean
  offset_bytes?: number
  returned_bytes?: number
}

export interface WorkspaceCapability {
  id: string
  access: 'read_only' | 'read_write'
	shell: boolean
  shell_backend?: 'sandbox' | 'host'
  shell_name?: string
  validators?: string[]
}

export interface WorkspaceInput {
  id: string
  access: 'read_only' | 'read_write'
}

export interface WorkspaceUploadResult {
  workspace_id: string
  path: string
  size: number
  sha256: string
}

export interface WorkspaceFileEntry {
  name: string
  type: 'file' | 'directory'
  size?: number
}

export interface WorkspaceFileList {
  workspace_id: string
  path: string
  entries: WorkspaceFileEntry[]
}

export interface WorkspaceFilePreview {
  workspace_id: string
  path: string
  size: number
  sha256: string
  content?: string
  binary?: boolean
}

export interface WorkspaceDeleteResult {
  workspace_id: string
  path: string
  type: 'file' | 'directory'
  size?: number
  sha256?: string
}

export interface ToolCapabilities {
  workspaces: WorkspaceCapability[]
}

export type LLMToolGuard = 'read_only' | 'policy_checked' | 'approval_required' | 'agent_state' | 'audited_control' | 'external_mcp'

export interface LLMToolDescriptor {
  name: string
  description: string
  category: string
  guard: LLMToolGuard
	enabled: boolean
  input_schema: Record<string, unknown>
}

export interface LLMToolCatalog {
  loaded: boolean
  agent: string
  framework: string
  execution_mode: string
  provider_id?: string
  model?: string
  loaded_at?: string
  count: number
	total: number
  tools: LLMToolDescriptor[]
}

export interface ManagedSkill {
  name: string
  summary: string
  enabled: boolean
  content?: string
  content_sha256?: string
  file_count?: number
  size_bytes?: number
  updated_at?: string
}

export type MCPTransport = 'stdio' | 'streamable_http'

export interface MCPTool {
  name: string
  exposed_name: string
  description?: string
}

export interface MCPServer {
  id: string
  name: string
  transport: MCPTransport
  command?: string
  args?: string[]
  cwd?: string
  url?: string
  env_keys?: string[]
  header_keys?: string[]
  enabled: boolean
  status: 'disabled' | 'disconnected' | 'connecting' | 'ready' | 'error'
  last_error?: string
  connected_at?: string
  tool_count: number
  tools?: MCPTool[]
  created_at: string
  updated_at: string
}

export interface MCPServerInput {
  id?: string
  name: string
  transport: MCPTransport
  command: string
  args: string[]
  cwd: string
  url: string
  env?: Record<string,string>
  headers?: Record<string,string>
  enabled: boolean
}

export interface MCPTestResult {
  ok: boolean
  latency_ms: number
  tool_count: number
  tools: MCPTool[]
}
