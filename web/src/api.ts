import type { AgentEvent, Approval, ApprovalExecutionResult, ChatSession, ChatState, Health, Host, HostInput, LLMToolCatalog, ManagedSkill, MCPOAuthStart, MCPServer, MCPServerInput, MCPTestResult, ModelCatalog, ModelDiscoveryInput, ModelProvider, ModelProviderInput, ModelTestInput, ModelTestJob, ModelTestResult, Proxy, ProxyInput, ProxyTestResult, Run, ServerLogResponse, SFTPFileList, SFTPMutationResult, SSHShell, SSHShellList, SSHShellSnapshot, SSHShellStartInput, SSHTunnel, SSHTunnelList, SSHTunnelStartInput, SystemSettings, SystemSettingsInput, ToolCapabilities, WebSearchResponse, WebSearchSettings, WebSearchSettingsInput, WorkspaceCapability, WorkspaceDeleteResult, WorkspaceFileList, WorkspaceFilePreview, WorkspaceInput, WorkspaceUploadResult } from './types'

async function request<T>(path: string, init?: RequestInit): Promise<T> {
	const multipart=typeof FormData!=='undefined'&&init?.body instanceof FormData
	const headers:Record<string,string> = { ...(multipart?{}:{'Content-Type':'application/json'}), ...(init?.headers as Record<string,string> || {}) }
  const response = await fetch(path, {
    ...init,
	credentials:'same-origin',
	headers,
  })
  if (!response.ok) {
		throw await responseError(response)
  }
  if (response.status === 204) return undefined as T
  return response.json()
}

async function requestList<T>(path: string): Promise<T[]> {
  const value = await request<T[] | null>(path)
  return Array.isArray(value) ? value : []
}

async function waitForModelTest(job:ModelTestJob):Promise<ModelTestResult>{
	let current=job
	while(current.status==='running'){
		await new Promise(resolve=>window.setTimeout(resolve,500))
		current=await request<ModelTestJob>(`/api/v1/model-tests/${encodeURIComponent(current.id)}`)
	}
	if(current.status==='failed')throw new Error(current.error||'Model test failed')
	if(!current.result)throw new Error('Model test returned no result')
	return current.result
}

async function startModelTest(path:string,body:string):Promise<ModelTestResult>{
	const job=await request<ModelTestJob>(path,{method:'POST',body})
	return waitForModelTest(job)
}

export const api = {
  health: () => request<Health>('/api/v1/health'),
	systemSettings: () => request<SystemSettings>('/api/v1/settings'),
	capabilities: () => request<ToolCapabilities>('/api/v1/capabilities'),
	llmTools: () => request<LLMToolCatalog>('/api/v1/agent/tools'),
	setLLMToolEnabled: (name:string,enabled:boolean) => request<LLMToolCatalog>(`/api/v1/agent/tools/${encodeURIComponent(name)}/${enabled?'enable':'disable'}`,{method:'POST',body:'{}'}),
	skills: () => requestList<ManagedSkill>('/api/v1/skills'),
	reloadSkills: () => request<LLMToolCatalog>('/api/v1/skills/reload',{method:'POST',body:'{}'}),
	skill: (name:string) => request<ManagedSkill>(`/api/v1/skills/${encodeURIComponent(name)}`),
	uploadSkill: (name:string,file:File) => {const body=new FormData();body.set('name',name);body.set('file',file);return request<ManagedSkill[]>('/api/v1/skills',{method:'POST',body})},
	saveSkill: (name:string,content:string) => request<ManagedSkill>(`/api/v1/skills/${encodeURIComponent(name)}`,{method:'PUT',body:JSON.stringify({content})}),
	deleteSkill: (name:string) => request<void>(`/api/v1/skills/${encodeURIComponent(name)}`,{method:'DELETE'}),
	setSkillEnabled: (name:string,enabled:boolean) => request<ManagedSkill>(`/api/v1/skills/${encodeURIComponent(name)}/${enabled?'enable':'disable'}`,{method:'POST',body:'{}'}),
	mcpServers: () => requestList<MCPServer>('/api/v1/mcp-servers'),
	saveMCPServer: (server:MCPServerInput) => server.id
		? request<MCPServer>(`/api/v1/mcp-servers/${encodeURIComponent(server.id)}`,{method:'PUT',body:JSON.stringify(server)})
		: request<MCPServer>('/api/v1/mcp-servers',{method:'POST',body:JSON.stringify(server)}),
	deleteMCPServer: (id:string) => request<void>(`/api/v1/mcp-servers/${encodeURIComponent(id)}`,{method:'DELETE'}),
	setMCPServerEnabled: (id:string,enabled:boolean) => request<MCPServer>(`/api/v1/mcp-servers/${encodeURIComponent(id)}/${enabled?'enable':'disable'}`,{method:'POST',body:'{}'}),
	retryMCPServer: (id:string) => request<MCPServer>(`/api/v1/mcp-servers/${encodeURIComponent(id)}/retry`,{method:'POST',body:'{}'}),
	testMCPServer: (id:string) => request<MCPTestResult>(`/api/v1/mcp-servers/${encodeURIComponent(id)}/test`,{method:'POST',body:'{}'}),
	startMCPOAuth: (id:string) => request<MCPOAuthStart>(`/api/v1/mcp-servers/${encodeURIComponent(id)}/oauth`,{method:'POST',body:'{}'}),
	clearMCPOAuth: (id:string) => request<MCPServer>(`/api/v1/mcp-servers/${encodeURIComponent(id)}/oauth`,{method:'DELETE'}),
	createWorkspace: (workspace:WorkspaceInput) => request<WorkspaceCapability>('/api/v1/workspaces',{method:'POST',body:JSON.stringify(workspace)}),
	updateWorkspace: (id:string,workspace:WorkspaceInput) => request<WorkspaceCapability>(`/api/v1/workspaces/${encodeURIComponent(id)}`,{method:'PUT',body:JSON.stringify(workspace)}),
	deleteWorkspace: (id:string) => request<void>(`/api/v1/workspaces/${encodeURIComponent(id)}`,{method:'DELETE'}),
	workspaceFiles: (workspaceId:string,path='.') => request<WorkspaceFileList>(`/api/v1/workspaces/${encodeURIComponent(workspaceId)}/files?path=${encodeURIComponent(path)}`),
	previewWorkspaceFile: (workspaceId:string,path:string) => request<WorkspaceFilePreview>(`/api/v1/workspaces/${encodeURIComponent(workspaceId)}/preview?path=${encodeURIComponent(path)}`),
	saveWorkspaceTextFile: (workspaceId:string,path:string,content:string) => request<WorkspaceUploadResult>(`/api/v1/workspaces/${encodeURIComponent(workspaceId)}/files`,{method:'PUT',body:JSON.stringify({path,content})}),
	uploadWorkspaceFile: (workspaceId:string,file:File,path:string) => {const query=new URLSearchParams({path,filename:file.name});return request<WorkspaceUploadResult>(`/api/v1/workspaces/${encodeURIComponent(workspaceId)}/files?${query}`,{method:'POST',body:file,headers:{'Content-Type':file.type||'application/octet-stream'}})},
	deleteWorkspaceEntry: (workspaceId:string,path:string) => request<WorkspaceDeleteResult>(`/api/v1/workspaces/${encodeURIComponent(workspaceId)}/files?path=${encodeURIComponent(path)}`,{method:'DELETE'}),
  saveSystemSettings: (settings: SystemSettingsInput) => request<SystemSettings>('/api/v1/settings', { method: 'PUT', body: JSON.stringify(settings) }),
  webSearchSettings: () => request<WebSearchSettings>('/api/v1/web-search/settings'),
  saveWebSearchSettings: (settings: WebSearchSettingsInput) => request<WebSearchSettings>('/api/v1/web-search/settings', { method: 'PUT', body: JSON.stringify(settings) }),
  testWebSearch: (query='Tavily Search API') => request<WebSearchResponse>('/api/v1/web-search/test', { method: 'POST', body: JSON.stringify({query}) }),
	proxies: () => requestList<Proxy>('/api/v1/proxies'),
	saveProxy: (proxy:ProxyInput) => request<Proxy>('/api/v1/proxies',{method:'POST',body:JSON.stringify(proxy)}),
	deleteProxy: (id:string) => request<void>(`/api/v1/proxies/${encodeURIComponent(id)}`,{method:'DELETE'}),
	testProxy: (id:string) => request<ProxyTestResult>(`/api/v1/proxies/${encodeURIComponent(id)}/test`,{method:'POST',body:'{}'}),
  modelProviders: () => requestList<ModelProvider>('/api/v1/model-providers'),
  discoverModels: (input: ModelDiscoveryInput) => request<ModelCatalog>('/api/v1/model-providers/discover', { method: 'POST', body: JSON.stringify(input) }),
  testModelConfiguration: (input: ModelTestInput) => startModelTest('/api/v1/model-providers/test', JSON.stringify(input)),
  saveModelProvider: (provider: ModelProviderInput) => request<ModelProvider>('/api/v1/model-providers', { method: 'POST', body: JSON.stringify(provider) }),
  activateModelProvider: (id: string) => request<ModelProvider>(`/api/v1/model-providers/${id}/activate`, { method: 'POST', body: '{}' }),
  deleteModelProvider: (id: string) => request<void>(`/api/v1/model-providers/${id}`, { method: 'DELETE' }),
  testModelProvider: (id: string) => startModelTest(`/api/v1/model-providers/${id}/test`, '{}'),
  hosts: () => requestList<Host>('/api/v1/hosts'),
  sshTunnels: () => request<SSHTunnelList>('/api/v1/ssh-tunnels'),
  startSSHTunnel: (input:SSHTunnelStartInput) => request<SSHTunnel>('/api/v1/ssh-tunnels', { method:'POST', body:JSON.stringify(input) }),
  stopSSHTunnel: (id:string) => request<SSHTunnel>(`/api/v1/ssh-tunnels/${encodeURIComponent(id)}`, { method:'DELETE' }),
  sshShells: (sessionId='') => request<SSHShellList>(`/api/v1/ssh-shells?session_id=${encodeURIComponent(sessionId)}`),
  startSSHShell: (input:SSHShellStartInput) => request<SSHShell>('/api/v1/ssh-shells', { method:'POST', body:JSON.stringify(input) }),
  sshShell: (id:string,after=0,coalesce=false) => request<SSHShellSnapshot>(`/api/v1/ssh-shells/${encodeURIComponent(id)}?after=${after}&coalesce=${coalesce}`),
  sshShellInput: (id:string,input:string,sensitive=false,submit=false,reason='') => request<void>(`/api/v1/ssh-shells/${encodeURIComponent(id)}/input`, { method:'POST', body:JSON.stringify({input,sensitive,submit,reason}) }),
  resizeSSHShell: (id:string,cols:number,rows:number) => request<SSHShell>(`/api/v1/ssh-shells/${encodeURIComponent(id)}/resize`, { method:'POST', body:JSON.stringify({cols,rows}) }),
  interruptSSHShell: (id:string) => request<SSHShell>(`/api/v1/ssh-shells/${encodeURIComponent(id)}/interrupt`, { method:'POST', body:'{}' }),
  closeSSHShell: (id:string) => request<SSHShell>(`/api/v1/ssh-shells/${encodeURIComponent(id)}`, { method:'DELETE' }),
  sftpEntries: (hostId:string,path='') => request<SFTPFileList>(`/api/v1/hosts/${encodeURIComponent(hostId)}/sftp/entries?path=${encodeURIComponent(path)}`),
  sftpFile: async(hostId:string,path:string) => {
		const response=await fetch(sftpDownloadURL(hostId,path),{credentials:'same-origin',headers:{Accept:'application/octet-stream'}})
		if(!response.ok){const body=await response.json().catch(()=>({error:response.statusText}));throw new Error(body.error||response.statusText)}
		return response.arrayBuffer()
	},
  uploadSFTPFile: (hostId:string,path:string,file:File,overwrite=false) => request<SFTPMutationResult>(`/api/v1/hosts/${encodeURIComponent(hostId)}/sftp/files?path=${encodeURIComponent(path)}&overwrite=${overwrite}`, { method:'PUT', body:file, headers:{'Content-Type':file.type||'application/octet-stream'} }),
  uploadSFTPTextFile: (hostId:string,path:string,content:string,encoding:'utf-8'|'utf-16le'|'utf-16be'|'gb18030') => request<SFTPMutationResult>(`/api/v1/hosts/${encodeURIComponent(hostId)}/sftp/files?path=${encodeURIComponent(path)}&overwrite=true&encoding=${encodeURIComponent(encoding)}`, { method:'PUT', body:content, headers:{'Content-Type':'text/plain;charset=utf-8'} }),
  createSFTPDirectory: (hostId:string,path:string) => request<SFTPMutationResult>(`/api/v1/hosts/${encodeURIComponent(hostId)}/sftp/directories`, { method:'POST', body:JSON.stringify({path}) }),
  renameSFTPEntry: (hostId:string,sourcePath:string,destinationPath:string) => request<SFTPMutationResult>(`/api/v1/hosts/${encodeURIComponent(hostId)}/sftp/entries`, { method:'PATCH', body:JSON.stringify({source_path:sourcePath,destination_path:destinationPath}) }),
  deleteSFTPEntry: (hostId:string,path:string,recursive=false) => request<SFTPMutationResult>(`/api/v1/hosts/${encodeURIComponent(hostId)}/sftp/entries?path=${encodeURIComponent(path)}&recursive=${recursive}`, { method:'DELETE' }),
  saveHost: (host: HostInput) => request<Host>('/api/v1/hosts', { method: 'POST', body: JSON.stringify(host) }),
  deleteHost: (id: string) => request<void>(`/api/v1/hosts/${id}`, { method: 'DELETE' }),
	  scanKey: (id: string) => request<{ fingerprint: string; algorithm?: string; trusted: boolean }>(`/api/v1/hosts/${id}/scan-key`, { method: 'POST', body: '{}' }),
	  trustKey: (id: string, fingerprint: string) => request<{ fingerprint: string; algorithm?: string; trusted: boolean }>(`/api/v1/hosts/${id}/trust-key`, { method: 'POST', body: JSON.stringify({ fingerprint }) }),
  probe: (id: string) => request<Record<string, string>>(`/api/v1/hosts/${id}/probe`, { method: 'POST', body: '{}' }),
  approvals: () => requestList<Approval>('/api/v1/approvals?status=pending&limit=100'),
  retryApprovalExplanation: (id: string) => request<Approval>(`/api/v1/approvals/${id}/explanation/retry`, { method: 'POST', body: '{}' }),
  approve: (id: string, reason: string) => request<ApprovalExecutionResult>(`/api/v1/approvals/${id}/approve`, { method: 'POST', body: JSON.stringify({ reason }) }),
  reject: (id: string, reason: string) => request(`/api/v1/approvals/${id}/reject`, { method: 'POST', body: JSON.stringify({ reason }) }),
  runs: (query = '') => requestList<Run>(`/api/v1/runs?limit=100&q=${encodeURIComponent(query)}`),
  logs: (filters: {level?:string;component?:string;q?:string;limit?:number} = {}) => {
    const params=new URLSearchParams()
    if(filters.level)params.set('level',filters.level)
    if(filters.component)params.set('component',filters.component)
    if(filters.q)params.set('q',filters.q)
    params.set('limit',String(filters.limit||500))
    return request<ServerLogResponse>(`/api/v1/logs?${params}`)
  },
  chatSessions: () => requestList<ChatSession>('/api/v1/chat/sessions?limit=50'),
  chatState: (id: string) => request<ChatState>(`/api/v1/chat/${encodeURIComponent(id)}/state`),
	setChatSessionWorkspace: (id:string,workspaceId:string) => request<ChatSession>(`/api/v1/chat/${encodeURIComponent(id)}/workspace`, { method:'PUT', body:JSON.stringify({workspace_id:workspaceId}) }),
	renameChatSession: (id:string,title:string) => request<ChatSession>(`/api/v1/chat/${encodeURIComponent(id)}/title`, { method:'PUT', body:JSON.stringify({title}) }),
	cancelChatSession: (id: string) => request<{cancelled:boolean;cancelled_tools:number;rejected_approvals:number}>(`/api/v1/chat/${encodeURIComponent(id)}/cancel`, { method: 'POST', body: '{}' }),
  deleteChatSession: (id: string) => request<void>(`/api/v1/chat/${encodeURIComponent(id)}`, { method: 'DELETE' }),
}

export function chatAttachmentURL(sessionId:string,attachmentId:string){
	return `/api/v1/chat/${encodeURIComponent(sessionId)}/attachments/${encodeURIComponent(attachmentId)}`
}

export function workspaceFileEventsURL(workspaceId:string,path:string){
	return `/api/v1/workspaces/${encodeURIComponent(workspaceId)}/events?path=${encodeURIComponent(path)}`
}

export function workspaceDownloadURL(workspaceId:string,path:string){
	return `/api/v1/workspaces/${encodeURIComponent(workspaceId)}/download?path=${encodeURIComponent(path)}`
}

export function sshShellEventsURL(shellId:string,after=0){
	return `/api/v1/ssh-shells/${encodeURIComponent(shellId)}/events?after=${after}`
}

export function sftpDownloadURL(hostId:string,path:string){
	return `/api/v1/hosts/${encodeURIComponent(hostId)}/sftp/files?path=${encodeURIComponent(path)}`
}

async function responseError(response:Response){
	const body=await response.json().catch(()=>({error:response.statusText}))
	const error=new Error(body.error||response.statusText) as Error&{status?:number}
	error.status=response.status
	return error
}

async function consumeAgentEventStream(response:Response,onEvent:(event:AgentEvent)=>void){
	if(!response.ok||!response.body)throw await responseError(response)
	const reader=response.body.getReader()
	const decoder=new TextDecoder()
	let buffer=''
	let terminalEventReceived=false
	let flushTimer:number|undefined
	let pending:AgentEvent[]=[]

	const flushPending=()=>{
		if(flushTimer!==undefined)window.clearTimeout(flushTimer)
		flushTimer=undefined
		const events=pending
		pending=[]
		for(const event of events)onEvent(event)
	}
	const isContentDelta=(event:AgentEvent)=>
		!!event.content&&(event.type==='reasoning'||event.type==='tool_output'||(event.type==='message'&&event.role!=='tool'))
	const sameContentStream=(left:AgentEvent,right:AgentEvent)=>
		left.type===right.type&&left.role===right.role&&left.tool_name===right.tool_name&&
		left.message_id===right.message_id&&left.tool_call_id===right.tool_call_id&&left.segment_id===right.segment_id&&
		left.session_id===right.session_id&&left.run_id===right.run_id&&left.stream===right.stream&&
		left.status===right.status
	const dispatch=(event:AgentEvent)=>{
		if(event.type==='done'||event.type==='error'||event.type==='model_error'||event.type==='interrupted')terminalEventReceived=true
		if(!isContentDelta(event)){
			flushPending()
			onEvent(event)
			return
		}
		const previous=pending.at(-1)
		if(previous&&sameContentStream(previous,event)){
			previous.content=(previous.content||'')+event.content
			previous.event_id=event.event_id||previous.event_id
		}else pending.push({...event})
		if(flushTimer===undefined)flushTimer=window.setTimeout(flushPending,40)
	}
	const processFrame=(frame:string)=>{
		const data=frame.split('\n').filter(line=>line.startsWith('data:')).map(line=>line.slice(5).replace(/^ /,'')).join('\n')
		if(!data)return
		dispatch(JSON.parse(data) as AgentEvent)
	}

	try{
		while(true){
			const{value,done}=await reader.read()
			if(done)break
			buffer+=decoder.decode(value,{stream:true})
			buffer=buffer.replace(/\r\n/g,'\n')
			let boundary=buffer.indexOf('\n\n')
			while(boundary>=0){processFrame(buffer.slice(0,boundary));buffer=buffer.slice(boundary+2);boundary=buffer.indexOf('\n\n')}
		}
		buffer+=decoder.decode()
	}finally{flushPending()}
	if(buffer.trim())throw new Error('SSE stream ended with an incomplete event')
	if(!terminalEventReceived)throw new Error('SSE stream ended before the Agent sent a terminal event')
}

export async function streamChat(sessionId: string, workspaceId:string, message: string, images:File[], onEvent: (event: AgentEvent) => void, signal?: AbortSignal) {
	const body=new FormData()
	body.set('session_id',sessionId)
	body.set('workspace_id',workspaceId)
	body.set('message',message)
	for(const image of images)body.append('images',image,image.name)
  const response = await fetch('/api/v1/chat', {
    method: 'POST',
	credentials:'same-origin',
	body,
    signal,
  })
	return consumeAgentEventStream(response,onEvent)
}

export async function reconnectChatStream(sessionId:string,after:number,onEvent:(event:AgentEvent)=>void,signal?:AbortSignal){
	const response=await fetch(`/api/v1/chat/${encodeURIComponent(sessionId)}/events?after=${Math.max(0,after)}`,{
		credentials:'same-origin',headers:{Accept:'text/event-stream'},signal,
	})
	return consumeAgentEventStream(response,onEvent)
}
