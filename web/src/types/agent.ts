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

export interface AgentPlanStep {
  number: number
  title: string
  description?: string
  status: 'pending' | 'in_progress' | 'completed' | 'skipped'
  updated_at: string
}

export interface AgentPlan {
  session_id: string
  goal: string
  status: 'active' | 'completed'
  steps: AgentPlanStep[]
  created_at: string
  updated_at: string
}
