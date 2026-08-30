import type { SSHShell, SSHShellUsage } from './ssh'

export interface Approval {
  id: string
  run_id: string
  session_id?: string
  host_id: string
  request_json: string
  request_digest: string
  status: string
  reason?: string
  ai_review?: CommandReview
  created_at: string
}

export interface ApprovalExecutionResult {
  run_id: string
  status: string
	auto_approved?: boolean
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
  status: string
  exit_code: number
  stdout_redacted?: string
  stderr_redacted?: string
  error?: string
  ai_review?: CommandReview
  started_at: string
  completed_at?: string
}

export interface CommandExplanation {
  summary: string
  mechanism: string
  risks: string[]
}

export interface CommandReview {
  status: 'pending' | 'completed' | 'degraded' | 'unavailable'
	kind?: 'automatic_approval'
  model?: string
  decision?: 'allow' | 'reject' | 'manual'
  reason?: string
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

export interface RunSearchPage {
  runs: Run[]
  has_more: boolean
  scan_limited?: boolean
  next_started_at?: string
  next_id?: string
}

export interface AuditRunDeleteResult {
  deleted: number
  retained: number
}

export interface RunDetail {
  run: Run
  stdout_raw?: string
  stderr_raw?: string
}
