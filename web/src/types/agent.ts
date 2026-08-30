import type { ChatQueueMode } from './chat'

export interface AgentEvent {
	event_id?: number
	type: string
	message_id?: string
	user_message_id?: string
	role?: string
	tool_name?: string
	tool_call_id?: string
	content?: string
	segment_id?: string
	session_id?: string
	title?: string
  run_id?: string
  stream?: 'stdout' | 'stderr' | 'progress'
  sequence?: number
	transferred_bytes?: number
	total_bytes?: number
  error?: string
  approval_id?: string
  status?: string
  retry_attempt?: number
  retry_max?: number
	context_tokens?: number
	context_window?: number
	input_tokens?: number
	output_tokens?: number
	total_tokens?: number
	queue_position?: number
	queue_count?: number
	queue_mode?: ChatQueueMode
	attachment_count?: number
}

export interface AgentTask {
  id: string
  subject: string
  description: string
  status: 'pending' | 'in_progress' | 'completed'
  blocks: string[]
  blocked_by: string[]
  active_form?: string
  owner?: string
  metadata?: Record<string, unknown>
  updated_at: string
}

export interface AgentTaskList {
  session_id: string
  items: AgentTask[]
  updated_at: string
}