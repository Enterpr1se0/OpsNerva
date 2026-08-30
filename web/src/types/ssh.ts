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
  agent_root_enabled: boolean
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

export interface SSHTunnel {
  id: string
  host_id: string
  host_name: string
  direction: 'local' | 'reverse'
  local_host: string
  local_port: number
  remote_host: string
  remote_port: number
  status: 'running' | 'retrying' | 'stopping' | 'stopped'
  proxy_used: boolean
  active_connections: number
  total_connections: number
  bytes_sent: number
  bytes_received: number
  reconnect_attempt?: number
  error?: string
  started_at: string
}

export interface SSHTunnelList {
  tunnels: SSHTunnel[]
  count: number
}

export interface SSHTunnelStartInput {
  host_id: string
  direction: 'local' | 'reverse'
  local_host: string
  local_port: number
  remote_host: string
  remote_port: number
}

export type SSHTunnelUpdateInput = SSHTunnelStartInput

export interface SSHShell {
  id: string
  run_id: string
  session_id: string
  kind: 'ssh' | 'workspace'
  surface: 'agent' | 'mcp' | 'quick' | 'workspace' | 'workspace_agent' | 'workspace_operator'
  host_id: string
  host_name: string
  workspace_id?: string
  backend?: 'sandbox' | 'host'
  user: string
  elevated: boolean
  cwd?: string
  status: 'starting' | 'running' | 'stopping' | 'completed' | 'closed' | 'interrupted' | 'failed'
  cols: number
  rows: number
  last_sequence: number
  exit_code?: number
  termination_reason?: 'requested_close' | 'service_stopped' | 'remote_exit' | 'remote_signal' | 'connection_lost' | 'process_exit' | 'process_signal' | 'process_lost' | 'start_failed'
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
  workspace_id?: string
}

export interface SSHHostStatus {
  cpu_total: number
  cpu_idle: number
  memory_used_bytes: number
  memory_total_bytes: number
  disk_used_bytes: number
  disk_total_bytes: number
  network_received_bytes: number
  network_sent_bytes: number
  uptime_seconds: number
  sampled_at: string
}

export interface SSHShellStartInput {
  host_id?: string
  workspace_id?: string
  cwd?: string
  surface?: 'quick' | 'workspace'
}

export interface SSHShellUsage {
  input: string
  output: string
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
