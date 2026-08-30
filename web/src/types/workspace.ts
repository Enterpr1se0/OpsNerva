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
	truncated?: boolean
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