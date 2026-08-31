export type ModelProviderKind = 'openai' | 'deepseek' | 'anthropic' | 'openai_compatible' | 'ollama'
export type ModelReasoningEffort = '' | 'low' | 'medium' | 'high' | 'xhigh' | 'max'

export interface ModelProvider {
  id: string
  name: string
  kind: ModelProviderKind
  base_url?: string
	model: string
	context_window: number
	resolved_context_window?: number
	reasoning_effort?: ModelReasoningEffort
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
	context_window: number | null
	reasoning_effort: ModelReasoningEffort
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
	reasoning_effort: ModelReasoningEffort
}

export interface ModelCatalog {
  models: string[]
	context_windows?: Record<string,number>
	metadata?: Record<string,ModelMetadata>
  count: number
}

export interface ModelMetadata {
	id: string
	name?: string
	family?: string
	context_window?: number
	input_token_limit?: number
	output_token_limit?: number
	attachment: boolean
	reasoning: boolean
	tool_call: boolean
	structured_output: boolean
	temperature: boolean
	knowledge?: string
	release_date?: string
	last_updated?: string
	status?: string
	input_modalities?: string[]
	output_modalities?: string[]
}

export interface ModelStatus {
  available: boolean
  approval_agent_available: boolean
  automatic_approval_agent_available: boolean
  approval_provider_id?: string
  approval_provider_name?: string
  approval_model?: string
  automatic_approval_provider_id?: string
  automatic_approval_provider_name?: string
  automatic_approval_model?: string
  approval_timeout_seconds?: number
  approval_error?: string
  automatic_approval_error?: string
  source: 'database' | 'environment' | 'none'
  provider_id?: string
  name?: string
  model?: string
	context_window: number
  error?: string
}

export interface ModelTestResult {
  provider_id?: string
  name?: string
  model: string
  response: string
  latency_ms: number
}

export interface ModelTestJob {
  id: string
  status: 'running' | 'completed' | 'failed'
  result?: ModelTestResult
  error?: string
  created_at: string
  finished_at?: string
}

export interface Health {
  status: string
  agent_available: boolean
  model: ModelStatus
  time: string
}
