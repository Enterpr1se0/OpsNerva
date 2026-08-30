export type LLMToolGuard = 'read_only' | 'approval_required' | 'agent_state' | 'audited_control' | 'external_mcp'

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