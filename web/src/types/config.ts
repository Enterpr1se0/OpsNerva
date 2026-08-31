export type WorkspaceShellMode = 'sandbox' | 'host' | 'disabled'
export type ApprovalMode = 'manual' | 'auto' | 'full_access'

export interface AuthStatus {
	enabled: boolean
	authenticated: boolean
	username?: string
}

export interface ConfigurationImportResult {
	proxies: number
	hosts: number
	model_providers: number
	runtime_reloaded: boolean
}

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

export interface SystemSettings {
  agent_max_iterations: number
  system_prompt: string
  default_system_prompt: string
  approval_mode: ApprovalMode
  approval_explanations_enabled: boolean
  subagent_model_provider_id: string
  automatic_approval_model_provider_id: string
  subagent_timeout_seconds: number
	context_compression_enabled: boolean
	context_compression_threshold_percent: number
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

export interface SystemSettingsInput {
  agent_max_iterations: number
  system_prompt?: string
  approval_mode?: ApprovalMode
  approval_explanations_enabled?: boolean
  subagent_model_provider_id?: string
  automatic_approval_model_provider_id?: string
  subagent_timeout_seconds?: number
	context_compression_enabled?: boolean
	context_compression_threshold_percent?: number
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
  results: Array<{title:string;url:string;content:string;score?:number;published_date?:string;truncated?:boolean;original_bytes?:number;returned_bytes?:number}>
  response_time?: number
  request_id?: string
  credits?: number
  truncated?: boolean
  original_bytes?: number
  returned_bytes?: number
  omitted_results?: number
  content_is_untrusted: boolean
}