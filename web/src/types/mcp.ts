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
  oauth_configured: boolean
  oauth_expires_at?: string
  enabled: boolean
  status: 'disabled' | 'disconnected' | 'connecting' | 'ready' | 'error'
  last_error?: string
  connected_at?: string
  tool_count: number
  tools?: MCPTool[]
  created_at: string
  updated_at: string
}

export interface MCPOAuthStart {
  authorization_url: string
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

export type MCPToolCallStatus='running'|'completed'|'failed'|'interrupted'

export interface MCPClientSession {
	id:string
	transport:string
	client_name?:string
	client_version?:string
	protocol_version?:string
	call_count:number
	running_calls:number
	started_at:string
	last_seen_at:string
}

export interface MCPToolCall {
	id:string
	session_id:string
	tool_name:string
	arguments_json:string
	status:MCPToolCallStatus
	run_id?:string
	approval_id?:string
	task_id?:string
	shell_id?:string
	tunnel_id?:string
	operation_status?:string
	error?:string
	started_at:string
	updated_at:string
	completed_at?:string
}

export interface MCPActivitySnapshot {
	sessions:MCPClientSession[]
	calls:MCPToolCall[]
}

export interface MCPActivityEvent {
	sequence:number
	type:'call_started'|'call_finished'|'call_output'|'call_progress'|'operation_status'
	session_id:string
	call_id:string
	session?:MCPClientSession
	call?:MCPToolCall
	run_id?:string
	stream?:string
	content?:string
	status?:string
	transferred_bytes?:number
	total_bytes?:number
}

export interface MCPTestResult {
  ok: boolean
  latency_ms: number
  tool_count: number
  tools: MCPTool[]
}