import type { AgentTaskList } from './agent'

export type ChatQueueMode = 'followup' | 'steering'

export interface ChatTokenUsage {
	input_tokens: number
	output_tokens: number
	total_tokens: number
	cached_tokens?: number
	reasoning_tokens?: number
}

export interface ChatSession {
  id: string
  title: string
	workspace_id: string
	context_tokens: number
	context_window: number
  message_count: number
  updated_at: string
  active: boolean
}

export interface ChatSessionDelta {
	sessions?: ChatSession[]
	removed_ids?: string[]
}

export type ChatToolCallStatus = 'running' | 'approval_required' | 'completed' | 'partial' | 'failed' | 'interrupted' | 'rejected' | 'expired' | 'unknown'

export interface ChatAttachment {
	id: string
	name: string
	mime_type: string
	size_bytes: number
}

export interface ChatMessage {
	id: string
  role: 'user' | 'assistant' | 'assistant_progress' | 'tool' | 'reasoning'
  content: string
	content_truncated?: boolean
	content_chars?: number
  tool_name?: string
	tool_call_id?: string
	run_id?: string
	tool_status?: ChatToolCallStatus
	token_usage?: ChatTokenUsage
  status: 'pending' | 'waiting_for_approval' | 'completed' | 'failed'
	attachments?: ChatAttachment[]
  created_at: string
}

export interface ChatMessagePage {
	messages: ChatMessage[]
	has_more: boolean
	next_created_at?: string
	next_id?: string
}

export interface ChatToolCall {
	session_id: string
	user_message_id: string
	message_id: string
	tool_call_id: string
	run_id?: string
	tool_name: string
	arguments_json: string
	status: ChatToolCallStatus
	result_json: string
	error?: string
	started_at: string
	updated_at: string
	completed_at?: string
}

export interface ChatState {
  active: boolean
	workspace_id: string
	context_tokens: number
	context_window: number
	messages?: ChatMessage[]
	messages_has_more?: boolean
	messages_next_created_at?: string
	messages_next_id?: string
	running_tool_calls: number
  tasks: AgentTaskList
	queued_messages: QueuedChatMessage[]
	context_summary?: ChatContextSummary
}

export interface QueuedChatMessage {
	id: string
	message: string
	mode: ChatQueueMode
	attachments?: ChatAttachment[]
	attachment_count?: number
	created_at: string
}

export interface ChatContextSummary {
	session_id: string
	through_message_id: string
	revision: number
	trigger: 'auto' | 'manual'
	source_tokens: number
	summary_tokens: number
	model?: string
	created_at: string
	updated_at: string
}

export interface ChatContextCompressionResult {
	summary: ChatContextSummary
	before_tokens: number
	after_tokens: number
}